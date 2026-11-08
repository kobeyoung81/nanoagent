package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanosec/internal/provider"
	"gitee.com/kobeyoung81/nanosec/internal/tool"
)

// newWatchdogAgent builds a minimal agent whose LLM timeouts are set to the
// given values, for driving chatStreamTurn directly.
func newWatchdogAgent(t *testing.T, ttfb, idle time.Duration, p provider.Provider) *Agent {
	t.Helper()
	ag, err := New(Config{
		UserID:               "test-user",
		MaxIterations:        5,
		SessionDir:           t.TempDir(),
		LLMTtfbTimeout:       ttfb,
		LLMStreamIdleTimeout: idle,
	}, &MockBusClient{}, p, tool.NewRegistry())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return ag
}

func inboundMsg() *nanoprotocol.InboundMessage {
	return &nanoprotocol.InboundMessage{UserID: "test-user", SessionID: "test:watchdog", Content: "hi"}
}

// TestChatStreamTurn_TtfbToleratesSlowThinking is the core regression: a
// reasoning model may produce its first token AFTER llmStreamIdleTimeout, as
// long as it arrives before llmTtfbTimeout. The old watchdog armed the idle
// timer immediately and would abort here.
func TestChatStreamTurn_TtfbToleratesSlowThinking(t *testing.T) {
	mock := provider.NewMock()
	mock.StreamFunc = func(ctx context.Context, req provider.ChatRequest) (<-chan provider.ChatStreamEvent, error) {
		ch := make(chan provider.ChatStreamEvent, 2)
		go func() {
			// First token arrives after the idle window (90ms) but well
			// before the TTFB window (1s).
			time.Sleep(120 * time.Millisecond)
			ch <- provider.ChatStreamEvent{Kind: "content", Delta: "hi"}
			ch <- provider.ChatStreamEvent{Done: true, FullContent: "hi"}
			close(ch)
		}()
		return ch, nil
	}

	ag := newWatchdogAgent(t, time.Second, 90*time.Millisecond, mock)
	resp, err := ag.chatStreamTurn(context.Background(), provider.ChatRequest{}, inboundMsg(), "m1")
	if err != nil {
		t.Fatalf("chatStreamTurn: %v", err)
	}
	if resp.Content != "hi" {
		t.Fatalf("content = %q, want %q", resp.Content, "hi")
	}
}

// TestChatStreamTurn_TtfbTimeout aborts when no event arrives within
// llmTtfbTimeout, classifying as an agent-side request timeout.
func TestChatStreamTurn_TtfbTimeout(t *testing.T) {
	mock := provider.NewMock()
	mock.StreamFunc = func(ctx context.Context, req provider.ChatRequest) (<-chan provider.ChatStreamEvent, error) {
		ch := make(chan provider.ChatStreamEvent, 1)
		go func() {
			// Never produce an event before the TTFB window elapses.
			time.Sleep(2 * time.Second)
			ch <- provider.ChatStreamEvent{Kind: "content", Delta: "late"}
			close(ch)
		}()
		return ch, nil
	}

	ag := newWatchdogAgent(t, 100*time.Millisecond, time.Second, mock)
	_, err := ag.chatStreamTurn(context.Background(), provider.ChatRequest{}, inboundMsg(), "m1")
	if err == nil {
		t.Fatal("expected a TTFB timeout error")
	}
	var rt provider.ErrRequestTimeout
	if !errors.As(err, &rt) {
		t.Fatalf("expected ErrRequestTimeout, got %T: %v", err, err)
	}
	if rt.Phase != "ttfb" {
		t.Fatalf("phase = %q, want ttfb", rt.Phase)
	}
}

// TestChatStreamTurn_IdleTimeoutAbortsAfterFirstByte keeps the existing
// post-first-byte stall behavior: once streaming has begun, a gap longer than
// llmStreamIdleTimeout aborts.
func TestChatStreamTurn_IdleTimeoutAbortsAfterFirstByte(t *testing.T) {
	mock := provider.NewMock()
	mock.StreamFunc = func(ctx context.Context, req provider.ChatRequest) (<-chan provider.ChatStreamEvent, error) {
		ch := make(chan provider.ChatStreamEvent, 2)
		go func() {
			ch <- provider.ChatStreamEvent{Kind: "content", Delta: "one"}
			// Stall long past the idle window (but within TTFB).
			time.Sleep(200 * time.Millisecond)
			ch <- provider.ChatStreamEvent{Kind: "content", Delta: "two"}
			close(ch)
		}()
		return ch, nil
	}

	ag := newWatchdogAgent(t, time.Second, 60*time.Millisecond, mock)
	_, err := ag.chatStreamTurn(context.Background(), provider.ChatRequest{}, inboundMsg(), "m1")
	if err == nil {
		t.Fatal("expected an idle timeout error")
	}
	var rt provider.ErrRequestTimeout
	if !errors.As(err, &rt) {
		t.Fatalf("expected ErrRequestTimeout, got %T: %v", err, err)
	}
	if rt.Phase != "idle" {
		t.Fatalf("phase = %q, want idle", rt.Phase)
	}
}
