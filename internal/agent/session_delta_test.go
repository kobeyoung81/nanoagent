package agent

// The session sync is what the storage migration exists for: a conversation used
// to cross the bus in full on every flush, and now only what is new does. These
// tests pin the three decisions that make it correct — the first snapshot is the
// whole conversation, a later one is only the suffix, and a bus that cannot apply
// a suffix gets the whole conversation instead.

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanosec/internal/provider"
	"gitee.com/kobeyoung81/nanosec/internal/session"
)

// recordingBus collects the save requests a syncer makes, and can be told to
// refuse a delta the way the bus does when it is missing a snapshot. It embeds
// the package's mock client so it satisfies BusClient, and replaces the one
// method under test.
type recordingBus struct {
	MockBusClient
	mu       sync.Mutex
	requests []nanoprotocol.SaveSessionRequest
	// resyncOnce refuses the *next* delta, the way a bus that is missing one
	// snapshot does — after the full snapshot that follows, it can take deltas
	// again.
	resyncOnce bool
}

func (b *recordingBus) SaveSession(req nanoprotocol.SaveSessionRequest) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.requests = append(b.requests, req)
	if b.resyncOnce && req.MessagesOmitted {
		b.resyncOnce = false
		return nanoprotocol.ErrSessionResync
	}
	return nil
}

func (b *recordingBus) saved() []nanoprotocol.SaveSessionRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]nanoprotocol.SaveSessionRequest{}, b.requests...)
}

func snapshot(id string, contents ...string) session.SessionSnapshot {
	msgs := make([]provider.Message, 0, len(contents))
	for i, c := range contents {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, provider.Message{Role: role, Content: c})
	}
	return session.SessionSnapshot{
		SessionID: id, AgentID: "agent-1", Messages: msgs,
		Meta: map[string]string{"_title": "A Title"},
	}
}

func decodeMessages(t *testing.T, payload string) []map[string]any {
	t.Helper()
	var out []map[string]any
	if err := json.Unmarshal([]byte(payload), &out); err != nil {
		t.Fatalf("payload is not a JSON array: %v (%s)", err, payload)
	}
	return out
}

func TestBusSessionSyncer_FirstSendIsTheWholeConversation(t *testing.T) {
	bus := &recordingBus{}
	syncer := newBusSessionSyncer(bus, "user-1")

	sent, err := syncer.SyncSession(snapshot("web:chat-1", "one", "two"))
	if err != nil {
		t.Fatalf("SyncSession: %v", err)
	}
	if sent == 0 {
		t.Fatal("the syncer reported sending nothing")
	}

	saved := bus.saved()
	if len(saved) != 1 {
		t.Fatalf("expected one request, got %d", len(saved))
	}
	req := saved[0]
	if req.MessagesOmitted || req.FromIndex != 0 {
		t.Fatalf("the first snapshot must be a full one: %+v", req)
	}
	if got := decodeMessages(t, req.Messages); len(got) != 2 {
		t.Fatalf("expected both messages, got %d", len(got))
	}
	// The scalars still travel every time: a delta replaces none of them.
	if req.Title != "A Title" || req.Status != "active" || req.Channel != "web" || req.ChatID != "chat-1" {
		t.Fatalf("unexpected scalars: %+v", req)
	}
}

// TestBusSessionSyncer_SecondSendIsOnlyTheSuffix is the change that stops the
// disk from filling: the payload is the new messages, not the conversation.
func TestBusSessionSyncer_SecondSendIsOnlyTheSuffix(t *testing.T) {
	bus := &recordingBus{}
	syncer := newBusSessionSyncer(bus, "user-1")

	if _, err := syncer.SyncSession(snapshot("web:chat-1", "one", "two")); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	_, err := syncer.SyncSession(snapshot("web:chat-1", "one", "two", "three", "four"))
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}

	saved := bus.saved()
	if len(saved) != 2 {
		t.Fatalf("expected two requests, got %d", len(saved))
	}
	delta := saved[1]
	if !delta.MessagesOmitted || delta.FromIndex != 2 {
		t.Fatalf("expected a delta from index 2: %+v", delta)
	}
	// The delta carries the two new messages and nothing else — that is the whole
	// point, and the byte count it reports has to reflect it.
	if got := decodeMessages(t, delta.Messages); len(got) != 2 {
		t.Fatalf("expected two messages in the delta, got %d: %s", len(got), delta.Messages)
	}
	// The comparison that matters is against sending everything: the conversation
	// is four messages and the delta is two of them.
	var whole []provider.Message
	for _, c := range []string{"one", "two", "three", "four"} {
		whole = append(whole, provider.Message{Role: "user", Content: c})
	}
	fullPayload, err := json.Marshal(whole)
	if err != nil {
		t.Fatalf("marshal whole conversation: %v", err)
	}
	if len(delta.Messages) >= len(fullPayload) {
		t.Fatalf("the delta (%d bytes) is not smaller than the whole conversation (%d bytes)",
			len(delta.Messages), len(fullPayload))
	}
}

