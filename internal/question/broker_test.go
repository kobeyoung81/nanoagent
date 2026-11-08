package question

import (
	"context"
	"testing"
	"time"
)

func TestBroker_WaitAndDeliver(t *testing.T) {
	b := NewBroker()
	done := make(chan []string, 1)
	go func() {
		answers, err := b.Wait(context.Background(), "q1", "sess1")
		if err != nil {
			done <- nil
			return
		}
		done <- answers
	}()

	deadline := time.After(time.Second)
	for !b.IsWaiting("sess1") {
		select {
		case <-deadline:
			t.Fatal("waiter was never registered")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if b.IsWaiting("other") {
		t.Error("unexpected waiter for a different session")
	}

	if !b.Deliver("q1", []string{"yes"}) {
		t.Fatal("Deliver returned false for a registered waiter")
	}
	if b.Deliver("q1", []string{"again"}) {
		t.Error("Deliver should return false for an already-resolved question")
	}

	select {
	case answers := <-done:
		if len(answers) != 1 || answers[0] != "yes" {
			t.Errorf("unexpected answers: %v", answers)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter was not resolved")
	}
}

func TestBroker_Cancel(t *testing.T) {
	b := NewBroker()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	if _, err := b.Wait(ctx, "q2", "sess2"); err == nil {
		t.Error("expected context cancellation error")
	}
}
