package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanosec/internal/provider"
	"gitee.com/kobeyoung81/nanosec/internal/session"
)

// TestBudgetAvailable_ClampsOutputReserve pins the fix for auto-compaction
// firing far too early: a catalog max_output_tokens that describes the model's
// capability (384k on a 1M model) must not be reserved verbatim.
func TestBudgetAvailable_ClampsOutputReserve(t *testing.T) {
	cases := []struct{ ctx, out, want int }{
		{1_000_000, 384_000, 1_000_000 - maxOutputReserve},
		{200_000, 128_000, 200_000 - maxOutputReserve},
		{128_000, 4_096, 128_000 - 4_096},
		{8_192, 1_024, 8_192 - 1_024},
		{1_024, 4_096, 1_024}, // floor
	}
	for _, c := range cases {
		if got := budgetAvailable(c.ctx, c.out); got != c.want {
			t.Errorf("budgetAvailable(%d, %d) = %d, want %d", c.ctx, c.out, got, c.want)
		}
	}
}

// TestNoteSessionAgent_AnchorsPlanArtefact verifies a plan -> build switch
// injects a mode note pointing at the plan artefact, and that the note is not
// rewritten while the mode is unchanged.
func TestNoteSessionAgent_AnchorsPlanArtefact(t *testing.T) {
	ag, _ := newSessionPolicyAgent(t, t.TempDir(), Config{}, nil)
	sess := session.New("web:chat1")

	ag.noteSessionAgent(sess, ResolvedAgent{ID: "plan"})
	if note := sess.GetMeta(sessionModeNoteMetaKey); note != "Mode: plan." {
		t.Errorf("plan note = %q", note)
	}

	// A plan artefact written in plan mode is picked up by the next switch.
	sess.SetMeta(lastPlanPathMetaKey, "/home/u/plans/fix-session-split.md")
	ag.noteSessionAgent(sess, ResolvedAgent{ID: "build"})
	note := sess.GetMeta(sessionModeNoteMetaKey)
	if !strings.HasPrefix(note, "Mode: build.") || !strings.Contains(note, "/home/u/plans/fix-session-split.md") {
		t.Errorf("build note must point at the plan artefact, got %q", note)
	}

	// Same mode again: the note must not be rewritten (keeps the prompt prefix
	// stable for provider caching).
	sess.SetMeta(sessionModeNoteMetaKey, "sentinel")
	ag.noteSessionAgent(sess, ResolvedAgent{ID: "build"})
	if got := sess.GetMeta(sessionModeNoteMetaKey); got != "sentinel" {
		t.Errorf("note rewritten for an unchanged mode: %q", got)
	}
}

// TestSplitSession_CarriesSummaryPlanAndMode verifies a session split keeps the
// conversation's bearings: the summary crosses the boundary (so the new segment
// is not context-free), the plan artefact path and mode note are inherited, the
// reply is retargeted at the new session and the switch is announced.
func TestSplitSession_CarriesSummaryPlanAndMode(t *testing.T) {
	bus := &MockBusClient{}
	var events []map[string]string
	bus.PostChunkFunc = func(chunk nanoprotocol.OutboundChunk) error {
		var payload map[string]string
		if json.Unmarshal([]byte(chunk.Content), &payload) == nil {
			events = append(events, payload)
		}
		return nil
	}
	ag, store := newSessionPolicyAgent(t, t.TempDir(), Config{}, bus)
	ag.rememberMessage(&nanoprotocol.InboundMessage{Channel: "web", ChatID: "chat1", SessionID: "web:chat1"})

	sess := store.GetOrCreate("web:chat1")
	sess.SetSummary("the agreed plan is X")
	sess.SetMeta(lastPlanPathMetaKey, "/home/u/plans/x.md")
	sess.SetMeta(sessionAgentMetaKey, "plan")
	sess.AddMessage(provider.Message{Role: "user", Content: "do the thing"})
	sess.AddMessage(provider.Message{Role: "assistant", Content: "planned"})

	msg := &nanoprotocol.InboundMessage{UserID: "u", SessionID: "web:chat1", Channel: "web", ChatID: "chat1"}
	next := ag.splitSession(context.Background(), sess, "web:chat1", msg, splitOnCompaction)

	if next.ID == "web:chat1" {
		t.Fatalf("expected a versioned session, got %q", next.ID)
	}
	if msg.SessionID != next.ID {
		t.Errorf("msg.SessionID = %q, want %q", msg.SessionID, next.ID)
	}
	if got := next.GetSummary(); got != "the agreed plan is X" {
		t.Errorf("new session summary = %q (the LLM context injection source)", got)
	}
	if got := next.GetMeta(handoffSummaryMetaKey); got != "the agreed plan is X" {
		t.Errorf("handoff_summary = %q", got)
	}
	if got := next.GetMeta(handoffFromMetaKey); got != "web:chat1" {
		t.Errorf("handoff_from = %q", got)
	}
	if got := next.GetMeta(lastPlanPathMetaKey); got != "/home/u/plans/x.md" {
		t.Errorf("plan path not inherited: %q", got)
	}
	if got := sess.GetMeta("_archived"); got != "true" {
		t.Errorf("previous session not archived: %q", got)
	}
	found := false
	for _, e := range events {
		if e["event"] == "session_switch" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a session_switch status chunk, got %v", events)
	}
}

// TestSplitSession_WritesSummaryWhenMissing covers the reported failure mode:
// a session that never accumulated a summary used to hand off nothing at all.
func TestSplitSession_WritesSummaryWhenMissing(t *testing.T) {
	bus := &MockBusClient{}
	providerMock := provider.NewMock()
	providerMock.Response = &provider.ChatResponse{Content: "fresh summary"}
	ag, store := newSessionPolicyAgent(t, t.TempDir(), Config{}, bus)
	ag.summarizer = newSummarizer(providerMock, "model", "")
	ag.summarizer.agent = ag

	sess := store.GetOrCreate("web:chat1")
	for i := 0; i < 6; i++ {
		sess.AddMessage(provider.Message{Role: "user", Content: "m"})
	}
	msg := &nanoprotocol.InboundMessage{UserID: "u", SessionID: "web:chat1"}

	next := ag.splitSession(context.Background(), sess, "web:chat1", msg, splitOnCompaction)

	if len(providerMock.Calls) == 0 {
		t.Fatal("expected the split to synthesize a summary before archiving")
	}
	if got := next.GetSummary(); got != "fresh summary" {
		t.Errorf("new session summary = %q, want the synthesized one", got)
	}
}
