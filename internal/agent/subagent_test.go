package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"gitee.com/kobeyoung81/nanosec/internal/provider"
	"gitee.com/kobeyoung81/nanosec/internal/tool"
)

// TestSubagent_EmptyCompletionRetriesOnceThenFails verifies the subagent
// mirrors the main loop: an empty completion (no content, no tool calls) is
// retried once, and repeated emptiness fails the subagent instead of
// announcing an empty result.
func TestSubagent_EmptyCompletionRetriesOnceThenFails(t *testing.T) {
	mockProvider := provider.NewMock()
	callCount := 0
	mockProvider.ResponseFunc = func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		callCount++
		return &provider.ChatResponse{Content: ""}, nil
	}

	mgr := NewSubagentManager(
		SubagentConfig{MaxIterations: 5, Workspace: t.TempDir()},
		map[string]SubagentProfile{"test": {MaxIterations: 5}},
		nil,
		mockProvider,
		&MockBusClient{},
		"test-user",
	)

	_, err := mgr.execute(context.Background(), SubagentProfile{MaxIterations: 5}, tool.CallerPolicy{}, "do something", noSubagentProgress)
	if err == nil {
		t.Fatal("expected an error for repeated empty completions")
	}
	if !strings.Contains(err.Error(), "empty completion from provider after retry") {
		t.Errorf("unexpected error: %v", err)
	}
	if callCount != 2 {
		t.Fatalf("expected exactly 2 LLM calls (1 + retry), got %d", callCount)
	}
}

// TestSubagent_EmptyCompletionThenSuccess verifies the single retry can
// recover a healthy subagent result after one empty completion.
func TestSubagent_EmptyCompletionThenSuccess(t *testing.T) {
	mockProvider := provider.NewMock()
	callCount := 0
	mockProvider.ResponseFunc = func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		callCount++
		if callCount == 1 {
			return &provider.ChatResponse{Content: ""}, nil
		}
		return &provider.ChatResponse{Content: "result"}, nil
	}

	mgr := NewSubagentManager(
		SubagentConfig{MaxIterations: 5, Workspace: t.TempDir()},
		map[string]SubagentProfile{"test": {MaxIterations: 5}},
		nil,
		mockProvider,
		&MockBusClient{},
		"test-user",
	)

	res, err := mgr.execute(context.Background(), SubagentProfile{MaxIterations: 5}, tool.CallerPolicy{}, "do something", noSubagentProgress)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res != "result" {
		t.Errorf("unexpected result %q", res)
	}
	if callCount != 2 {
		t.Fatalf("expected 2 LLM calls, got %d", callCount)
	}
}

// TestSubagent_TruncatesToolResult verifies a subagent tool result is capped by
// the manager's MaxToolResultChars when the profile does not override it.
func TestSubagent_TruncatesToolResult(t *testing.T) {
	mockProvider := provider.NewMock()
	callCount := 0
	mockProvider.ResponseFunc = func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		callCount++
		if callCount == 1 {
			return &provider.ChatResponse{ToolCalls: []provider.ToolCall{{ID: "c1", Name: "big", Arguments: "{}"}}}, nil
		}
		return &provider.ChatResponse{Content: "done"}, nil
	}

	factory := func(p SubagentProfile, _ tool.CallerPolicy) *tool.Registry {
		reg := tool.NewRegistry()
		reg.Register(&MockTool{NameVal: "big", ExecuteFunc: func(ctx context.Context, params map[string]any) (string, error) {
			return strings.Repeat("x", 100), nil
		}})
		return reg
	}

	mgr := NewSubagentManager(
		SubagentConfig{MaxIterations: 5, Workspace: t.TempDir(), MaxToolResultChars: 20},
		map[string]SubagentProfile{"test": {MaxIterations: 5}},
		factory,
		mockProvider,
		&MockBusClient{},
		"test-user",
	)

	if _, err := mgr.execute(context.Background(), SubagentProfile{MaxIterations: 5}, tool.CallerPolicy{}, "go", noSubagentProgress); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if callCount != 2 {
		t.Fatalf("expected 2 LLM calls, got %d", callCount)
	}
	var toolMsg string
	for _, m := range mockProvider.Calls[1].Messages {
		if m.Role == "tool" {
			toolMsg = m.Content
		}
	}
	if !strings.Contains(toolMsg, "truncated") {
		t.Fatalf("tool result was not truncated: %q", toolMsg)
	}
	if strings.Contains(toolMsg, strings.Repeat("x", 30)) {
		t.Fatalf("tool result kept too much content: %q", toolMsg)
	}
}

// TestSubagent_FailedToolStillAppendsResult verifies a tool execution error is
// answered with a "tool" message, so the subagent's next request is not
// rejected with "tool_calls must be followed by tool messages".
func TestSubagent_FailedToolStillAppendsResult(t *testing.T) {
	mockProvider := provider.NewMock()
	callCount := 0
	mockProvider.ResponseFunc = func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		callCount++
		if callCount == 1 {
			return &provider.ChatResponse{ToolCalls: []provider.ToolCall{{ID: "c1", Name: "boom", Arguments: "{}"}}}, nil
		}
		return &provider.ChatResponse{Content: "done"}, nil
	}

	factory := func(p SubagentProfile, _ tool.CallerPolicy) *tool.Registry {
		reg := tool.NewRegistry()
		reg.Register(&MockTool{NameVal: "boom", ExecuteFunc: func(ctx context.Context, params map[string]any) (string, error) {
			return "", fmt.Errorf("kaboom")
		}})
		return reg
	}

	mgr := NewSubagentManager(
		SubagentConfig{MaxIterations: 5, Workspace: t.TempDir()},
		map[string]SubagentProfile{"test": {MaxIterations: 5}},
		factory,
		mockProvider,
		&MockBusClient{},
		"test-user",
	)

	res, err := mgr.execute(context.Background(), SubagentProfile{MaxIterations: 5}, tool.CallerPolicy{}, "go", noSubagentProgress)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res != "done" {
		t.Fatalf("unexpected result %q", res)
	}
	if callCount != 2 {
		t.Fatalf("expected 2 LLM calls, got %d", callCount)
	}
	var toolMsg *provider.Message
	for i := range mockProvider.Calls[1].Messages {
		m := &mockProvider.Calls[1].Messages[i]
		if m.Role == "tool" && m.ToolCallID == "c1" {
			toolMsg = m
		}
	}
	if toolMsg == nil {
		t.Fatalf("no tool result appended for failed call: %+v", mockProvider.Calls[1].Messages)
	}
	if !strings.Contains(toolMsg.Content, "kaboom") {
		t.Fatalf("tool result did not carry the error: %q", toolMsg.Content)
	}
}

// noSubagentProgress is the no-op progress sink used by the subagent tests.
func noSubagentProgress(string, string) {}
