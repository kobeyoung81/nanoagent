package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanosec/internal/provider"
	"gitee.com/kobeyoung81/nanosec/internal/session"
	"gitee.com/kobeyoung81/nanosec/internal/tool"
)

// TestCompact_StreamsSummary verifies /compact announces a "started" status,
// streams the summary as summary chunks, persists the compaction marker, and
// completes the turn.
func TestCompact_StreamsSummary(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "compact-stream-test")
	defer os.RemoveAll(tmpDir)
	sessStore, _ := session.NewStore(tmpDir)

	mockProvider := provider.NewMock()
	mockProvider.StreamFunc = func(ctx context.Context, req provider.ChatRequest) (<-chan provider.ChatStreamEvent, error) {
		ch := make(chan provider.ChatStreamEvent, 2)
		ch <- provider.ChatStreamEvent{Delta: "SUM", Kind: nanoprotocol.ChunkKindContent}
		ch <- provider.ChatStreamEvent{Done: true, FullContent: "SUMMARY"}
		close(ch)
		return ch, nil
	}

	var chunks []nanoprotocol.OutboundChunk
	var completed bool
	mockBus := &MockBusClient{
		PostChunkFunc: func(c nanoprotocol.OutboundChunk) error {
			chunks = append(chunks, c)
			return nil
		},
		PostCompleteFunc: func(c nanoprotocol.OutboundComplete) error {
			completed = true
			return nil
		},
	}

	a, _ := New(Config{UserID: "u", Model: "m", MaxIterations: 5, SessionDir: tmpDir}, mockBus, mockProvider, tool.NewRegistry())
	a.sessions = sessStore

	sess := sessStore.GetOrCreate("web:1")
	for i := 0; i < 8; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		sess.AddMessage(provider.Message{Role: role, Content: "m"})
	}
	if err := sessStore.Save(sess); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	msg := &nanoprotocol.InboundMessage{UserID: "u", SessionID: "web:1", Channel: "web", ChatID: "c1", Content: "/compact"}
	if err := a.processMessage(context.Background(), msg); err != nil {
		t.Fatalf("processMessage: %v", err)
	}

	var sawStarted, sawApplied, sawSummary bool
	startedIdx, summaryIdx := -1, -1
	for i, c := range chunks {
		switch c.Kind {
		case nanoprotocol.ChunkKindSummary:
			if c.Content == "SUM" {
				sawSummary = true
				summaryIdx = i
			}
		case nanoprotocol.ChunkKindStatus:
			var p struct{ Event, State string }
			_ = json.Unmarshal([]byte(c.Content), &p)
			if p.Event == "compaction" && p.State == "started" {
				sawStarted = true
				startedIdx = i
			}
			if p.Event == "compaction" && p.State == "applied" {
				sawApplied = true
			}
		}
	}
	if !sawStarted {
		t.Error("missing compaction/started status")
	}
	if !sawApplied {
		t.Error("missing compaction/applied status")
	}
	if !sawSummary {
		t.Error("missing streamed summary chunk")
	}
	if startedIdx < 0 || summaryIdx < 0 || startedIdx > summaryIdx {
		t.Errorf("compaction/started (idx %d) must precede the streamed summary (idx %d)", startedIdx, summaryIdx)
	}
	if !completed {
		t.Error("expected an outbound completion")
	}
	if got := sess.GetSummary(); got != "SUMMARY" {
		t.Errorf("session summary = %q, want SUMMARY", got)
	}
}

