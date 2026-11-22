package tool

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func judgeGoal() GoalContext {
	return GoalContext{
		GoalID: "g_9", Round: 2,
		Instruction: "【长程目标 · 第 3 轮】\n判据（全部满足才算完成）：\n所有测试通过\n\n约束：\n- 不改已有测试\n\n这一轮继续推进；结束时调用 goal_report 交结构化证据（命令要带退出码），没有证据不要声称完成。",
	}
}

// TestGoalReport_PostsTheVerdict is the round trip: the judge's answer and the
// evidence reach the workspace's report endpoint, with the internal token.
func TestGoalReport_PostsTheVerdict(t *testing.T) {
	var gotPath, gotToken string
	var got Report
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotToken = r.URL.Path, r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode report: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"running"}`))
	}))
	defer srv.Close()

	tl := NewGoalReportTool(GoalReportOptions{WorkspaceURL: srv.URL, Token: "internal"})
	tl.SetGoalContext(judgeGoal())
	tl.SetGoalJudge(func(_ context.Context, input string) (string, bool) {
		if !strings.Contains(input, "所有测试通过") {
			t.Errorf("the judge did not receive the condition:\n%s", input)
		}
		return `{"verdict":"met","reason":"tests pass"}`, true
	})

	out, err := tl.Execute(context.Background(), map[string]any{
		"summary":  "all green",
		"evidence": []any{map[string]any{"kind": "command", "cmd": "go build ./...", "exit": float64(0)}},
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if gotPath != "/api/v1/workspace/goals/g_9/report" {
		t.Errorf("path = %q", gotPath)
	}
	if gotToken != "Bearer internal" {
		t.Errorf("token = %q", gotToken)
	}
	if got.Verdict != "met" || got.Round != 2 || got.GoalID != "g_9" {
		t.Errorf("report = %+v", got)
	}
	if len(got.Evidence) != 1 || got.Evidence[0].Cmd != "go build ./..." {
		t.Errorf("evidence lost on the way: %+v", got.Evidence)
	}
	if !strings.Contains(out, "met") {
		t.Errorf("the model was not told the verdict: %q", out)
	}
}

// TestGoalReport_UnreadableJudgeAnswerIsNotYet pins the failure mode: a broken
// answer must never look like success.
func TestGoalReport_UnreadableJudgeAnswerIsNotYet(t *testing.T) {
	for _, raw := range []string{
		"no json here",
		`{"verdict":"maybe"}`,
		`{"verdict":`,
	} {
		verdict, reason, _ := parseVerdict(raw)
		if verdict != "not_yet" {
			t.Errorf("%q → %q, want not_yet", raw, verdict)
		}
		if reason == "" {
			t.Errorf("%q produced no reason", raw)
		}
	}
}

// TestGoalReport_ParsesFencedJSON covers a model that wraps its answer.
func TestGoalReport_ParsesFencedJSON(t *testing.T) {
	verdict, reason, missing := parseVerdict("```json\n{\"verdict\":\"impossible\",\"reason\":\"contradicts the constraints\",\"missing\":[\"x\"]}\n```")
	if verdict != "impossible" || missing[0] != "x" {
		t.Fatalf("verdict = %q, missing = %v, reason = %q", verdict, missing, reason)
	}
}

// TestSplitInstruction covers the parser that feeds the judge's CONDITION and
// CONSTRAINTS sections, including the fallback for text it does not recognise.
func TestSplitInstruction(t *testing.T) {
	cond, cons := splitInstruction(judgeGoal().Instruction)
	if !strings.Contains(cond, "所有测试通过") || strings.Contains(cond, "约束") {
		t.Errorf("condition = %q", cond)
	}
	if !strings.Contains(cons, "不改已有测试") {
		t.Errorf("constraints = %q", cons)
	}

	cond, cons = splitInstruction("just a sentence")
	if cond != "just a sentence" {
		t.Errorf("fallback condition = %q", cond)
	}
	if cons == "" {
		t.Error("fallback constraints must not be empty")
	}
}

// TestGoalReport_RefusesWithoutAJudge keeps the tool from self-certifying.
func TestGoalReport_RefusesWithoutAJudge(t *testing.T) {
	tl := NewGoalReportTool()
	tl.SetGoalContext(judgeGoal())
	_, err := tl.Execute(context.Background(), map[string]any{
		"summary":  "done",
		"evidence": []any{map[string]any{"kind": "file", "path": "a.go"}},
	})
	if err == nil || !strings.Contains(err.Error(), "judge") {
		t.Fatalf("err = %v, want a missing-judge error", err)
	}
}

