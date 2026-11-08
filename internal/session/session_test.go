package session

import (
	"testing"

	"gitee.com/kobeyoung81/nanosec/internal/provider"
)

func TestSession_AddMessage(t *testing.T) {
	s := New("test-session")
	msg := provider.Message{Role: "user", Content: "hello"}

	s.AddMessage(msg)

	msgs := s.GetMessages()
	if len(msgs) != 1 {
		t.Fatalf("Expected 1 message, got %d", len(msgs))
	}
	if msgs[0].Content != "hello" {
		t.Errorf("Expected content 'hello', got '%s'", msgs[0].Content)
	}
	if msgs[0].Timestamp.IsZero() {
		t.Error("Expected AddMessage to set a non-zero Timestamp")
	}
}

func TestSession_Clear(t *testing.T) {
	s := New("test-session")
	s.AddMessage(provider.Message{Role: "user", Content: "hello"})

	s.Clear()

	msgs := s.GetMessages()
	if len(msgs) != 0 {
		t.Errorf("Expected 0 messages after Clear, got %d", len(msgs))
	}
}

func TestSession_GetMessages_Copy(t *testing.T) {
	s := New("test-session")
	s.AddMessage(provider.Message{Role: "user", Content: "original"})

	msgs := s.GetMessages()
	msgs[0].Content = "modified"

	// Original should not be modified
	original := s.GetMessages()
	if original[0].Content != "original" {
		t.Error("GetMessages did not return a copy")
	}
}

func TestSession_TurnRunning(t *testing.T) {
	s := New("test-session")
	if s.TurnRunning() {
		t.Fatal("expected a new session to be idle")
	}
	s.SetTurnRunning(true)
	if !s.TurnRunning() {
		t.Fatal("expected TurnRunning true after SetTurnRunning(true)")
	}
	// Clear resets the run state so a reset session is never reported running.
	s.Clear()
	if s.TurnRunning() {
		t.Fatal("expected Clear to reset TurnRunning")
	}
}