// TestCompact_MarksTurnRunning pins the flag a client relies on: the memory
// flush and the summary are long inner LLM calls that emit nothing to the
// client, so the only evidence that /compact is still running is the session's
// turn flag. It used to stay false for the whole command, and a client that
// re-read the session reconciled its busy state away after a few seconds of
// stream silence — the composer went back to idle mid-compaction.
func TestCompact_MarksTurnRunning(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "compact-turn-test")
	defer os.RemoveAll(tmpDir)
	sessStore, _ := session.NewStore(tmpDir)

	sess := sessStore.GetOrCreate("web:1")
	for i := 0; i < 8; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		sess.AddMessage(provider.Message{Role: role, Content: "m"})
	}
	if err := sessStore.Save(sess); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if sess.TurnRunning() {
		t.Fatal("session must not be running before the command")
	}

	// Every LLM call the command makes is one of its silent stretches: the flag
	// and the compaction marker have to be set for all of them.
	var running, compacting []bool
	mockProvider := provider.NewMock()
	mockProvider.StreamFunc = func(ctx context.Context, req provider.ChatRequest) (<-chan provider.ChatStreamEvent, error) {
		running = append(running, sess.TurnRunning())
		compacting = append(compacting, sess.GetMeta(compactingMetaKey) == "true")
		ch := make(chan provider.ChatStreamEvent, 2)
		ch <- provider.ChatStreamEvent{Delta: "SUM", Kind: nanoprotocol.ChunkKindContent}
		ch <- provider.ChatStreamEvent{Done: true, FullContent: "SUMMARY"}
		close(ch)
		return ch, nil
	}
	mockBus := &MockBusClient{
		PostChunkFunc:    func(nanoprotocol.OutboundChunk) error { return nil },
		PostCompleteFunc: func(nanoprotocol.OutboundComplete) error { return nil },
	}

	a, _ := New(Config{UserID: "u", Model: "m", MaxIterations: 5, SessionDir: tmpDir}, mockBus, mockProvider, tool.NewRegistry())
	a.sessions = sessStore

	msg := &nanoprotocol.InboundMessage{UserID: "u", SessionID: "web:1", Channel: "web", ChatID: "c1", Content: "/compact"}
	if err := a.processMessage(context.Background(), msg); err != nil {
		t.Fatalf("processMessage: %v", err)
	}

	if len(running) == 0 {
		t.Fatal("expected at least one LLM call during /compact")
	}
	for i := range running {
		if !running[i] {
			t.Errorf("LLM call %d ran with turn_running=false — a client reads that as an idle turn", i+1)
		}
		if !compacting[i] {
			t.Errorf("LLM call %d ran without the %s marker", i+1, compactingMetaKey)
		}
	}
	if sess.TurnRunning() {
		t.Error("session still marked running after /compact returned")
	}
	if got := sess.GetMeta(compactingMetaKey); got != "" {
		t.Errorf("%s = %q after /compact, want it cleared", compactingMetaKey, got)
	}
}

// TestCompact_FailedSummaryClearsTurnRunning covers the failure path: when the
// summary call fails (/compact ignores the failure and still completes) the
// command must not leave the session marked as running.
func TestCompact_FailedSummaryClearsTurnRunning(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "compact-fail-test")
	defer os.RemoveAll(tmpDir)
	sessStore, _ := session.NewStore(tmpDir)

	sess := sessStore.GetOrCreate("web:1")
	for i := 0; i < 8; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		sess.AddMessage(provider.Message{Role: role, Content: "m"})
	}
	if err := sessStore.Save(sess); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	mockProvider := provider.NewMock()
	mockProvider.StreamFunc = func(ctx context.Context, req provider.ChatRequest) (<-chan provider.ChatStreamEvent, error) {
		return nil, errors.New("provider unavailable")
	}
	var completed bool
	mockBus := &MockBusClient{
		PostChunkFunc:    func(nanoprotocol.OutboundChunk) error { return nil },
		PostCompleteFunc: func(nanoprotocol.OutboundComplete) error { completed = true; return nil },
	}

	a, _ := New(Config{UserID: "u", Model: "m", MaxIterations: 5, SessionDir: tmpDir}, mockBus, mockProvider, tool.NewRegistry())
	a.sessions = sessStore

	msg := &nanoprotocol.InboundMessage{UserID: "u", SessionID: "web:1", Channel: "web", ChatID: "c1", Content: "/compact"}
	if err := a.processMessage(context.Background(), msg); err != nil {
		t.Fatalf("processMessage: %v", err)
	}

	if !completed {
		t.Error("expected a completion even when the summary fails")
	}
	if sess.TurnRunning() {
		t.Error("a failed summary must not leave the turn marked running")
	}
	if got := sess.GetMeta(compactingMetaKey); got != "" {
		t.Errorf("%s = %q after a failed summary, want it cleared", compactingMetaKey, got)
	}
	if got := sess.GetMeta(splitPendingMetaKey); got != "true" {
		t.Errorf("%s = %q, want the compacted segment still queued for the split", splitPendingMetaKey, got)
	}
}

