package agent

import (
	"context"
	"testing"
)

func TestTurnCancellation(t *testing.T) {
	a := &Agent{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a.registerTurn("s1", cancel)
	if !a.cancelTurn("s1") {
		t.Fatal("expected cancelTurn to find the registered turn")
	}
	if ctx.Err() == nil {
		t.Fatal("expected the turn context to be cancelled")
	}
	if a.cancelTurn("missing") {
		t.Error("cancelTurn should return false for an unknown session")
	}
	a.unregisterTurn("s1")
	if a.cancelTurn("s1") {
		t.Error("cancelTurn should return false after unregister")
	}
}
