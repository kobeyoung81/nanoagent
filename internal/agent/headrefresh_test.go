package agent

// The head (one system message per session file) is frozen for the life of a
// segment so the provider's prompt cache keeps hitting. Compaction is the one
// moment it is re-read: the floor that compaction writes drops the tail update
// rows from earlier turns, so a file that moved mid-segment would otherwise be
// stale in the head *and* absent from the request. These tests pin that refresh
// on all three paths — the automatic one, the overflow retry and /compact — plus
// the segment split that re-reads the files on its own.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanosec/internal/provider"
	"gitee.com/kobeyoung81/nanosec/internal/session"
	"gitee.com/kobeyoung81/nanosec/internal/tool"
	openai "github.com/sashabaranov/go-openai"
)

// headHome is a temp agent home holding the session files, plus the memory path
// the tests rewrite mid-segment.
type headHome struct {
	cb     *ContextBuilder
	memory string
}

func newHeadHome(t *testing.T) headHome {
	t.Helper()
	ws := t.TempDir()
	agentDir := filepath.Join(ws, "agent")
	bootstrap := filepath.Join(agentDir, "bootstrap")
	memory := filepath.Join(agentDir, "memory")
	for _, dir := range []string{agentDir, bootstrap, memory} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(agentDir, "AGENT.md"), "# Identity\n\nI am a test agent.\n")
	writeFile(t, filepath.Join(bootstrap, "SOUL.md"), "Direct answers only.\n")
	memPath := filepath.Join(memory, "MEMORY.md")
	writeFile(t, memPath, "- uses Go\n")
	return headHome{
		cb:     NewContextBuilder(ws, filepath.Join(agentDir, "AGENT.md"), bootstrap, memory, nil, 1),
		memory: memPath,
	}
}

// mechanicalProvider answers the inner calls a compaction makes: the memory flush
// reports "nothing to save" (so the test, not the model, decides what MEMORY.md
// holds) and the summarizer returns a fixed line.
func mechanicalProvider(t *testing.T) *provider.MockProvider {
	t.Helper()
	m := provider.NewMock()
	m.ResponseFunc = func(_ context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		if len(req.Messages) > 0 && strings.Contains(req.Messages[0].Content, "# Memory flush") {
			return &provider.ChatResponse{Content: "NOTHING_TO_SAVE"}, nil
		}
		return &provider.ChatResponse{Content: "a summary"}, nil
	}
	return m
}

// headSegmentHash returns the hash the head's section for one file carries.
func headSegmentHash(snap headSnapshot, name string) string {
	for _, s := range snap.segments {
		if s.Name == name {
			return s.Hash
		}
	}
	return ""
}

// headCarries reports whether a request carries a *head* row for name (its marker
// opens the row) holding needle. A tail update row mentions the same marker but is
// prefixed with "[更新", and the difference is the point here: the floor is what
// drops the tail update, so only the head can still deliver the file.
func headCarries(msgs []provider.Message, name, needle string) bool {
	for _, m := range msgs {
		if m.Role == "system" && strings.HasPrefix(m.Content, "["+name+"][") && strings.Contains(m.Content, needle) {
			return true
		}
	}
	return false
}

// TestCompactionReSnapshotsTheHead pins the refresh itself: a file the agent
// rewrote mid-segment is in the head again once the turn compacts.
func TestCompactionReSnapshotsTheHead(t *testing.T) {
	home := newHeadHome(t)
	ag := &Agent{ctxBuilder: home.cb}
	ag.summarizer = newSummarizer(mechanicalProvider(t), "m", "")
	ag.summarizer.agent = ag
	sess := session.New("web:chat1")

	before := renderHead(ag.headSnapshotFor(sess).messages)
	if !strings.Contains(before, "[MEMORY.md][") {
		t.Fatalf("head must carry the memory segment, got %q", before)
	}

	writeFile(t, home.memory, "- uses Go\n- prefers Postgres\n")
	// A compaction needs something to summarize: an empty conversation returns
	// early, before the floor and the refresh.
	sess.AddMessage(provider.Message{Role: "user", Content: "hello"})
	sess.AddMessage(provider.Message{Role: "assistant", Content: "hi"})
	if got := ag.summarizer.compactDuringTurn(context.Background(), sess, len(sess.ConversationMessages()), nil); got == "" {
		t.Fatal("expected the compaction to produce a summary")
	}

	snap := ag.headSnapshotFor(sess)
	if !strings.Contains(renderHead(snap.messages), "prefers Postgres") {
		t.Errorf("the head still holds the pre-compaction MEMORY.md:\n%s", renderHead(snap.messages))
	}
	if got, want := headSegmentHash(snap, "MEMORY.md"), TextHash("- uses Go\n- prefers Postgres"); got != want {
		t.Errorf("MEMORY.md head hash = %q, want the file's current hash %q", got, want)
	}
}