// TestCompact_EventsGoToTheTurnChat pins the routing of a compaction's chunks.
// The agent handles several messages at once (agents.max_concurrent_messages),
// so a command that runs for minutes must post its status and summary chunks to
// the chat that asked for it. It used to read the agent-wide "last message",
// which another conversation can own by the time the summary is out — the
// compaction then appeared in the wrong conversation, or nowhere at all.
func TestCompact_EventsGoToTheTurnChat(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "compact-routing-test")
	defer os.RemoveAll(tmpDir)
	sessStore, _ := session.NewStore(tmpDir)

	var chunks []nanoprotocol.OutboundChunk
	mockBus := &MockBusClient{
		PostChunkFunc: func(c nanoprotocol.OutboundChunk) error {
			chunks = append(chunks, c)
			return nil
		},
		PostCompleteFunc: func(nanoprotocol.OutboundComplete) error { return nil },
	}

	self := &nanoprotocol.InboundMessage{UserID: "u", SessionID: "web:1", Channel: "web", ChatID: "chat-A", Content: "/compact"}
	// A second conversation of the same agent, arriving mid-command.
	other := &nanoprotocol.InboundMessage{UserID: "u", SessionID: "web:2", Channel: "web", ChatID: "chat-B", Content: "unrelated"}

	var a *Agent
	mockProvider := provider.NewMock()
	mockProvider.StreamFunc = func(ctx context.Context, req provider.ChatRequest) (<-chan provider.ChatStreamEvent, error) {
		// The summary call runs while the other conversation lands.
		a.rememberMessage(other)
		ch := make(chan provider.ChatStreamEvent, 2)
		ch <- provider.ChatStreamEvent{Delta: "SUM", Kind: nanoprotocol.ChunkKindContent}
		ch <- provider.ChatStreamEvent{Done: true, FullContent: "SUMMARY"}
		close(ch)
		return ch, nil
	}
	var err error
	a, err = New(Config{UserID: "u", Model: "m", MaxIterations: 5, SessionDir: tmpDir}, mockBus, mockProvider, tool.NewRegistry())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a.sessions = sessStore

	sess := sessStore.GetOrCreate("web:1")
	for i := 0; i < 8; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		sess.AddMessage(provider.Message{Role: role, Content: "m"})
	}
	if err := sessStore.Save(sess); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	if err := a.processMessage(context.Background(), self); err != nil {
		t.Fatalf("processMessage: %v", err)
	}

	if len(chunks) == 0 {
		t.Fatal("expected chunks")
	}
	var sawStarted, sawApplied, sawSummary bool
	for i, c := range chunks {
		if c.Channel != "web" || c.ChatID != "chat-A" || c.SessionID != "web:1" {
			t.Errorf("chunk %d (%s) went to channel=%q chat=%q session=%q, want the /compact message's web/chat-A/web:1",
				i, c.Kind, c.Channel, c.ChatID, c.SessionID)
		}
		switch c.Kind {
		case nanoprotocol.ChunkKindSummary:
			sawSummary = true
		case nanoprotocol.ChunkKindStatus:
			var p struct{ Event, State string }
			_ = json.Unmarshal([]byte(c.Content), &p)
			if p.Event == "compaction" && p.State == "started" {
				sawStarted = true
			}
			if p.Event == "compaction" && p.State == "applied" {
				sawApplied = true
			}
		}
	}
	if !sawStarted || !sawApplied || !sawSummary {
		t.Errorf("expected started+applied status and a summary chunk, got %v", chunks)
	}
}

