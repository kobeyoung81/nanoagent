package agent

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"gitee.com/kobeyoung81/nanosec/internal/config"
	"gitee.com/kobeyoung81/nanosec/internal/provider"
	"gitee.com/kobeyoung81/nanosec/internal/session"
	"gitee.com/kobeyoung81/nanosec/internal/tool"
)

func seedConversation(sess *session.Session, n int) {
	for i := 0; i < n; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		sess.AddMessage(provider.Message{Role: role, Content: fmt.Sprintf("m%d", i)})
	}
}

// TestMaybeSummarize_SkipsBelowGrowth verifies that a session which already has
// a summary is not re-summarized when there are no new messages. This is the
// regression that previously rewrote the summary on every iteration and bounced
// the upstream prompt cache.
func TestMaybeSummarize_SkipsBelowGrowth(t *testing.T) {
	mock := provider.NewMock()
	s := newSummarizer(mock, "model", "")

	sess := session.New("test:grow")
	seedConversation(sess, 10)
	sess.SetSummary("old")
	sess.SetMeta(session.SummaryCountMetaKey, "10")

	s.maybeSummarize(context.Background(), sess, summaryBudget{ContextWindow: 8192, MaxOutputTokens: 1024, TriggerPercent: 75, GrowthThreshold: 1}, nil)

	if len(mock.Calls) != 0 {
		t.Fatalf("expected no summarization with no new messages, got %d call(s)", len(mock.Calls))
	}
}

// TestMaybeSummarize_SkipsBelowTokenBudget verifies a session with new messages
// is only re-summarized once the estimated context nears the budget (token-driven
// trigger), preserving prompt caching.
func TestMaybeSummarize_SkipsBelowTokenBudget(t *testing.T) {
	mock := provider.NewMock()
	s := newSummarizer(mock, "model", "")

	sess := session.New("test:budget")
	seedConversation(sess, 20)
	sess.SetSummary("old")
	sess.SetMeta(session.SummaryCountMetaKey, "10")

	s.maybeSummarize(context.Background(), sess, summaryBudget{ContextWindow: 1_000_000, MaxOutputTokens: 1024, TriggerPercent: 75, GrowthThreshold: 1}, nil)

	if len(mock.Calls) != 0 {
		t.Fatalf("expected no summarization below the token budget, got %d call(s)", len(mock.Calls))
	}
}

// TestMaybeSummarize_SkipsBelowGrowthWatermark verifies that even when new
// messages exist past the token budget, a summary is not rewritten until the
// growth watermark is reached (so a long tool loop does not re-summarize on
// every iteration).
func TestMaybeSummarize_SkipsBelowGrowthWatermark(t *testing.T) {
	mock := provider.NewMock()
	s := newSummarizer(mock, "model", "")

	sess := session.New("test:growth")
	seedConversation(sess, 20)
	sess.SetSummary("old")
	sess.SetMeta(session.SummaryCountMetaKey, "10")

	// 10 new messages (< 50) with a tiny budget that would otherwise trigger.
	s.maybeSummarize(context.Background(), sess, summaryBudget{ContextWindow: 4096, TriggerPercent: 1, GrowthThreshold: 50}, nil)

	if len(mock.Calls) != 0 {
		t.Fatalf("expected growth watermark to suppress summarization, got %d call(s)", len(mock.Calls))
	}
}

// TestMaybeSummarize_FlushesMemoryBeforeSummarizing verifies the automatic path
// runs the pre-compaction memory flush (once) before summarizing.
func TestMaybeSummarize_FlushesMemoryBeforeSummarizing(t *testing.T) {
	mock := provider.NewMock()
	mock.Response = &provider.ChatResponse{Content: "fact"}
	s := newSummarizer(mock, "model", "")

	sess := session.New("test:flush")
	seedConversation(sess, 20)

	s.maybeSummarize(context.Background(), sess, summaryBudget{ContextWindow: 4096, TriggerPercent: 1, GrowthThreshold: 1}, nil)

	if !sess.MemoryFlushed {
		t.Fatalf("expected memory flush to run before automatic summarization")
	}
	if len(mock.Calls) < 2 {
		t.Fatalf("expected a flush + summarize call, got %d", len(mock.Calls))
	}
}