// TestBusSessionSyncer_UnchangedConversationStillReportsState: a flush that adds
// no message still has to carry turn_running / status, and it must not claim to
// be a full snapshot (that would rewrite the array).
func TestBusSessionSyncer_UnchangedConversationStillReportsState(t *testing.T) {
	bus := &recordingBus{}
	syncer := newBusSessionSyncer(bus, "user-1")

	if _, err := syncer.SyncSession(snapshot("web:chat-1", "one")); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	snap := snapshot("web:chat-1", "one")
	snap.TurnRunning = true
	if _, err := syncer.SyncSession(snap); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	saved := bus.saved()
	last := saved[len(saved)-1]
	if !last.MessagesOmitted || last.FromIndex != 1 {
		t.Fatalf("expected an empty delta from index 1: %+v", last)
	}
	if len(decodeMessages(t, last.Messages)) != 0 {
		t.Fatalf("expected no messages in the delta: %s", last.Messages)
	}
	if !last.TurnRunning {
		t.Fatalf("the state change was not carried: %+v", last)
	}
}

// TestBusSessionSyncer_ResyncSendsEverything: when the bus says it cannot apply a
// suffix, the sender answers with the whole conversation in the same call — not
// with an error the ledger would count, and not by retrying the delta.
func TestBusSessionSyncer_ResyncSendsEverything(t *testing.T) {
	bus := &recordingBus{resyncOnce: true}
	syncer := newBusSessionSyncer(bus, "user-1")

	if _, err := syncer.SyncSession(snapshot("web:chat-1", "one")); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	sent, err := syncer.SyncSession(snapshot("web:chat-1", "one", "two"))
	if err != nil {
		t.Fatalf("the resync should have been handled inside the call: %v", err)
	}
	if sent == 0 {
		t.Fatal("the syncer reported sending nothing")
	}

	saved := bus.saved()
	if len(saved) != 3 {
		t.Fatalf("expected the delta and then the full snapshot, got %d requests", len(saved))
	}
	if !saved[1].MessagesOmitted {
		t.Fatalf("the second request should have been the delta: %+v", saved[1])
	}
	if saved[2].MessagesOmitted || saved[2].FromIndex != 0 {
		t.Fatalf("the third request should have been the full snapshot: %+v", saved[2])
	}
	if got := decodeMessages(t, saved[2].Messages); len(got) != 2 {
		t.Fatalf("the resync snapshot is not the whole conversation: %d messages", len(got))
	}

	// And the next save is a delta again, measured from the full snapshot it just
	// sent: the index was reset, not left at the stale value.
	if _, err := syncer.SyncSession(snapshot("web:chat-1", "one", "two", "three")); err != nil {
		t.Fatalf("third sync: %v", err)
	}
	last := bus.saved()
	if !last[len(last)-1].MessagesOmitted || last[len(last)-1].FromIndex != 2 {
		t.Fatalf("expected a delta from index 2 after the resync: %+v", last[len(last)-1])
	}
}

// TestBusSessionSyncer_ShrunkSegmentIsSentInFull: a segment's messages are only
// appended to, so a shorter array means something rewrote them — and then no
// suffix can be trusted.
func TestBusSessionSyncer_ShrunkSegmentIsSentInFull(t *testing.T) {
	bus := &recordingBus{}
	syncer := newBusSessionSyncer(bus, "user-1")

	if _, err := syncer.SyncSession(snapshot("web:chat-1", "one", "two", "three")); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if _, err := syncer.SyncSession(snapshot("web:chat-1", "one")); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	saved := bus.saved()
	last := saved[len(saved)-1]
	if last.MessagesOmitted {
		t.Fatalf("a shrunk segment must be sent in full: %+v", last)
	}
	if got := decodeMessages(t, last.Messages); len(got) != 1 {
		t.Fatalf("expected the one remaining message, got %d", len(got))
	}
}

// TestBusSessionSyncer_SeparateSessionsHaveSeparateCursors: compaction starts a
// new session id, and the new one has to begin with a full snapshot even if the
// old one was mid-delta.
func TestBusSessionSyncer_SeparateSessionsHaveSeparateCursors(t *testing.T) {
	bus := &recordingBus{}
	syncer := newBusSessionSyncer(bus, "user-1")

	if _, err := syncer.SyncSession(snapshot("web:chat-1:v1", "one", "two")); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if _, err := syncer.SyncSession(snapshot("web:chat-1:v2", "summary")); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	saved := bus.saved()
	if saved[1].MessagesOmitted {
		t.Fatalf("the new segment must start with a full snapshot: %+v", saved[1])
	}
}

// TestBusSessionSyncer_ClientErrorIsReturned: anything that is not a resync is a
// failure the ledger has to see.
func TestBusSessionSyncer_ClientErrorIsReturned(t *testing.T) {
	wantErr := errors.New("bus unreachable")
	bus := &MockBusClient{SaveSessionFunc: func(nanoprotocol.SaveSessionRequest) error { return wantErr }}
	syncer := newBusSessionSyncer(bus, "user-1")

	if _, err := syncer.SyncSession(snapshot("web:chat-1", "one")); !errors.Is(err, wantErr) {
		t.Fatalf("expected the client's error, got %v", err)
	}
}