// TestCompact_CompletionCarriesSummary pins the second delivery path for the
// summary: the completion. Without it a client whose stream lost the summary
// chunks (a dropped socket, a provider that streams no deltas) showed an empty
// divider until the page was reloaded, because the persisted divider only
// reaches the client through a history read.
func TestCompact_CompletionCarriesSummary(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "compact-complete-test")
	defer os.RemoveAll(tmpDir)
	sessStore, _ := session.NewStore(tmpDir)

	var last nanoprotocol.OutboundComplete
	mockBus := &MockBusClient{
		PostChunkFunc:    func(nanoprotocol.OutboundChunk) error { return nil },
		PostCompleteFunc: func(c nanoprotocol.OutboundComplete) error { last = c; return nil },
	}
	mockProvider := provider.NewMock()
	mockProvider.StreamFunc = func(ctx context.Context, req provider.ChatRequest) (<-chan provider.ChatStreamEvent, error) {
		ch := make(chan provider.ChatStreamEvent, 2)
		ch <- provider.ChatStreamEvent{Delta: "SUM", Kind: nanoprotocol.ChunkKindContent}
		ch <- provider.ChatStreamEvent{Done: true, FullContent: "SUMMARY"}
		close(ch)
		return ch, nil
	}

	a, err := New(Config{UserID: "u", Model: "m", MaxIterations: 5, SessionDir: tmpDir}, mockBus, mockProvider, tool.NewRegistry())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a.sessions = sessStore

	sess := sessStore.GetOrCreate("web:1")
	for i := 0; i < 8; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		sess.AddMessage(provider.Message{Role: role, Content: "m"})
	}
	if err := sessStore.Save(sess); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	msg := &nanoprotocol.InboundMessage{UserID: "u", SessionID: "web:1", Channel: "web", ChatID: "c1", Content: "/compact"}
	if err := a.processMessage(context.Background(), msg); err != nil {
		t.Fatalf("processMessage: %v", err)
	}

	if last.Summary == "" {
		t.Error("completion carries no summary — a client that missed the streamed chunks shows nothing")
	}
	if last.Summary != sess.GetSummary() {
		t.Errorf("completion summary = %q, want the session's %q", last.Summary, sess.GetSummary())
	}
}

// TestCompact_NothingNewStillReportsSummary covers the silent /compact: when
// there is nothing new to summarize (typically a manual compaction right after
// an automatic one) the command streams no summary chunk at all, yet it still
// reports a finished compaction. The completion has to carry the summary the
// session holds, or the user sees a divider with no result until a reload.
func TestCompact_NothingNewStillReportsSummary(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "compact-nothing-new-test")
	defer os.RemoveAll(tmpDir)
	sessStore, _ := session.NewStore(tmpDir)

	var chunks []nanoprotocol.OutboundChunk
	var last nanoprotocol.OutboundComplete
	mockBus := &MockBusClient{
		PostChunkFunc:    func(c nanoprotocol.OutboundChunk) error { chunks = append(chunks, c); return nil },
		PostCompleteFunc: func(c nanoprotocol.OutboundComplete) error { last = c; return nil },
	}
	streamCalls := 0
	mockProvider := provider.NewMock()
	mockProvider.StreamFunc = func(ctx context.Context, req provider.ChatRequest) (<-chan provider.ChatStreamEvent, error) {
		// Only the summary call matters: the pre-compaction memory flush is a
		// separate call (its fallback even reuses the ":summarize" id), so match
		// on the summarizer's own system prompt.
		if len(req.Messages) > 0 && req.Messages[0].Role == "system" && req.Messages[0].Content == summarizeSystemPrompt {
			streamCalls++
		}
		ch := make(chan provider.ChatStreamEvent, 2)
		ch <- provider.ChatStreamEvent{Done: true, FullContent: "SHOULD NOT RUN"}
		close(ch)
		return ch, nil
	}

	a, err := New(Config{UserID: "u", Model: "m", MaxIterations: 5, SessionDir: tmpDir}, mockBus, mockProvider, tool.NewRegistry())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a.sessions = sessStore

	sess := sessStore.GetOrCreate("web:1")
	for i := 0; i < 8; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		sess.AddMessage(provider.Message{Role: role, Content: "m"})
	}
	// As if an automatic compaction had just summarized everything.
	sess.SetSummary("EARLIER SUMMARY")
	sess.SetMeta(session.SummaryCountMetaKey, strconv.Itoa(len(sess.ConversationMessages())))
	if err := sessStore.Save(sess); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	msg := &nanoprotocol.InboundMessage{UserID: "u", SessionID: "web:1", Channel: "web", ChatID: "c1", Content: "/compact"}
	if err := a.processMessage(context.Background(), msg); err != nil {
		t.Fatalf("processMessage: %v", err)
	}

	if streamCalls != 0 {
		t.Errorf("summarizer ran %d time(s) with nothing new to summarize", streamCalls)
	}
	for _, c := range chunks {
		if c.Kind == nanoprotocol.ChunkKindSummary {
			t.Errorf("streamed a summary chunk with nothing new to summarize: %q", c.Content)
		}
	}
	if last.Summary != "EARLIER SUMMARY" {
		t.Errorf("completion summary = %q, want the session's EARLIER SUMMARY", last.Summary)
	}
}

