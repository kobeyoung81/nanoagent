package tool

import (
	"context"
	"strings"
	"testing"
)

func TestReadOnlyTaskTool_AllowsReadOnlyTypes(t *testing.T) {
	runner := &fakeTaskRunner{}
	tool := NewReadOnlyTaskTool(runner, []string{"explore", "scout"})

	out, err := tool.Execute(context.Background(), map[string]any{
		"description":   "find loops",
		"prompt":        "find the loops",
		"subagent_type": "explore",
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, "<task_result>") {
		t.Errorf("unexpected result: %q", out)
	}
	if len(runner.runTypes) != 1 || runner.runTypes[0] != "explore" {
		t.Errorf("runner saw %v, want [explore]", runner.runTypes)
	}
}

func TestReadOnlyTaskTool_RefusesWriteCapableTypes(t *testing.T) {
	runner := &fakeTaskRunner{}
	tool := NewReadOnlyTaskTool(runner, []string{"explore", "scout"})

	_, err := tool.Execute(context.Background(), map[string]any{
		"description":   "refactor",
		"prompt":        "refactor everything",
		"subagent_type": "general",
	})
	if err == nil {
		t.Fatal("expected general to be refused")
	}
	if !strings.Contains(err.Error(), `"general"`) || !strings.Contains(err.Error(), "explore, scout") {
		t.Errorf("error should name the refused type and the alternatives, got: %v", err)
	}
	if len(runner.runTypes) != 0 {
		t.Errorf("runner must not be called, got %v", runner.runTypes)
	}
}

func TestReadOnlyTaskTool_AdvertisesOnlyAllowedTypes(t *testing.T) {
	tool := NewReadOnlyTaskTool(&fakeTaskRunner{}, []string{"explore", "scout"})

	if got := tool.Name(); got != "readonly_task" {
		t.Errorf("Name() = %q, want readonly_task", got)
	}
	desc := tool.Description()
	if !strings.Contains(desc, "explore, scout") || strings.Contains(desc, "general") {
		t.Errorf("description should advertise only read-only types, got: %q", desc)
	}
	params := tool.Parameters()
	if got := params.Properties["subagent_type"].Description; !strings.Contains(got, `"explore"`) || !strings.Contains(got, `"scout"`) {
		t.Errorf("subagent_type description should list allowed types, got: %q", got)
	}
	if _, ok := params.Properties["prompt"]; !ok {
		t.Error("prompt parameter missing")
	}
	// Concurrency belongs to parallel_execution now: no batch or background
	// parameter survives here.
	if _, ok := params.Properties["tasks"]; ok {
		t.Error("tasks parameter should be gone")
	}
	if _, ok := params.Properties["background"]; ok {
		t.Error("background parameter should be gone")
	}
}
