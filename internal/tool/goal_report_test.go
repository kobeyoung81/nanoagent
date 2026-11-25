package tool

import (
	"context"
	"net/http"
	"net/http/httptest"
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
	ctx := turnCtx(goalRound(), IntakeContext{})
	_, err := tl.Execute(ctx, map[string]any{
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
	ctx := turnCtx(goalRound(), IntakeContext{})

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
			_, err := tl.Execute(ctx, tc.params)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestGoalReport_NormalisesTheReport covers the happy path: the binding is
// stamped on and all three evidence shapes survive the round trip.
func TestGoalReport_NormalisesTheReport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var judged string
	tl := NewGoalReportTool(GoalReportOptions{WorkspaceURL: srv.URL, Token: "t"})
	ctx := turnCtx(goalRound(), IntakeContext{})
	tl.SetGoalJudge(func(_ context.Context, input string) (string, bool) {
		judged = input
		return `{"verdict":"not_yet","reason":"four call sites left","missing":["middleware.go"]}`, true
	})
	out, err := tl.Execute(ctx, map[string]any{
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
	res := decodeResult(t, out)
	if !res.Delivered || res.Round != 4 || res.Verdict != "not_yet" || res.Reason != "four call sites left" {
		t.Errorf("answer = %s, want round 4 / not_yet / the judge's reason", out)
	}
	// The judge sees the round's evidence, not the model's summary alone.
	for _, want := range []string{
		"go build ./...", "exit 0", "internal/auth/token.go",
		"update call sites", "auth token migrated",
	} {
		if !strings.Contains(judged, want) {
			t.Errorf("judge input is missing %q:\n%s", want, judged)
		}
	}
}
