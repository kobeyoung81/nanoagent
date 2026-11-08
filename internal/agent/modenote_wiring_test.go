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

// TestTurnInjectsTheModeNote pins the call site, not the renderer: the mode note
// has to reach the session from a real turn. The renderer and the injection both
// existed for a whole release with no caller — nothing in the transcript showed
// the mode, and every test that called injectSessionState directly stayed green —
// so this test drives processMessage and checks the row it produces.
func TestTurnInjectsTheModeNote(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := session.NewStore(tmpDir)
	if err != nil {
		t.Fatal(err)
	}

	mockProvider := provider.NewMock()
	mockProvider.ResponseFunc = func(context.Context, provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "ok"}, nil
	}
	ag, err := New(Config{UserID: "u", MaxIterations: 3, SessionDir: tmpDir}, &MockBusClient{}, mockProvider, tool.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	ag.sessions = store

	sess := store.GetOrCreate("web:chat-mode-note")
	if err := ag.processMessage(context.Background(), &nanoprotocol.InboundMessage{
		UserID: "u", SessionID: "web:chat-mode-note", Channel: "web", ChatID: "chat-mode-note", Content: "hi",
	}); err != nil {
		t.Fatalf("processMessage: %v", err)
	}

	sess = store.GetOrCreate("web:chat-mode-note")
	notes := 0
	for _, m := range sess.Messages {
		if m.Role == "system" && strings.Contains(m.Content, modeNotePrefix) {
			notes++
		}
	}
	if notes != 1 {
		t.Fatalf("expected exactly one mode note row after one turn, got %d", notes)
	}

	// The note has to be in the request too, or it steers nothing.
	if len(mockProvider.Calls) == 0 {
		t.Fatal("no provider call was recorded")
	}
	sent := false
	for _, m := range mockProvider.Calls[0].Messages {
		if m.Role == "system" && strings.Contains(m.Content, modeNotePrefix) {
			sent = true
		}
	}
	if !sent {
		t.Error("the mode note must be part of the request the model receives")
	}
}