// TestAutoCompaction_StreamsSummaryDeltas covers a compaction that runs inside
// a normal turn: it must announce itself, stream the summary as it is produced
// (not as one lump at the end), post everything to the turn's own chat, and
// carry the summary on the turn's completion.
func TestAutoCompaction_StreamsSummaryDeltas(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "auto-compact-stream-test")
	defer os.RemoveAll(tmpDir)
	sessStore, _ := session.NewStore(tmpDir)

	var chunks []nanoprotocol.OutboundChunk
	var last nanoprotocol.OutboundComplete
	mockBus := &MockBusClient{
		PostChunkFunc:    func(c nanoprotocol.OutboundChunk) error { chunks = append(chunks, c); return nil },
		PostCompleteFunc: func(c nanoprotocol.OutboundComplete) error { last = c; return nil },
	}
	var a *Agent
	mockProvider := provider.NewMock()
	mockProvider.StreamFunc = func(ctx context.Context, req provider.ChatRequest) (<-chan provider.ChatStreamEvent, error) {
		// Another conversation lands while this turn is being summarized.
		a.rememberMessage(&nanoprotocol.InboundMessage{UserID: "u", SessionID: "web:2", Channel: "web", ChatID: "chat-B"})
		ch := make(chan provider.ChatStreamEvent, 4)
		ch <- provider.ChatStreamEvent{Delta: "ONE ", Kind: nanoprotocol.ChunkKindContent}
		ch <- provider.ChatStreamEvent{Delta: "TWO ", Kind: nanoprotocol.ChunkKindContent}
		ch <- provider.ChatStreamEvent{Delta: "THREE", Kind: nanoprotocol.ChunkKindContent}
		ch <- provider.ChatStreamEvent{Done: true, FullContent: "SUMMARY"}
		close(ch)
		return ch, nil
	}
	var err error
	a, err = New(Config{
		UserID:                    "u",
		Model:                     "m",
		MaxIterations:             5,
		SessionDir:                tmpDir,
		ContextWindow:             4096,
		SummarizeTokenPercent:     1,
		SummarizeMessageThreshold: 2,
	}, mockBus, mockProvider, tool.NewRegistry())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a.sessions = sessStore

	sess := sessStore.GetOrCreate("web:1")
	sess.AddMessage(provider.Message{Role: "user", Content: strings.Repeat("context ", 60)})
	sess.AddMessage(provider.Message{Role: "assistant", Content: strings.Repeat("reply ", 60)})
	if err := sessStore.Save(sess); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	msg := &nanoprotocol.InboundMessage{UserID: "u", SessionID: "web:1", Channel: "web", ChatID: "chat-A", Content: "hello"}
	if err := a.processMessage(context.Background(), msg); err != nil {
		t.Fatalf("processMessage: %v", err)
	}

	if sess.GetSummary() == "" {
		t.Fatal("expected the gate to compact the session")
	}
	summaryChunks := 0
	var sawStarted bool
	for _, c := range chunks {
		if c.ChatID != "chat-A" || c.SessionID != "web:1" {
			t.Errorf("chunk %s went to chat=%q session=%q, want the turn's chat-A/web:1", c.Kind, c.ChatID, c.SessionID)
		}
		switch c.Kind {
		case nanoprotocol.ChunkKindSummary:
			summaryChunks++
		case nanoprotocol.ChunkKindStatus:
			var p struct{ Event, State string }
			_ = json.Unmarshal([]byte(c.Content), &p)
			if p.Event == "compaction" && p.State == "started" {
				sawStarted = true
			}
		}
	}
	if !sawStarted {
		t.Error("auto-compaction must announce itself so the client opens a divider")
	}
	if summaryChunks < 2 {
		t.Errorf("streamed %d summary chunk(s); the summary must accumulate instead of arriving as one lump", summaryChunks)
	}
	if last.Summary == "" {
		t.Error("completion carries no summary for the compaction that ran inside the turn")
	}
}

