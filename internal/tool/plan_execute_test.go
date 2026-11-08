package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlanExecuteTool_AppendsReportNextToPlan(t *testing.T) {
	ws := t.TempDir()
	writeTool := NewPlanWriteTool(ws, "")
	out, err := writeTool.Execute(context.Background(), map[string]any{
		"slug": "fix-session-split", "content": "## Steps\n1. do a thing",
	})
	if err != nil {
		t.Fatalf("plan_write: %v", err)
	}
	planPath := PlanPathFromResult(out)

	execTool := NewPlanExecuteTool(ws, "")
	execTool.SetPlanPath(planPath)
	result, err := execTool.Execute(context.Background(), map[string]any{
		"status":  "partial",
		"summary": "split moved to the next turn; stray master.yml left alone",
		"report":  "ran: go test ./internal/agent/... — ok",
	})
	if err != nil {
		t.Fatalf("plan_execute: %v", err)
	}
	if got := PlanExecutedPath(result); got != planPath {
		t.Fatalf("PlanExecutedPath = %q, want %q (result %q)", got, planPath, result)
	}

	report := planPath[:len(planPath)-len(".md")] + ".report.md"
	data, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, "partial") || !strings.Contains(body, "split moved to the next turn") {
		t.Errorf("report missing the outcome: %q", body)
	}
	if !strings.Contains(body, "go test ./internal/agent/...") {
		t.Errorf("report missing the evidence: %q", body)
	}
	// The plan itself stays the original contract.
	plan, _ := os.ReadFile(planPath)
	if strings.Contains(string(plan), "partial") {
		t.Errorf("plan was mutated: %q", string(plan))
	}

	// A second execution accumulates instead of overwriting.
	if _, err := execTool.Execute(context.Background(), map[string]any{
		"status": "done", "summary": "follow-up finished",
	}); err != nil {
		t.Fatalf("second plan_execute: %v", err)
	}
	data, _ = os.ReadFile(report)
	if !strings.Contains(string(data), "done") || !strings.Contains(string(data), "split moved to the next turn") {
		t.Errorf("second report did not append: %q", string(data))
	}
}

func TestPlanExecuteTool_RejectsWithoutAPlan(t *testing.T) {
	execTool := NewPlanExecuteTool(t.TempDir(), "")
	if _, err := execTool.Execute(context.Background(), map[string]any{
		"status": "done", "summary": "nothing to close",
	}); err == nil {
		t.Error("expected an error when the session has no plan artefact")
	}
}

func TestPlanExecuteTool_RejectsBadStatusAndMissingPlan(t *testing.T) {
	ws := t.TempDir()
	execTool := NewPlanExecuteTool(ws, "")
	execTool.SetPlanPath(filepath.Join(ws, "plans", "gone.md"))

	if _, err := execTool.Execute(context.Background(), map[string]any{
		"status": "finished", "summary": "x",
	}); err == nil {
		t.Error("expected an error for an unknown status")
	}
	if _, err := execTool.Execute(context.Background(), map[string]any{
		"status": "done", "summary": "   ",
	}); err == nil {
		t.Error("expected an error for an empty summary")
	}
	if _, err := execTool.Execute(context.Background(), map[string]any{
		"status": "done", "summary": "x",
	}); err == nil {
		t.Error("expected an error when the plan file does not exist")
	}
}

// TestPlanExecuteTool_KeepsReportInsidePlansDir guards the confinement: the plan
// path comes from session metadata, but a report must never be written outside
// the plans directory it belongs to.
func TestPlanExecuteTool_KeepsReportInsidePlansDir(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "plans"), 0o755); err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(ws, "plans", "x.md")
	if err := os.WriteFile(planPath, []byte("plan"), 0o644); err != nil {
		t.Fatal(err)
	}
	execTool := NewPlanExecuteTool(ws, "")
	execTool.SetPlanPath(planPath)
	if _, err := execTool.Execute(context.Background(), map[string]any{
		"status": "done", "summary": "ok",
	}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, "plans", "x.report.md")); err != nil {
		t.Errorf("report not written next to the plan: %v", err)
	}
}

func TestPlanExecutedPath_IgnoresOtherResults(t *testing.T) {
	if got := PlanExecutedPath("Error: something failed"); got != "" {
		t.Errorf("expected empty path, got %q", got)
	}
}
