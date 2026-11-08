package provider

import (
	"context"
	"testing"
)

func TestMockProvider(t *testing.T) {
	mock := NewMock()
	mock.Response = &ChatResponse{Content: "hello"}

	req := ChatRequest{
		Model: "test-model",
		Messages: []Message{
			{Role: "user", Content: "hi"},
		},
	}

	resp, err := mock.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}

	if resp.Content != "hello" {
		t.Errorf("Expected 'hello', got '%s'", resp.Content)
	}

	if len(mock.Calls) != 1 {
		t.Errorf("Expected 1 call, got %d", len(mock.Calls))
	}
	if mock.Calls[0].Model != "test-model" {
		t.Errorf("Expected model 'test-model', got '%s'", mock.Calls[0].Model)
	}
}

func TestMockProvider_ResponseFunc(t *testing.T) {
	mock := NewMock()
	mock.ResponseFunc = func(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
		return &ChatResponse{Content: "dynamic " + req.Messages[0].Content}, nil
	}

	req := ChatRequest{
		Messages: []Message{
			{Role: "user", Content: "world"},
		},
	}

	resp, err := mock.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}

	if resp.Content != "dynamic world" {
		t.Errorf("Expected 'dynamic world', got '%s'", resp.Content)
	}
}
