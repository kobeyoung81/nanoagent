package agent

import "testing"

// TestParseGoalCommand covers the word boundary: /goal with an argument, bare
// /goal, and the near-misses that are ordinary messages.
func TestParseGoalCommand(t *testing.T) {
	cases := []struct {
		in   string
		desc string
		ok   bool
	}{
		{"/goal 让所有测试通过", "让所有测试通过", true},
		{"/goal", "", true},
		{"  /goal   迁移 token  ", "迁移 token", true},
		{"/goal：中文冒号", "中文冒号", true},
		{"/goals are not a command", "", false},
		{"/goalx", "", false},
		{"please /goal this", "", false},
	}
	for _, tc := range cases {
		desc, ok := parseGoalCommand(tc.in)
		if ok != tc.ok || desc != tc.desc {
			t.Errorf("parseGoalCommand(%q) = %q, %v; want %q, %v", tc.in, desc, ok, tc.desc, tc.ok)
		}
	}
}

// TestGoalContextOf_OnlyForGoalRounds keeps the binding empty on ordinary turns
// so the report tool refuses instead of writing into a stale goal.
func TestGoalContextOf_OnlyForGoalRounds(t *testing.T) {
	ordinary := scheduledMessage("web:c1", "hi")
	ordinary.Metadata["source"] = "cron"
	if gc := goalContextOf(ordinary); gc.GoalID != "" {
		t.Errorf("a cron turn carries a goal binding: %+v", gc)
	}

	round := scheduledMessage("web:c1", "instructions")
	round.Metadata["source"] = "goal"
	round.Metadata["goal_id"] = "g_7"
	round.Metadata["round"] = float64(6) // metadata is decoded from JSON
	round.Metadata["verify"] = true
	gc := goalContextOf(round)
	if gc.GoalID != "g_7" || gc.Round != 6 || !gc.Verify {
		t.Fatalf("binding = %+v", gc)
	}
	if gc.Instruction != "instructions" {
		t.Errorf("instruction = %q; the judge needs the round's own words", gc.Instruction)
	}
}