// TestOverflowCompactionRefreshesTheHead drives a whole turn: the provider rejects
// the request as too long, the agent force-compacts, and the retried request has to
// carry the file's current text in its head rather than the snapshot the turn
// started with. It is the wiring half — buildMessages has to read the head per
// call, not once per turn.
func TestOverflowCompactionRefreshesTheHead(t *testing.T) {
	home := newHeadHome(t)
	dir := t.TempDir()
	store, err := session.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	turns := 0
	mock := provider.NewMock()
	mock.ResponseFunc = func(_ context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		if len(req.Messages) > 0 && strings.Contains(req.Messages[0].Content, "# Memory flush") {
			return &provider.ChatResponse{Content: "NOTHING_TO_SAVE"}, nil
		}
		if strings.HasSuffix(req.SessionID, ":summarize") {
			return &provider.ChatResponse{Content: "a summary"}, nil
		}
		if strings.HasSuffix(req.SessionID, ":title") {
			// Title generation is another mechanical call; it must not consume the
			// single overflow the test injects.
			return &provider.ChatResponse{Content: "a title"}, nil
		}
		mu.Lock()
		turns++
		n := turns
		mu.Unlock()
		if n == 2 {
			// The upstream's own rejection. It has to be an APIError: a bare error
			// classifies as a provider outage, which callLLM retries instead of
			// handing to the context-length path.
			return nil, &openai.APIError{HTTPStatusCode: 400, Message: "maximum context length exceeded"}
		}
		return &provider.ChatResponse{Content: "ok"}, nil
	}

	ag, err := New(Config{UserID: "u", Model: "m", MaxIterations: 3, SessionDir: dir}, &MockBusClient{}, mock, tool.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	ag.sessions = store
	ag.SetContextBuilder(home.cb)

	turn := func(content string) {
		t.Helper()
		if err := ag.processMessage(context.Background(), &nanoprotocol.InboundMessage{
			UserID: "u", SessionID: "web:chat1", Channel: "web", ChatID: "chat1", Content: content,
		}); err != nil {
			t.Fatalf("processMessage(%q): %v", content, err)
		}
	}
	turn("first turn")
	writeFile(t, home.memory, "- uses Go\n- prefers Postgres\n")
	turn("second turn")

	calls := mock.Calls
	if len(calls) < 4 {
		t.Fatalf("expected the overflow to be retried after a compaction, got %d provider calls", len(calls))
	}
	last := calls[len(calls)-1]
	if !headCarries(last.Messages, "MEMORY.md", "prefers Postgres") {
		t.Errorf("the retried request's head does not carry the file's current text:\n%s", renderHead(last.Messages))
	}
}

// TestSplitReSnapshotsTheHead pins the property a compaction boundary leans on: the
// segment a split hands off to reads the files again (its id is new, so nothing is
// cached for it) and the archived segment's snapshot is dropped.
func TestSplitReSnapshotsTheHead(t *testing.T) {
	home := newHeadHome(t)
	ag, store := newSessionPolicyAgent(t, t.TempDir(), Config{}, nil)
	ag.SetContextBuilder(home.cb)

	sess := store.GetOrCreate("web:chat1")
	if !strings.Contains(renderHead(ag.headSnapshotFor(sess).messages), "[MEMORY.md][") {
		t.Fatal("expected the head to carry the memory segment")
	}
	writeFile(t, home.memory, "- uses Go\n- prefers Postgres\n")

	msg := &nanoprotocol.InboundMessage{UserID: "u", SessionID: "web:chat1", Channel: "web", ChatID: "chat1"}
	next := ag.splitSession(context.Background(), sess, "web:chat1", msg, splitOnCompaction)
	if next.ID == sess.ID {
		t.Fatalf("expected a versioned segment, got %q", next.ID)
	}
	if got := renderHead(ag.headSnapshotFor(next).messages); !strings.Contains(got, "prefers Postgres") {
		t.Errorf("the new segment's head is stale:\n%s", got)
	}
	if n := len(ag.heads); n != 1 {
		t.Errorf("the archived segment's snapshot is still cached: %d snapshots", n)
	}
}

// TestCompactCommandReSnapshotsTheHead covers /compact, which compacts without
// running the ReAct loop: the head it would otherwise keep is the one the segment's
// earlier turns built.
func TestCompactCommandReSnapshotsTheHead(t *testing.T) {
	home := newHeadHome(t)
	ag, store := newSessionPolicyAgent(t, t.TempDir(), Config{}, nil)
	ag.SetContextBuilder(home.cb)

	turn := func(content string) {
		t.Helper()
		if err := ag.processMessage(context.Background(), &nanoprotocol.InboundMessage{
			UserID: "u", SessionID: "web:chat1", Channel: "web", ChatID: "chat1", Content: content,
		}); err != nil {
			t.Fatalf("processMessage(%q): %v", content, err)
		}
	}
	turn("hello") // the segment's first turn freezes the head
	sess := store.GetOrCreate("web:chat1")
	if headSegmentHash(ag.headSnapshotFor(sess), "MEMORY.md") != TextHash("- uses Go") {
		t.Fatal("precondition: the head should still hold the pre-edit MEMORY.md")
	}

	writeFile(t, home.memory, "- uses Go\n- prefers Postgres\n")
	turn("/compact")

	if got, want := headSegmentHash(ag.headSnapshotFor(sess), "MEMORY.md"), TextHash(strings.TrimSpace(readFile(t, home.memory))); got != want {
		t.Errorf("MEMORY.md head hash = %q, want the file's current hash %q", got, want)
	}
}