// TestGoalReport_DeliveryFailureIsReported makes a lost report visible to the
// model instead of silently ending the round.
func TestGoalReport_DeliveryFailureIsReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	defer srv.Close()
	tl := NewGoalReportTool(GoalReportOptions{WorkspaceURL: srv.URL, Token: "t"})
	tl.SetGoalContext(judgeGoal())
	tl.SetGoalJudge(func(context.Context, string) (string, bool) { return `{"verdict":"not_yet"}`, true })
	_, err := tl.Execute(context.Background(), map[string]any{
		"summary":  "done",
		"evidence": []any{map[string]any{"kind": "file", "path": "a.go"}},
	})
	if err == nil || !strings.Contains(err.Error(), "not delivered") {
		t.Fatalf("err = %v, want a delivery error", err)
	}
}

// TestGoalReport_SecondCallIsHarmless pins the guard against a model that
// reports twice: the second call answers without judging or posting, instead of
// sending a report for a round the host has already advanced.
func TestGoalReport_SecondCallIsHarmless(t *testing.T) {
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var judgements int
	tl := NewGoalReportTool(GoalReportOptions{WorkspaceURL: srv.URL, Token: "t"})
	tl.SetGoalContext(judgeGoal())
	tl.SetGoalJudge(func(context.Context, string) (string, bool) {
		judgements++
		return `{"verdict":"met"}`, true
	})
	params := map[string]any{
		"summary":  "done",
		"evidence": []any{map[string]any{"kind": "file", "path": "a.go"}},
	}
	if _, err := tl.Execute(context.Background(), params); err != nil {
		t.Fatalf("first: %v", err)
	}
	out, err := tl.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if posts != 1 || judgements != 1 {
		t.Fatalf("posts = %d, judgements = %d; want one of each", posts, judgements)
	}
	if !strings.Contains(out, "已经提交") {
		t.Errorf("second call answered %q", out)
	}

	// A new round resets the guard.
	gc := judgeGoal()
	gc.Round++
	tl.SetGoalContext(gc)
	if _, err := tl.Execute(context.Background(), params); err != nil {
		t.Fatalf("next round: %v", err)
	}
	if posts != 2 {
		t.Fatalf("posts = %d, want 2 after the round advanced", posts)
	}
}

// TestGoalReport_VerificationRoundMayReport covers the round whose only job is
// to report: it keeps the round number, so counting "one report per round"
// without the verify flag refused it and the goal stayed in verifying.
func TestGoalReport_VerificationRoundMayReport(t *testing.T) {
	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"achieved"}`))
	}))
	defer srv.Close()

	tl := NewGoalReportTool(GoalReportOptions{WorkspaceURL: srv.URL, Token: "internal"})
	tl.SetGoalJudge(func(context.Context, string) (string, bool) {
		return `{"verdict":"met","reason":"ok"}`, true
	})
	report := map[string]any{
		"summary":  "done",
		"evidence": []any{map[string]any{"kind": "command", "cmd": "ls", "exit": float64(0)}},
	}

	work := judgeGoal()
	tl.SetGoalContext(work)
	if _, err := tl.Execute(context.Background(), report); err != nil {
		t.Fatalf("work round: %v", err)
	}

	// The same round number, this time the verification.
	verify := work
	verify.Verify = true
	tl.SetGoalContext(verify)
	if _, err := tl.Execute(context.Background(), report); err != nil {
		t.Fatalf("verify round: %v", err)
	}
	if posts != 2 {
		t.Fatalf("the verification round's report was not delivered: posts=%d", posts)
	}
}

// TestGoalReport_SameRoundTwiceIsStillRefused keeps the guard the flag above
// relaxes: a model that calls goal_report twice in one round only sends once.
func TestGoalReport_SameRoundTwiceIsStillRefused(t *testing.T) {
	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"running"}`))
	}))
	defer srv.Close()

	tl := NewGoalReportTool(GoalReportOptions{WorkspaceURL: srv.URL, Token: "internal"})
	tl.SetGoalJudge(func(context.Context, string) (string, bool) {
		return `{"verdict":"not_yet","reason":"not done"}`, true
	})
	tl.SetGoalContext(judgeGoal())
	report := map[string]any{"summary": "partly", "evidence": []any{map[string]any{"kind": "command", "cmd": "ls", "exit": float64(0)}}}
	if _, err := tl.Execute(context.Background(), report); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := tl.Execute(context.Background(), report); err != nil {
		t.Fatalf("second: %v", err)
	}
	if posts != 1 {
		t.Fatalf("a second report in the same round was delivered: posts=%d", posts)
	}
}
