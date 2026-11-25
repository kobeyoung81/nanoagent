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
		Instruction: "【长程目标 · 第 3 轮】\n\n目标\n所有测试通过\n\n要守住的边界\n- 不改已有测试\n\n这是第一轮：先确认现状，再按计划推进。\n\n结束时交一份带证据的报告（用 goal_report：命令与退出码 / 改动的文件 / 勾选清单），没有证据不要声称完成。",
	}
}

// decodeResult parses what the tool returned to the model. The contract is JSON,
// so every assertion about the answer goes through this.
func decodeResult(t *testing.T, out string) reportResult {
	t.Helper()
	var r reportResult
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("the tool did not answer JSON: %q (%v)", out, err)
	}
	return r
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
	ctx := turnCtx(judgeGoal(), IntakeContext{})
	tl.SetGoalJudge(func(_ context.Context, input string) (string, bool) {
		if !strings.Contains(input, "所有测试通过") {
			t.Errorf("the judge did not receive the condition:\n%s", input)
		}
		return `{"verdict":"met","reason":"tests pass"}`, true
	})

	out, err := tl.Execute(ctx, map[string]any{
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
	res := decodeResult(t, out)
	if !res.Delivered || res.Verdict != "met" || res.Round != 3 || res.Verify {
		t.Errorf("the model was not told the verdict: %q", out)
	}
	if res.Reason != "tests pass" {
		t.Errorf("the reason was lost: %q", out)
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
	ctx := turnCtx(judgeGoal(), IntakeContext{})
	_, err := tl.Execute(ctx, map[string]any{
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
	ctx := turnCtx(judgeGoal(), IntakeContext{})
	tl.SetGoalJudge(func(context.Context, string) (string, bool) { return `{"verdict":"not_yet"}`, true })
	_, err := tl.Execute(ctx, map[string]any{
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
	ctx := turnCtx(judgeGoal(), IntakeContext{})
	tl.SetGoalJudge(func(context.Context, string) (string, bool) {
		judgements++
		return `{"verdict":"met"}`, true
	})
	params := map[string]any{
		"summary":  "done",
		"evidence": []any{map[string]any{"kind": "file", "path": "a.go"}},
	}
	if _, err := tl.Execute(ctx, params); err != nil {
		t.Fatalf("first: %v", err)
	}
	out, err := tl.Execute(ctx, params)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if posts != 1 || judgements != 1 {
		t.Fatalf("posts = %d, judgements = %d; want one of each", posts, judgements)
	}
	second := decodeResult(t, out)
	if second.Delivered || !strings.Contains(second.Reason, "已经提交") {
		t.Errorf("second call answered %q", out)
	}

	// A new round is a new turn, so the guard starts clean.
	gc := judgeGoal()
	gc.Round++
	if _, err := tl.Execute(turnCtx(gc, IntakeContext{}), params); err != nil {
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
	workCtx := turnCtx(work, IntakeContext{})
	if _, err := tl.Execute(workCtx, report); err != nil {
		t.Fatalf("work round: %v", err)
	}

	// The same round number in a new turn: the verification has its own binding,
	// so the work round's delivery must not silence it.
	verify := work
	verify.Verify = true
	verifyCtx := turnCtx(verify, IntakeContext{})
	if _, err := tl.Execute(verifyCtx, report); err != nil {
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
	ctx := turnCtx(judgeGoal(), IntakeContext{})
	report := map[string]any{"summary": "partly", "evidence": []any{map[string]any{"kind": "command", "cmd": "ls", "exit": float64(0)}}}
	if _, err := tl.Execute(ctx, report); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := tl.Execute(ctx, report); err != nil {
		t.Fatalf("second: %v", err)
	}
	if posts != 1 {
		t.Fatalf("a second report in the same round was delivered: posts=%d", posts)
	}
}

// TestGoalReport_History is the judge's memory: the earlier rounds the scheduler
// attached arrive as RECENT, and a round without a reason still appears.
func TestGoalReport_History(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var judged string
	tl := NewGoalReportTool(GoalReportOptions{WorkspaceURL: srv.URL, Token: "t"})
	gc := judgeGoal()
	gc.History = []RoundNote{
		{N: 0, Verdict: "not_yet", Reason: "refresh.go left"},
		{N: 1, Verdict: "not_yet"},
	}
	tl.SetGoalJudge(func(_ context.Context, input string) (string, bool) {
		judged = input
		return `{"verdict":"not_yet","reason":"middleware.go left"}`, true
	})
	if _, err := tl.Execute(turnCtx(gc, IntakeContext{}), map[string]any{
		"summary":  "migrated two call sites",
		"evidence": []any{map[string]any{"kind": "file", "path": "internal/auth/token.go"}},
	}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	for _, want := range []string{"RECENT", "round 1: not_yet — refresh.go left", "round 2: not_yet — no reason recorded"} {
		if !strings.Contains(judged, want) {
			t.Errorf("the judge input is missing %q:\n%s", want, judged)
		}
	}
	if strings.Contains(judged, "no earlier rounds are included") {
		t.Errorf("the placeholder survived a round with history:\n%s", judged)
	}
}

// TestGoalReport_FirstRoundTellsTheJudgeSo keeps the first round from inventing
// history: the placeholder is what the prompt calls an empty RECENT block.
func TestGoalReport_FirstRoundTellsTheJudgeSo(t *testing.T) {
	input := judgeInput(judgeGoal(), Report{Summary: "looked around"})
	if !strings.Contains(input, "(no earlier rounds)") {
		t.Errorf("the first round's RECENT is not marked empty:\n%s", input)
	}
}

// TestSplitInstruction_Briefing covers the headings the scheduler writes today:
// the judge must still receive the condition alone and the constraints as a list.
func TestSplitInstruction_Briefing(t *testing.T) {
	cond, cons := splitInstruction(judgeGoal().Instruction)
	if cond != "所有测试通过" {
		t.Errorf("condition = %q", cond)
	}
	if !strings.Contains(cons, "不改已有测试") || strings.Contains(cons, "结束时") {
		t.Errorf("constraints = %q", cons)
	}

	// A briefing without boundaries still yields the condition, not the whole
	// instruction.
	noCons := "【长程目标 · 第 2 轮】\n\n目标\nall green\n\n这是第一轮。\n"
	cond, cons = splitInstruction(noCons)
	if cond != "all green" {
		t.Errorf("condition without boundaries = %q", cond)
	}
	if cons != "(none)" {
		t.Errorf("constraints without boundaries = %q", cons)
	}
}
