package tool

import (
	"context"
	"sync"
)

// The per-turn binding of the goal surfaces.
//
// A goal round knows which goal and round it is; an intake turn knows which
// draft it fills. Both used to live on the tool instance, and the loop reset
// them on every inbound message — the agent runs up to
// `agents.max_concurrent_messages` messages at once, so a message in one
// conversation cleared the binding a round in another conversation was still
// using. The round's report then either refused ("only available inside a
// long-running goal round") or was told it had already been submitted.
//
// The binding is therefore carried by the turn's context, and a new one is
// created per message.

// Turn is one message's bindings and the small bookkeeping that belongs to it.
type Turn struct {
	Goal   GoalContext
	Intake IntakeContext

	mu       sync.Mutex
	reported bool
}

// NewTurn builds the binding for one message.
func NewTurn(goal GoalContext, intake IntakeContext) *Turn {
	return &Turn{Goal: goal, Intake: intake}
}

type turnKey struct{}

// WithTurn puts the binding on the message's context.
func WithTurn(ctx context.Context, turn *Turn) context.Context {
	if turn == nil {
		return ctx
	}
	return context.WithValue(ctx, turnKey{}, turn)
}

// TurnFrom reads the binding a tool call belongs to. nil means the call is not
// part of a message with a binding (a utility model, a test), and the goal tools
// refuse rather than acting on another turn's state.
func TurnFrom(ctx context.Context) *Turn {
	turn, _ := ctx.Value(turnKey{}).(*Turn)
	return turn
}

// GoalReported reports whether this turn already delivered a report.
func (t *Turn) GoalReported() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.reported
}

// MarkGoalReported records the delivery.
func (t *Turn) MarkGoalReported() {
	t.mu.Lock()
	t.reported = true
	t.mu.Unlock()
}
