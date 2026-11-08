package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlanWriteTool_WritesUnderPlansDir(t *testing.T) {
	ws := t.TempDir()
	tool := NewPlanWriteTool(ws, "")
	out, err := tool.Execute(context.Background(), map[string]any{
		"slug":    "Fix Session/Split!",
		"title":   "Fix the session split",
		"content": "## Steps\n1. do a thing",
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	path := PlanPathFromResult(out)
	if path == "" {
		t.Fatalf("result carries no path: %q", out)
	}
	want := filepath.Join(ws, "plans", "fix-session-split.md")
	if path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read plan: %v", err)
	}
	if !strings.HasPrefix(string(data), "# Fix the session split\n\n## Steps") {
		t.Errorf("unexpected plan body: %q", string(data))
	}
}

func TestPlanWriteTool_RejectsTraversalAndEmptyContent(t *testing.T) {
	ws := t.TempDir()
	tool := NewPlanWriteTool(ws, "")

	// A hostile slug cannot escape the plans directory.
	out, err := tool.Execute(context.Background(), map[string]any{
		"slug":    "../../etc/passwd",
		"content": "x",
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	path := PlanPathFromResult(out)
	if !strings.HasPrefix(path, filepath.Join(ws, "plans")+string(os.PathSeparator)) {
		t.Fatalf("plan escaped the plans dir: %q", path)
	}

	if _, err := tool.Execute(context.Background(), map[string]any{"content": "   "}); err == nil {
		t.Error("expected an error for empty content")
	}
}

func TestPlanWriteTool_TimestampSlugFallback(t *testing.T) {
	ws := t.TempDir()
	tool := NewPlanWriteTool(ws, "")
	out, err := tool.Execute(context.Background(), map[string]any{"content": "plan"})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if path := PlanPathFromResult(out); !strings.HasPrefix(filepath.Base(path), "plan-") {
		t.Errorf("expected a timestamped fallback slug, got %q", path)
	}
}

func TestPlanPathFromResult_IgnoresOtherResults(t *testing.T) {
	if got := PlanPathFromResult("Error: something failed"); got != "" {
		t.Errorf("expected empty path, got %q", got)
	}
}