// TestMaybeSummarize_IncrementalInput verifies that only messages added since
// the last summary are fed to the summarizer, and that the watermark advances.
func TestMaybeSummarize_IncrementalInput(t *testing.T) {
	mock := provider.NewMock()
	mock.Response = &provider.ChatResponse{Content: "new summary"}
	s := newSummarizer(mock, "model", "")

	sess := session.New("test:incremental")
	seedConversation(sess, 20)
	sess.SetSummary("old summary")
	sess.SetMeta(session.SummaryCountMetaKey, "8")
	sess.MemoryFlushed = true // skip the pre-compaction flush in this test

	// Small budget + low trigger percent so the token-driven gate fires.
	s.maybeSummarize(context.Background(), sess, summaryBudget{ContextWindow: 4096, TriggerPercent: 1, GrowthThreshold: 1}, nil)

	if len(mock.Calls) != 1 {
		t.Fatalf("expected exactly one summarization call, got %d", len(mock.Calls))
	}
	content := mock.Calls[0].Messages[1].Content
	if !strings.Contains(content, "old summary") {
		t.Errorf("summarizer prompt must carry the previous summary preamble")
	}
	if strings.Contains(content, "User: m0\n") {
		t.Errorf("already-summarized messages leaked into the incremental input")
	}
	if !strings.Contains(content, "User: m8") || !strings.Contains(content, "User: m18") {
		t.Errorf("new messages missing from incremental input: %q", content)
	}
	if got := sess.GetSummary(); got != "new summary" {
		t.Errorf("summary = %q, want new summary", got)
	}
	if got := sess.GetMeta(session.SummaryCountMetaKey); got != "20" {
		t.Errorf("summary watermark = %q, want 20", got)
	}
	if mock.Calls[0].SessionID != "test:incremental:summarize" {
		t.Errorf("SessionID = %q, want test:incremental:summarize", mock.Calls[0].SessionID)
	}
}

// TestSummarizeUpTo_Range verifies the forced/compaction path summarizes exactly
// [watermark, upTo) and advances the watermark.
func TestSummarizeUpTo_Range(t *testing.T) {
	mock := provider.NewMock()
	mock.Response = &provider.ChatResponse{Content: "s"}
	s := newSummarizer(mock, "model", "")

	sess := session.New("test:upto")
	seedConversation(sess, 3)

	s.summarizeUpTo(context.Background(), sess, 3)

	if len(mock.Calls) != 1 {
		t.Fatalf("expected one summarization call, got %d", len(mock.Calls))
	}
	if got := sess.GetMeta(session.SummaryCountMetaKey); got != "3" {
		t.Errorf("summary watermark = %q, want 3", got)
	}
}

// TestClearResetsSummaryWatermark ensures /new does not leave a stale count
// that would suppress summarization for the fresh conversation.
func TestClearResetsSummaryWatermark(t *testing.T) {
	sess := session.New("test:clear")
	sess.SetSummary("old")
	sess.SetMeta(session.SummaryCountMetaKey, "172")
	sess.Clear()

	if sess.GetSummary() != "" {
		t.Fatalf("Clear did not reset summary")
	}
	if got := sess.GetMeta(session.SummaryCountMetaKey); got != "" {
		t.Fatalf("summary watermark = %q after Clear, want empty", got)
	}
}

// TestCompactionPlan_ClampsAnUnreachableTrigger pins the guard against a
// trigger the gate can never reach. The gate measures a budget-windowed
// request, which is bounded by the budget itself, so a percentage above 100 %
// quietly disables compaction — the one failure mode a context guard must not
// have. The live platform ran a 1 M-token catalog window with the trigger at
// 85 %, which is where "it only compacts when it hits the ceiling" came from.
func TestCompactionPlan_ClampsAnUnreachableTrigger(t *testing.T) {
	const (
		window = 1_000_000
		out    = 384_000
	)
	budget, threshold := compactionPlan(window, out, 150)
	if budget != 967_232 {
		t.Fatalf("budget = %d, want 967232", budget)
	}
	if threshold != budget {
		t.Fatalf("threshold = %d, want it clamped to the budget (%d)", threshold, budget)
	}
	if _, got := compactionPlan(window, out, 0); got != budget*75/100 {
		t.Fatalf("an unset trigger must fall back to 75%% of the budget, got %d", got)
	}
	if _, got := compactionPlan(window, out, 85); got != budget*85/100 {
		t.Fatalf("a valid trigger must be used as configured, got %d", got)
	}
}

// TestApplySettings_ReportsTriggerClamp pins the other half of the guard: a
// value the code has to override is announced, so an out-of-range setting can
// never look like "auto-compaction just does not fire".
func TestApplySettings_ReportsTriggerClamp(t *testing.T) {
	a, err := New(Config{UserID: "u", Model: "m", SessionDir: t.TempDir()},
		&MockBusClient{}, provider.NewMock(), tool.NewRegistry())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	a.ApplySettings(config.AgentConfig{Agent: config.AgentCoreConfig{SummarizeTokenPercent: 150}})

	if got := a.config.SummarizeTokenPercent; got != 100 {
		t.Fatalf("stored trigger = %d, want it clamped to 100", got)
	}
	if !strings.Contains(buf.String(), "out of range") {
		t.Errorf("the clamp must be reported, log was: %q", buf.String())
	}
}