// TestContextLengthError_CompactionIsAnnounced covers the overflow path: when
// the provider rejects a request for length, the agent compacts mid-turn. It
// used to do that silently (no status, no summary, no divider), so the user saw
// an unexplained retry and a compaction divider only after a reload.
func TestContextLengthError_CompactionIsAnnounced(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "forced-compact-test")
	defer os.RemoveAll(tmpDir)
	sessStore, _ := session.NewStore(tmpDir)

	var chunks []nanoprotocol.OutboundChunk
	var last nanoprotocol.OutboundComplete
	mockBus := &MockBusClient{
		PostChunkFunc:    func(c nanoprotocol.OutboundChunk) error { chunks = append(chunks, c); return nil },
		PostCompleteFunc: func(c nanoprotocol.OutboundComplete) error { last = c; return nil },
	}
	answerCalls := 0
	mockProvider := provider.NewMock()
	mockProvider.StreamFunc = func(ctx context.Context, req provider.ChatRequest) (<-chan provider.ChatStreamEvent, error) {
		// The two utility calls (memory flush, summarizer) carry their own system
		// prompts; everything else is a request of the turn itself.
		utility := len(req.Messages) > 0 &&
			(req.Messages[0].Content == summarizeSystemPrompt || req.Messages[0].Content == memoryFlushSystemPrompt)
		body := "ANSWER"
		if utility {
			body = "SUMMARY"
		}
		ch := make(chan provider.ChatStreamEvent, 4)
		if !utility {
			answerCalls++
			if answerCalls == 1 {
				ch <- provider.ChatStreamEvent{Err: errors.New("context_length_exceeded: maximum context length is 4096 tokens")}
				close(ch)
				return ch, nil
			}
		}
		ch <- provider.ChatStreamEvent{Delta: body, Kind: nanoprotocol.ChunkKindContent}
		ch <- provider.ChatStreamEvent{Done: true, FullContent: body}
		close(ch)
		return ch, nil
	}

	a, err := New(Config{
		UserID:                "u",
		Model:                 "m",
		MaxIterations:         5,
		SessionDir:            tmpDir,
		ContextWindow:         128000,
		SummarizeTokenPercent: 75,
	}, mockBus, mockProvider, tool.NewRegistry())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a.sessions = sessStore

	sess := sessStore.GetOrCreate("web:1")
	for i := 0; i < 8; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		sess.AddMessage(provider.Message{Role: role, Content: "m"})
	}
	if err := sessStore.Save(sess); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	msg := &nanoprotocol.InboundMessage{UserID: "u", SessionID: "web:1", Channel: "web", ChatID: "chat-A", Content: "hello"}
	if err := a.processMessage(context.Background(), msg); err != nil {
		t.Fatalf("processMessage: %v", err)
	}

	var sawStarted, sawApplied, sawSummary bool
	for _, c := range chunks {
		if c.ChatID != "chat-A" || c.SessionID != "web:1" {
			t.Errorf("chunk %s went to chat=%q session=%q, want the turn's chat-A/web:1", c.Kind, c.ChatID, c.SessionID)
		}
		switch c.Kind {
		case nanoprotocol.ChunkKindSummary:
			sawSummary = true
		case nanoprotocol.ChunkKindStatus:
			var p struct{ Event, State string }
			_ = json.Unmarshal([]byte(c.Content), &p)
			if p.Event == "compaction" && p.State == "started" {
				sawStarted = true
			}
			if p.Event == "compaction" && p.State == "applied" {
				sawApplied = true
			}
		}
	}
	if !sawStarted || !sawApplied {
		t.Errorf("forced compaction must announce itself, got %v", chunks)
	}
	if !sawSummary {
		t.Error("forced compaction must stream its summary")
	}
	if last.Summary == "" {
		t.Error("completion carries no summary for the forced compaction")
	}
	if last.Summary != "SUMMARY" {
		t.Errorf("completion summary = %q, want the forced compaction's SUMMARY", last.Summary)
	}
	if last.FullContent != "ANSWER" {
		t.Errorf("completion content = %q, want the retried answer", last.FullContent)
	}
	if sess.GetSummary() != "SUMMARY" {
		t.Errorf("session summary = %q, want SUMMARY", sess.GetSummary())
	}
	if !sess.MemoryFlushed {
		t.Error("expected the pre-compaction memory flush to have run")
	}
}

