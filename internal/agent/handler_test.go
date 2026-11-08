package agent

import (
	"context"
	"testing"
	"time"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanosec/internal/provider"
	"gitee.com/kobeyoung81/nanosec/internal/tool"
)

// buildTestAgent constructs a real Agent for handler tests.
func buildTestAgent(t *testing.T, record nanoprotocol.AgentRecord, mock *mockBusClient, llm provider.Provider) *Agent {
	t.Helper()
	cfg := Config{
		UserID:        "user-1",
		AgentID:       record.ID,
		Model:         "test-model",
		MaxIterations: 5,
		SessionDir:    t.TempDir(),
	}
	ag, err := New(cfg, mock, llm, tool.NewRegistry())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return ag
}

func TestNewAgentHandler_Defaults(t *testing.T) {
	mock := newMockBusClient()
	record := nanoprotocol.AgentRecord{
		ID:                    "agent-1",
		DisplayName:           "Test Agent",
		ModelOverride:         "claude-sonnet",
		MaxIterations:         10,
		MaxConcurrentMessages: 0, // Should default to 4
	}
	ag := buildTestAgent(t, record, mock, provider.NewMock())

	h := NewAgentHandler(record, ag)
	if h.ID != "agent-1" {
		t.Fatalf("expected agent-1, got %s", h.ID)
	}
	if h.Config.MaxConcurrentMessages != 4 {
		t.Fatalf("expected default 4 concurrent messages, got %d", h.Config.MaxConcurrentMessages)
	}
	if cap(h.semaphore) != 4 {
		t.Fatalf("expected semaphore capacity 4, got %d", cap(h.semaphore))
	}
}

func TestNewAgentHandler_ConfiguredConcurrency(t *testing.T) {
	mock := newMockBusClient()
	record := nanoprotocol.AgentRecord{
		ID:                    "agent-2",
		MaxConcurrentMessages: 8,
	}
	ag := buildTestAgent(t, record, mock, provider.NewMock())

	h := NewAgentHandler(record, ag)
	if h.Config.MaxConcurrentMessages != 8 {
		t.Fatalf("expected 8 concurrent, got %d", h.Config.MaxConcurrentMessages)
	}
	if cap(h.semaphore) != 8 {
		t.Fatalf("expected semaphore capacity 8, got %d", cap(h.semaphore))
	}
}

func TestProcessMessage_LLMResponse(t *testing.T) {
	mock := newMockBusClient()
	llm := provider.NewMock()
	llm.Response = &provider.ChatResponse{Content: "Hello there!"}

	record := nanoprotocol.AgentRecord{
		ID:            "agent-1",
		MaxIterations: 10,
	}
	ag := buildTestAgent(t, record, mock, llm)
	h := NewAgentHandler(record, ag)

	msg := &nanoprotocol.InboundMessage{
		UserID:    "user-1",
		BindingID: 1,
		AgentID:   "agent-1",
		Channel:   "web",
		ChatID:    "chat-1",
		MessageID: "msg-1",
		Content:   "Hello world",
	}

	if err := h.processMessage(context.Background(), msg); err != nil {
		t.Fatalf("processMessage: %v", err)
	}

	if len(mock.completions) != 1 {
		t.Fatalf("expected 1 outbound complete, got %d", len(mock.completions))
	}
	out := mock.completions[0]
	if out.FullContent != "Hello there!" {
		t.Fatalf("unexpected content: %q", out.FullContent)
	}
	if out.AgentID != "agent-1" {
		t.Fatalf("expected agent-1, got %s", out.AgentID)
	}
	if out.UserID != "user-1" {
		t.Fatalf("expected user-1, got %s", out.UserID)
	}
}

func TestProcessMessageWithLimit_AcquiresSemaphore(t *testing.T) {
	mock := newMockBusClient()
	record := nanoprotocol.AgentRecord{
		ID:                    "agent-1",
		MaxConcurrentMessages: 1,
	}
	ag := buildTestAgent(t, record, mock, provider.NewMock())
	h := NewAgentHandler(record, ag)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	msg := &nanoprotocol.InboundMessage{
		UserID:    "user-1",
		MessageID: "msg-1",
		Content:   "test",
	}

	done := make(chan struct{})
	go func() {
		h.processMessageWithLimit(ctx, msg)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("first message blocked")
	}
}

func TestProcessMessageWithLimit_CancelledContext(t *testing.T) {
	mock := newMockBusClient()
	record := nanoprotocol.AgentRecord{
		ID:                    "agent-1",
		MaxConcurrentMessages: 1,
	}
	ag := buildTestAgent(t, record, mock, provider.NewMock())
	h := NewAgentHandler(record, ag)

	// Acquire the semaphore
	h.semaphore <- struct{}{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	msg := &nanoprotocol.InboundMessage{
		UserID:    "user-1",
		MessageID: "msg-1",
		Content:   "test",
	}

	done := make(chan struct{})
	go func() {
		h.processMessageWithLimit(ctx, msg)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("cancelled context should not block")
	}
}
