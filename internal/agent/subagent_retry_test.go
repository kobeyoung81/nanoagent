package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"gitee.com/kobeyoung81/nanosec/internal/provider"
	"gitee.com/kobeyoung81/nanosec/internal/tool"
)

// fastRetry removes the pause before a retried completion and restores it.
func fastRetry(t *testing.T) {
	t.Helper()
	prev := subagentRetryDelay
	subagentRetryDelay = 0
	t.Cleanup(func() { subagentRetryDelay = prev })
}

// TestSubagent_RetriesGatewayFailureOnce pins the retry that the transcript
// demanded: a 502 (or an HTML error page where JSON was expected) used to throw
// away everything a subagent had done, including the minutes it had spent.
func TestSubagent_RetriesGatewayFailureOnce(t *testing.T) {
	fastRetry(t)
	mockProvider := provider.NewMock()
	calls := 0
	mockProvider.ResponseFunc = func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		calls++
		if calls == 1 {
			return nil, fmt.Errorf("openai chat stream: error, status code: 502, message: Bad Gateway")
		}
		return &provider.ChatResponse{Content: "recovered"}, nil
	}

	mgr := NewSubagentManager(
		SubagentConfig{MaxIterations: 3, Workspace: t.TempDir()},
		map[string]SubagentProfile{"test": {MaxIterations: 3}},
		nil,
		mockProvider,
		&MockBusClient{},
		"test-user",
	)

	res, err := mgr.execute(context.Background(), SubagentProfile{MaxIterations: 3}, tool.CallerPolicy{}, "go", noSubagentProgress)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res != "recovered" {
		t.Errorf("unexpected result %q", res)
	}
	if calls != 2 {
		t.Fatalf("expected 2 LLM calls (1 + retry), got %d", calls)
	}
}

// TestSubagent_RetriesHtmlErrorPage pins the other gateway shape: a JSON decode
// failure caused by an HTML page.
func TestSubagent_RetriesHtmlErrorPage(t *testing.T) {
	fastRetry(t)
	mockProvider := provider.NewMock()
	calls := 0
	mockProvider.ResponseFunc = func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		calls++
		if calls == 1 {
			return nil, fmt.Errorf("invalid character '<' looking for beginning of value")
		}
		return &provider.ChatResponse{Content: "ok"}, nil
	}

	mgr := NewSubagentManager(
		SubagentConfig{MaxIterations: 3, Workspace: t.TempDir()},
		map[string]SubagentProfile{"test": {MaxIterations: 3}},
		nil,
		mockProvider,
		&MockBusClient{},
		"test-user",
	)

	if _, err := mgr.execute(context.Background(), SubagentProfile{MaxIterations: 3}, tool.CallerPolicy{}, "go", noSubagentProgress); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected the HTML decode failure to be retried, got %d calls", calls)
	}
}

// TestSubagent_DoesNotRetryFatalErrors pins the other half: a decision (context
// overflow, denied tool, maxed iterations) is not a blip, and retrying it only
// spends time.
func TestSubagent_DoesNotRetryFatalErrors(t *testing.T) {
	fastRetry(t)
	for _, msg := range []string{
		"context_length_exceeded: maximum context length is 131072 tokens",
		`"write" is denied by permission policy.`,
	} {
		mockProvider := provider.NewMock()
		calls := 0
		mockProvider.ResponseFunc = func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
			calls++
			return nil, fmt.Errorf("%s", msg)
		}
		mgr := NewSubagentManager(
			SubagentConfig{MaxIterations: 3, Workspace: t.TempDir()},
			map[string]SubagentProfile{"test": {MaxIterations: 3}},
			nil,
			mockProvider,
			&MockBusClient{},
			"test-user",
		)
		if _, err := mgr.execute(context.Background(), SubagentProfile{MaxIterations: 3}, tool.CallerPolicy{}, "go", noSubagentProgress); err == nil {
			t.Fatalf("expected an error for %q", msg)
		}
		if calls != 1 {
			t.Errorf("%q must not be retried, got %d calls", msg, calls)
		}
	}
}

// TestSubagent_RunEnforcesBudget pins the overall bound a single foreground
// delegation used to lack (one call was measured taking 49 minutes).
func TestSubagent_RunEnforcesBudget(t *testing.T) {
	mockProvider := provider.NewMock()
	mockProvider.ResponseFunc = func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}

	mgr := NewSubagentManager(
		SubagentConfig{MaxIterations: 3, Workspace: t.TempDir(), BatchTimeout: 50 * time.Millisecond},
		map[string]SubagentProfile{"test": {MaxIterations: 3}},
		nil,
		mockProvider,
		&MockBusClient{},
		"test-user",
	)

	_, err := mgr.Run(context.Background(), tool.CallerPolicy{},
		tool.TaskRequest{SubagentType: "test", Description: "slow", Prompt: "go"}, "", "")
	if err == nil {
		t.Fatal("expected the budget to stop the subagent")
	}
	if !strings.Contains(err.Error(), "exceeded its time budget") {
		t.Errorf("unexpected error: %v", err)
	}
}