// TestAutoCompaction_SetsTheFloorForItsOwnTurn pins that a compaction shrinks
// the request it was triggered for. Compaction used to write only the split
// marker, so the rest of that turn still sent the whole transcript plus the
// freshly written summary — the request stayed exactly the size that tripped
// the gate, and history was only shed at the next turn's segment split (which
// is also why the overflow retry never got smaller).
func TestAutoCompaction_SetsTheFloorForItsOwnTurn(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "auto-compact-floor-test")
	defer os.RemoveAll(tmpDir)
	sessStore, _ := session.NewStore(tmpDir)

	mockProvider := provider.NewMock()
	mockProvider.StreamFunc = func(ctx context.Context, req provider.ChatRequest) (<-chan provider.ChatStreamEvent, error) {
		ch := make(chan provider.ChatStreamEvent, 2)
		ch <- provider.ChatStreamEvent{Delta: "SUMMARY", Kind: nanoprotocol.ChunkKindContent}
		ch <- provider.ChatStreamEvent{Done: true, FullContent: "SUMMARY"}
		close(ch)
		return ch, nil
	}
	a, err := New(Config{
		UserID:                    "u",
		Model:                     "m",
		MaxIterations:             3,
		SessionDir:                tmpDir,
		ContextWindow:             4096,
		SummarizeTokenPercent:     1,
		SummarizeMessageThreshold: 2,
	}, &MockBusClient{}, mockProvider, tool.NewRegistry())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a.sessions = sessStore

	const seed = 20
	sess := sessStore.GetOrCreate("web:floor")
	seedConversation(sess, seed)
	if err := sessStore.Save(sess); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	msg := &nanoprotocol.InboundMessage{UserID: "u", SessionID: "web:floor", Channel: "web", ChatID: "chat-A", Content: "carry on"}
	if err := a.processMessage(context.Background(), msg); err != nil {
		t.Fatalf("processMessage: %v", err)
	}
	if sess.GetSummary() == "" {
		t.Fatal("expected the gate to compact this session")
	}
	// The turn's own user message is the last one when the gate runs, so the
	// floor lands on it.
	if floor := compactFrom(sess); floor != seed {
		t.Fatalf("compact_from = %d, want %d: the summarized prefix must be dropped from this turn's own request", floor, seed)
	}
	ctx := a.contextMessages(sess)
	if len(ctx) >= len(sess.ConversationMessages()) {
		t.Fatalf("context was not reduced: %d of %d conversation messages", len(ctx), len(sess.ConversationMessages()))
	}
	if len(ctx) > 4 {
		t.Fatalf("context holds %d messages after compaction, want the current turn only", len(ctx))
	}
}

// TestEmitUsage_CarriesCompactionThreshold pins the client-facing half of the
// threshold unification: the usage event must report the token count the gate
// fires at. The ring used to hard-code 75 % of the budget while the gate read a
// configured percentage, so a real trigger point looked like a missed one.
func TestEmitUsage_CarriesCompactionThreshold(t *testing.T) {
	var payloads []map[string]any
	mockBus := &MockBusClient{PostChunkFunc: func(c nanoprotocol.OutboundChunk) error {
		if c.Kind == nanoprotocol.ChunkKindStatus {
			var p map[string]any
			if err := json.Unmarshal([]byte(c.Content), &p); err == nil {
				payloads = append(payloads, p)
			}
		}
		return nil
	}}
	a, err := New(Config{
		UserID:                "u",
		Model:                 "m",
		SessionDir:            t.TempDir(),
		ContextWindow:         1_000_000,
		MaxOutputTokens:       384_000,
		SummarizeTokenPercent: 85,
	}, mockBus, provider.NewMock(), tool.NewRegistry())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	a.emitUsageValues(&nanoprotocol.InboundMessage{UserID: "u", SessionID: "web:1", Channel: "web", ChatID: "c"}, 1000, 1_000_000)

	if len(payloads) != 1 {
		t.Fatalf("expected one usage status chunk, got %d", len(payloads))
	}
	if got := payloads[0]["budget"]; got != float64(967232) {
		t.Errorf("budget = %v, want 967232", got)
	}
	if got := payloads[0]["threshold"]; got != float64(822147) {
		t.Errorf("threshold = %v, want 822147 (85%% of the budget)", got)
	}
}
