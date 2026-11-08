package provider

import (
	"context"
	"sync"
)

// MockProvider is a mock implementation of the Provider interface for testing.
type MockProvider struct {
	mu           sync.Mutex
	Response     *ChatResponse
	Err          error
	Calls        []ChatRequest
	ResponseFunc func(ctx context.Context, req ChatRequest) (*ChatResponse, error)
	StreamFunc   func(ctx context.Context, req ChatRequest) (<-chan ChatStreamEvent, error)
}

func NewMock() *MockProvider {
	return &MockProvider{
		Calls: make([]ChatRequest, 0),
	}
}

func (m *MockProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.Calls = append(m.Calls, req)

	if m.ResponseFunc != nil {
		return m.ResponseFunc(ctx, req)
	}

	if m.Err != nil {
		return nil, m.Err
	}

	if m.Response != nil {
		return m.Response, nil
	}

	return &ChatResponse{Content: "mock response"}, nil
}

// ChatStream streams a completion. When StreamFunc is unset it synthesizes a
// single Done event from Chat so callers can exercise the streaming path in
// tests without a real provider.
func (m *MockProvider) ChatStream(ctx context.Context, req ChatRequest) (<-chan ChatStreamEvent, error) {
	if m.StreamFunc != nil {
		return m.StreamFunc(ctx, req)
	}
	resp, err := m.Chat(ctx, req)
	ch := make(chan ChatStreamEvent, 1)
	if err != nil {
		ch <- ChatStreamEvent{Err: err}
		close(ch)
		return ch, nil
	}
	ch <- ChatStreamEvent{Done: true, FullContent: resp.Content, ToolCalls: resp.ToolCalls, Usage: resp.Usage, FinishReason: resp.FinishReason}
	close(ch)
	return ch, nil
}
