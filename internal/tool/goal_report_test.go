package tool

import (
	"context"
	"strings"
	"testing"
)

func goalRound() GoalContext {
	return GoalContext{GoalID: "g_1", Round: 3}
}

// TestGoalReport_RequiresAGoalRound keeps the tool invisible outside the loop:
// a normal conversation that calls it is refused rather than silently accepted.
func TestGoalReport_RequiresAGoalRound(t *testing.T) {
	tl := NewGoalReportTool()
	_, err := tl.Execute(context.Background(), map[string]any{
		"summary":  "fixed it",
		"evidence": []any{map[string]any{"kind": "file", "path": "a.go"}},
	})
	if err == nil {
		t.Fatal("a report outside a goal round must be refused")
	}
}

// TestGoalReport_RejectsInterrupted pins the one kind the model may not submit.
// An interrupted round is a platform fact; accepting it here would give the
// agent a second way to end a goal.
func TestGoalReport_RejectsInterrupted(t *testing.T) {
	tl := NewGoalReportTool()
	tl.SetGoalContext(goalRound())
	_, err := tl.Execute(context.Background(), map[string]any{
		"summary":  "stopped early",
		"evidence": []any{map[string]any{"kind": "interrupted", "note": "ran out of time"}},
	})
	if err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("interrupted = %v, want an invalid-kind error", err)
	}
}

// TestGoalReport_L1Checks covers every way a report can be malformed. Each of
// these would otherwise reach the judge as a claim nobody can check.
func TestGoalReport_L1Checks(t *testing.T) {
	tl := NewGoalReportTool()
	tl.SetGoalContext(goalRound())

	cases := []struct {
		name   string
		params map[string]any
		want   string
	}{
		{"no summary", map[string]any{
			"evidence": []any{map[string]any{"kind": "file", "path": "a.go"}},
		}, "summary"},
		{"no evidence", map[string]any{
			"summary": "did it",
		}, "evidence"},
		{"empty evidence", map[string]any{
			"summary": "did it", "evidence": []any{},
		}, "evidence"},
		{"command without a command", map[string]any{
			"summary": "did it", "evidence": []any{map[string]any{"kind": "command", "exit": float64(0)}},
		}, "needs a cmd"},
		{"command without an exit code", map[string]any{
			"summary": "did it", "evidence": []any{map[string]any{"kind": "command", "cmd": "go build ./..."}},
		}, "exit code"},
		{"file without a path", map[string]any{
			"summary": "did it", "evidence": []any{map[string]any{"kind": "file"}},
		}, "needs a path"},
		{"list without items", map[string]any{
			"summary": "did it", "evidence": []any{map[string]any{"kind": "list"}},
		}, "needs items"},
		{"list item without text", map[string]any{
			"summary": "did it", "evidence": []any{map[string]any{"kind": "list", "items": []any{
				map[string]any{"done": true},
			}}},
		}, "text is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tl.Execute(context.Background(), tc.params)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestGoalReport_NormalisesTheReport covers the happy path: the binding is
// stamped on and all three evidence shapes survive the round trip.
func TestGoalReport_NormalisesTheReport(t *testing.T) {
	tl := NewGoalReportTool()
	tl.SetGoalContext(goalRound())
	out, err := tl.Execute(context.Background(), map[string]any{
		"summary": "  auth token migrated  ",
		"evidence": []any{
			map[string]any{"kind": "command", "cmd": "go build ./...", "exit": float64(0), "tail": "ok"},
			map[string]any{"kind": "file", "path": "internal/auth/token.go", "note": "rewrote refresh"},
			map[string]any{"kind": "list", "items": []any{
				map[string]any{"text": "update call sites", "done": false},
			}},
		},
		"remaining": []any{"four call sites left"},
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	for _, want := range []string{
		`"goal_id": "g_1"`, `"round": 3`, `"summary": "auth token migrated"`,
		`"cmd": "go build ./..."`, `"exit": 0`,
		`"internal/auth/token.go"`, "update call sites",
		"four call sites left",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report is missing %q:\n%s", want, out)
		}
	}
}
