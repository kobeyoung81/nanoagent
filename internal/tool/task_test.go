package tool

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// fakeTaskRunner records the single-task calls the task tools make.
type fakeTaskRunner struct {
	runTypes []string
	err      error
}

func (f *fakeTaskRunner) Run(_ context.Context, _ CallerPolicy, req TaskRequest, channel, chatID string) (string, error) {
	f.runTypes = append(f.runTypes, req.SubagentType)
	if f.err != nil {
		return "", f.err
	}
	return "ran " + req.SubagentType + ": " + req.Prompt, nil
}

func TestTaskTool_SingleForegroundRendersOneBlock(t *testing.T) {
	runner := &fakeTaskRunner{}
	tool := NewTaskTool(runner)

	out, err := tool.Execute(context.Background(), map[string]any{
		"description":   "check loops",
		"prompt":        "find the loops",
		"subagent_type": "explore",
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, "<task_result>") || strings.Contains(out, "<task_batch") {
		t.Errorf("a task call renders one <task> block, got: %q", out)
	}
	if !strings.Contains(out, `elapsed_ms="`) {
		t.Errorf("the result should carry how long it took, got: %q", out)
	}
	if len(runner.runTypes) != 1 || runner.runTypes[0] != "explore" {
		t.Errorf("runner saw %v, want [explore]", runner.runTypes)
	}
}

// TestTaskTool_ErrorStillCarriesElapsed pins that a failed delegation reports
// both the failure and the time it burned before failing.
func TestTaskTool_ErrorStillCarriesElapsed(t *testing.T) {
	runner := &fakeTaskRunner{err: fmt.Errorf("subagent exceeded its time budget (10m0s)")}
	tool := NewTaskTool(runner)

	out, err := tool.Execute(context.Background(), map[string]any{"prompt": "go"})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, "<task_error>") || !strings.Contains(out, "time budget") {
		t.Errorf("expected a task_error block carrying the failure, got: %q", out)
	}
	if !strings.Contains(out, `elapsed_ms="`) {
		t.Errorf("expected an elapsed time, got: %q", out)
	}
}

// TestTaskTool_HasNoBatchOrBackgroundParams pins the contract change: a task is
// one subagent, concurrency goes through parallel_execution.
func TestTaskTool_HasNoBatchOrBackgroundParams(t *testing.T) {
	params := NewTaskTool(&fakeTaskRunner{}).Parameters()
	for _, gone := range []string{"tasks", "background"} {
		if _, ok := params.Properties[gone]; ok {
			t.Errorf("%q parameter should be gone", gone)
		}
	}
	if _, ok := params.Properties["prompt"]; !ok {
		t.Error("prompt parameter missing")
	}
	if strings.Contains(NewTaskTool(&fakeTaskRunner{}).Description(), "tasks[]") {
		t.Error("the description still advertises tasks[]")
	}
}

func TestTaskTool_RejectsEmptyPrompt(t *testing.T) {
	runner := &fakeTaskRunner{}
	tool := NewTaskTool(runner)

	if _, err := tool.Execute(context.Background(), map[string]any{"description": "x"}); err == nil {
		t.Error("expected a missing-prompt error")
	}
	if _, err := tool.Execute(context.Background(), map[string]any{"prompt": "   "}); err == nil {
		t.Error("expected a blank-prompt error")
	}
	if len(runner.runTypes) != 0 {
		t.Errorf("runner must not be called, got %v", runner.runTypes)
	}
}

func TestTaskTool_SubagentTypeDefaultsToGeneral(t *testing.T) {
	runner := &fakeTaskRunner{}
	tool := NewTaskTool(runner)

	if _, err := tool.Execute(context.Background(), map[string]any{"prompt": "do it"}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(runner.runTypes) != 1 || runner.runTypes[0] != "general" {
		t.Errorf("runner saw %v, want [general]", runner.runTypes)
	}
}

func TestTaskTool_DescriptionFallsBackToPrompt(t *testing.T) {
	runner := &fakeTaskRunner{}
	tool := NewTaskTool(runner)

	out, err := tool.Execute(context.Background(), map[string]any{"prompt": "summarise the retry logic"})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, "<summary>summarise the retry logic</summary>") {
		t.Errorf("expected the prompt to supply the summary, got: %q", out)
	}
}

func TestAttenuateEnabled(t *testing.T) {
	enabled := map[string]bool{"read": true, "write": true, "bash": true}

	if got := AttenuateEnabled(enabled, nil); len(got) != 3 {
		t.Errorf("an unrestricted caller must not narrow the tools: %v", got)
	}
	got := AttenuateEnabled(enabled, map[string]bool{"read": true})
	if len(got) != 1 || !got["read"] {
		t.Errorf("intersection = %v, want only read", got)
	}
	// A profile without an allowlist gets everything, so the caller's set is the
	// bound.
	got = AttenuateEnabled(nil, map[string]bool{"read": true, "grep": true})
	if len(got) != 2 || !got["grep"] {
		t.Errorf("nil enabled must bound to the caller's set, got %v", got)
	}
	// The caller's set never adds tools the profile did not have.
	got = AttenuateEnabled(map[string]bool{"read": true}, map[string]bool{"read": true, "bash": true})
	if len(got) != 1 || !got["read"] {
		t.Errorf("attenuation must not widen the profile, got %v", got)
	}
}
