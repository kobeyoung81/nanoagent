package tool

import (
	"context"
	"sync"
	"testing"
)

// turnCtx is the binding a tool call belongs to, for tests that do not go
// through the agent loop.
func turnCtx(goal GoalContext, intake IntakeContext) context.Context {
	return WithTurn(context.Background(), NewTurn(goal, intake))
}

// TestTurn_BindingIsPerTurn is the concurrency the agent actually has: several
// messages run at once, and the goal surfaces used to read their binding from
// the tool instance — so a message in one conversation cleared the binding a
// round in another was still using. Two turns must not see each other's.
func TestTurn_BindingIsPerTurn(t *testing.T) {
	work := turnCtx(GoalContext{GoalID: "g_1", Round: 0}, IntakeContext{})
	verify := turnCtx(GoalContext{GoalID: "g_1", Round: 0, Verify: true}, IntakeContext{})
	other := turnCtx(GoalContext{GoalID: "g_2", Round: 3}, IntakeContext{DraftID: "g_9"})

	workTurn, verifyTurn, otherTurn := TurnFrom(work), TurnFrom(verify), TurnFrom(other)
	if workTurn == verifyTurn || verifyTurn == otherTurn {
		t.Fatal("each message must get its own turn")
	}
	if workTurn.Goal.Verify {
		t.Error("the work round must not inherit the verification flag")
	}
	if !verifyTurn.Goal.Verify {
		t.Error("the verification turn lost its flag")
	}
	if otherTurn.Intake.DraftID != "g_9" {
		t.Errorf("intake binding = %q", otherTurn.Intake.DraftID)
	}

	// A report delivered by one turn does not silence the other.
	workTurn.MarkGoalReported()
	if verifyTurn.GoalReported() {
		t.Error("the verification turn was told it had already reported")
	}
	if TurnFrom(context.Background()) != nil {
		t.Error("a context without a turn must have no binding")
	}
}

// TestTurn_SharedAcrossToolCalls checks the flag survives inside one turn (a
// model that calls goal_report twice), which is the only reason it exists.
func TestTurn_SharedAcrossToolCalls(t *testing.T) {
	ctx := turnCtx(GoalContext{GoalID: "g_1"}, IntakeContext{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		TurnFrom(ctx).MarkGoalReported()
	}()
	wg.Wait()
	if !TurnFrom(ctx).GoalReported() {
		t.Fatal("the same turn must see its own delivery")
	}
}
