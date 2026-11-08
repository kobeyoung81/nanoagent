package agent

import (
	"context"
	"strings"
	"testing"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanosec/internal/provider"
	"gitee.com/kobeyoung81/nanosec/internal/session"
	"gitee.com/kobeyoung81/nanosec/internal/tool"
)

func newSessionPolicyAgent(t *testing.T, tmpDir string, cfg Config, bus *MockBusClient) (*Agent, *session.Store) {
	t.Helper()
	sessStore, err := session.NewStore(tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	if bus == nil {
		bus = &MockBusClient{}
	}
	if cfg.UserID == "" {
		cfg.UserID = "u"
	}
	if cfg.Model == "" {
		cfg.Model = "m"
	}
	if cfg.MaxIterations == 0 {
		cfg.MaxIterations = 5
	}
	cfg.SessionDir = tmpDir
	mockProvider := provider.NewMock()
	mockProvider.ResponseFunc = func(context.Context, provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "ok"}, nil
	}
	ag, err := New(cfg, bus, mockProvider, tool.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	ag.sessions = sessStore
	return ag, sessStore
}

// sendTurn runs one message through the agent and returns the completion the
// bus received, which is where the (possibly versioned) session id travels.
func sendTurn(t *testing.T, ag *Agent, bus *MockBusClient, sessionID, content string) nanoprotocol.OutboundComplete {
	t.Helper()
	var last nanoprotocol.OutboundComplete
	bus.PostCompleteFunc = func(c nanoprotocol.OutboundComplete) error {
		last = c
		return nil
	}
	if err := ag.processMessage(context.Background(), &nanoprotocol.InboundMessage{
		UserID: "u", SessionID: sessionID, Content: content,
	}); err != nil {
		t.Fatalf("processMessage(%q): %v", content, err)
	}
	return last
}

// TestAgent_NoCompactionKeepsOneSession verifies a conversation that never
// compacts stays in one segment: compaction is the only automatic boundary now
// that the row cap and the idle-timeout rule are gone.
func TestAgent_NoCompactionKeepsOneSession(t *testing.T) {
	bus := &MockBusClient{}
	ag, store := newSessionPolicyAgent(t, t.TempDir(), Config{}, bus)

	for _, content := range []string{"hello", "follow up"} {
		if got := sendTurn(t, ag, bus, "web:chat1", content); got.SessionID != "web:chat1" {
			t.Fatalf("turn %q moved session to %q", content, got.SessionID)
		}
	}

	base := store.GetOrCreate("web:chat1")
	if got := len(withoutModeNotes(base.GetMessages())); got != 4 {
		t.Fatalf("expected 4 messages in the single session, got %d", got)
	}
	if base.GetMeta("_archived") != "" {
		t.Error("session must not be archived without a compaction")
	}
	if base.GetMeta(splitPendingMetaKey) != "" {
		t.Error("no split may be pending without a compaction")
	}
}

// TestSummarizer_AutoCompactionMarksSplitPending verifies the automatic
// compaction gate records that the segment is finished. The split itself is
// deferred to the next turn (see the test below) so a turn is never torn.
func TestSummarizer_AutoCompactionMarksSplitPending(t *testing.T) {
	bus := &MockBusClient{}
	ag, store := newSessionPolicyAgent(t, t.TempDir(), Config{}, bus)

	sess := store.GetOrCreate("web:chat1")
	sess.AddMessage(provider.Message{Role: "user", Content: strings.Repeat("context ", 60)})
	sess.AddMessage(provider.Message{Role: "assistant", Content: strings.Repeat("reply ", 60)})

	// A 1 % trigger makes any conversation cross the gate; the budget floor is
	// 1024 tokens, so the threshold is ~40.
	ag.summarizer.maybeSummarize(context.Background(), sess, summaryBudget{
		ContextWindow:   4096,
		MaxOutputTokens: 0,
		TriggerPercent:  1,
		GrowthThreshold: 2,
	}, &messageRef{channel: "web", chatID: "chat1", sessionID: sess.ID})

	if sess.GetSummary() == "" {
		t.Fatal("expected the gate to compact the session")
	}
	if got := sess.GetMeta(splitPendingMetaKey); got != "true" {
		t.Fatalf("split_pending = %q, want the compaction to mark the segment finished", got)
	}
	last := sess.GetMessages()[len(sess.GetMessages())-1]
	if last.Role != "compaction" {
		t.Errorf("expected the compaction divider to close the segment, got role %q", last.Role)
	}
}

// TestAgent_PendingSplitStartsNewSegmentOnNextTurn verifies the deferred split:
// the flag set by a compaction is consumed at the start of the next turn, before
// the user message is recorded, so the turn stays in one segment.
func TestAgent_PendingSplitStartsNewSegmentOnNextTurn(t *testing.T) {
	bus := &MockBusClient{}
	ag, store := newSessionPolicyAgent(t, t.TempDir(), Config{}, bus)

	sendTurn(t, ag, bus, "web:chat1", "first")
	base := store.GetOrCreate("web:chat1")
	base.SetSummary("the conversation so far")
	base.SetMeta(titleMetaKey, "Session title")
	base.SetMeta(titleCountMetaKey, "9")
	// What a compaction leaves behind: a summarized prefix, a prompt-token
	// watermark and the pending marker. None of these may reach the new segment.
	base.SetMeta(compactFromMetaKey, "2")
	base.SetMeta(session.SummaryCountMetaKey, "2")
	base.SetMeta(lastPromptTokensMetaKey, "123456")
	base.SetMeta(splitPendingMetaKey, "true")

	got := sendTurn(t, ag, bus, "web:chat1", "second")

	if got.SessionID != "web:chat1:v1" {
		t.Fatalf("completion session = %q, want web:chat1:v1", got.SessionID)
	}
	if base.GetMeta("_archived") != "true" {
		t.Error("the compacted segment must be archived")
	}
	next := store.GetOrCreate("web:chat1:v1")
	if got := len(withoutModeNotes(next.GetMessages())); got != 2 {
		t.Errorf("expected only the current turn in the new segment, got %d messages", got)
	}
	if msgs := next.GetMessages(); msgs[0].Role != "user" || msgs[0].Content != "second" {
		t.Errorf("the turn's user message must be the first row of the new segment, got %+v", msgs[0])
	}
	if got := next.GetSummary(); got != "the conversation so far" {
		t.Errorf("new segment summary = %q", got)
	}
	if got := next.GetMeta(handoffFromMetaKey); got != "web:chat1" {
		t.Errorf("handoff_from = %q", got)
	}
	if got := next.GetMeta(titleMetaKey); got != "Session title" {
		t.Errorf("title not carried: %q (the sidebar would be renamed)", got)
	}
	if got := next.GetMeta(titleCountMetaKey); got != "9" {
		t.Errorf("title count not carried: %q", got)
	}
	if _, ok := next.GetAllMeta()[splitPendingMetaKey]; ok {
		t.Error("the pending marker must not cross into the new segment")
	}
	if _, ok := next.GetAllMeta()[compactFromMetaKey]; ok {
		t.Error("compact_from must not cross into the new segment")
	}
	if _, ok := next.GetAllMeta()[session.SummaryCountMetaKey]; ok {
		t.Error("the summary cursor must not cross: it would skip the new segment's messages")
	}
	if _, ok := next.GetAllMeta()[lastPromptTokensMetaKey]; ok {
		t.Error("the stale prompt-token count must not cross into the new segment")
	}
}

// TestAgent_NewCommandVersionsInsteadOfClearing verifies /new keeps history: the
// segment is archived and a new version starts with a clean context but the same
// title, and the completion hands the new id to the client.
func TestAgent_NewCommandVersionsInsteadOfClearing(t *testing.T) {
	bus := &MockBusClient{}
	ag, store := newSessionPolicyAgent(t, t.TempDir(), Config{}, bus)

	sendTurn(t, ag, bus, "web:chat1", "hello")
	base := store.GetOrCreate("web:chat1")
	base.SetSummary("something remembered")
	base.SetMeta(titleMetaKey, "Session title")
	base.SetMeta(lastPlanPathMetaKey, "/home/u/plans/x.md")
	base.SetMeta(sessionModeNoteMetaKey, "Mode: plan.")

	got := sendTurn(t, ag, bus, "web:chat1", "/new")

	if got.SessionID != "web:chat1:v1" {
		t.Fatalf("/new completion session = %q, want web:chat1:v1", got.SessionID)
	}
	if got.FullContent != "New session started." {
		t.Errorf("unexpected /new reply: %q", got.FullContent)
	}
	if base.GetMeta("_archived") != "true" {
		t.Error("the previous segment must be archived, not cleared")
	}
	if got := len(withoutModeNotes(base.GetMessages())); got != 2 {
		t.Errorf("history must survive a /new, got %d messages", got)
	}
	next := store.GetOrCreate("web:chat1:v1")
	if next.MessageCount() != 0 {
		t.Errorf("expected an empty new segment, got %d messages", next.MessageCount())
	}
	if next.GetSummary() != "" {
		t.Error("/new must drop the summary: it asks for a clean context")
	}
	if got := next.GetMeta(lastPlanPathMetaKey); got != "" {
		t.Errorf("plan path must not survive /new, got %q", got)
	}
	if got := next.GetMeta(sessionModeNoteMetaKey); got != "" {
		t.Errorf("mode note must not survive /new, got %q", got)
	}
	if got := next.GetMeta(titleMetaKey); got != "Session title" {
		t.Errorf("title not carried: %q", got)
	}
}

// TestAgent_ArchivedSessionRedirects verifies a client still holding an
// archived id continues in the live segment instead of appending to a transcript
// the portal has already collapsed into the previous version.
func TestAgent_ArchivedSessionRedirects(t *testing.T) {
	bus := &MockBusClient{}
	ag, store := newSessionPolicyAgent(t, t.TempDir(), Config{}, bus)

	sendTurn(t, ag, bus, "web:chat1", "first")
	base := store.GetOrCreate("web:chat1")
	if err := store.ArchiveSession(base.ID); err != nil {
		t.Fatalf("archive: %v", err)
	}
	archivedRows := base.MessageCount()

	got := sendTurn(t, ag, bus, "web:chat1", "after the compaction")

	if got.SessionID != "web:chat1:v1" {
		t.Fatalf("completion session = %q, want the redirect to web:chat1:v1", got.SessionID)
	}
	archived := store.GetOrCreate("web:chat1")
	if archived.MessageCount() != archivedRows {
		t.Errorf("archived segment grew from %d to %d messages", archivedRows, archived.MessageCount())
	}
	next := store.GetOrCreate("web:chat1:v1")
	if got := len(withoutModeNotes(next.GetMessages())); got != 2 {
		t.Errorf("expected the turn in the live segment, got %d messages", got)
	}
}
