package provider

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"testing"
	"time"

	"gitee.com/kobeyoung81/nanoprotocol"
	openai "github.com/sashabaranov/go-openai"
)

func TestClassifyProviderError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		category string
		want     string
	}{
		{
			name: "429 rate limited",
			err: &openai.APIError{
				HTTPStatusCode: 429,
				Message:        "rate limit exceeded",
			},
			want: nanoprotocol.TurnErrorRateLimited,
		},
		{
			name: "401 auth",
			err: &openai.APIError{
				HTTPStatusCode: 401,
				Message:        "invalid api key sk-secret",
			},
			want: nanoprotocol.TurnErrorProviderAuth,
		},
		{
			name: "404 model not found",
			err: &openai.APIError{
				HTTPStatusCode: 404,
				Message:        "The model `x` does not exist",
			},
			want: nanoprotocol.TurnErrorProviderNotFound,
		},
		{
			name: "502 provider unavailable",
			err: &openai.APIError{
				HTTPStatusCode: 502,
				Message:        "bad gateway",
			},
			want: nanoprotocol.TurnErrorProviderUnavailable,
		},
		{
			name: "context length",
			err: &openai.APIError{
				HTTPStatusCode: 400,
				Message:        "This model's maximum context length is 8192 tokens",
			},
			want: nanoprotocol.TurnErrorContextTooLong,
		},
		{
			name: "reasoning replay required",
			err: &openai.APIError{
				HTTPStatusCode: 400,
				Message:        "The `reasoning_content` in the thinking mode must be passed back to the API.",
			},
			want: nanoprotocol.TurnErrorInternal,
		},
		{
			name: "deadline exceeded",
			err:  fmt.Errorf("openai chat stream: %w", context.DeadlineExceeded),
			want: nanoprotocol.TurnErrorProviderUnavailable,
		},
		{
			name: "agent-side ttfb timeout",
			err:  ErrRequestTimeout{Phase: "ttfb", Duration: 300 * time.Second},
			want: nanoprotocol.TurnErrorTimeout,
		},
		{
			name: "agent-side idle timeout",
			err:  ErrRequestTimeout{Phase: "idle", Duration: 90 * time.Second},
			want: nanoprotocol.TurnErrorTimeout,
		},
		{
			name: "agent-side request timeout wrapped",
			err:  fmt.Errorf("openai chat: %w", ErrRequestTimeout{Phase: "request", Duration: 300 * time.Second}),
			want: nanoprotocol.TurnErrorTimeout,
		},
		{
			name: "connection refused",
			err:  &url.Error{Op: "Post", Err: errors.New("connection refused")},
			want: nanoprotocol.TurnErrorProviderUnavailable,
		},
		{
			name:     "override wins",
			err:      errors.New("whatever"),
			category: nanoprotocol.TurnErrorMaxIterations,
			want:     nanoprotocol.TurnErrorMaxIterations,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, detail := ClassifyProviderError(tt.err, tt.category)
			if got != tt.want {
				t.Fatalf("category = %q, want %q", got, tt.want)
			}
			if tt.want == nanoprotocol.TurnErrorMaxIterations {
				return
			}
			if detail == "" {
				t.Fatalf("expected non-empty detail")
			}
		})
	}
}

func TestClassifyProviderError_ReasoningReplayGuidance(t *testing.T) {
	_, detail := ClassifyProviderError(&openai.APIError{
		HTTPStatusCode: 400,
		Message:        "The `reasoning_content` in the thinking mode must be passed back to the API.",
	}, "")
	if !stringsContains(detail, "new session") {
		t.Fatalf("expected guidance to start a new session, got %q", detail)
	}
}

func TestSanitizeDetailStripsURLs(t *testing.T) {
	detail := sanitizeDetail("Post \"https://api.example.com/v1/chat?api_key=abc\": 502 Bad Gateway\nsecond line")
	if stringsContains(detail, "https://") || stringsContains(detail, "\n") || stringsContains(detail, "api_key") {
		t.Fatalf("detail leaks URL/secret: %q", detail)
	}
}

func TestRetryableTurnError(t *testing.T) {
	if !RetryableTurnError(nanoprotocol.TurnErrorRateLimited) {
		t.Fatal("rate_limited should be retryable")
	}
	if !RetryableTurnError(nanoprotocol.TurnErrorProviderUnavailable) {
		t.Fatal("provider_unavailable should be retryable")
	}
	if RetryableTurnError(nanoprotocol.TurnErrorTimeout) {
		t.Fatal("request_timeout must not retry (agent-side policy decision)")
	}
	if RetryableTurnError(nanoprotocol.TurnErrorProviderAuth) {
		t.Fatal("auth errors must not retry")
	}
	if RetryableTurnError(nanoprotocol.TurnErrorProviderNotFound) {
		t.Fatal("not-found must not retry")
	}
}

func TestIsRequestTimeout(t *testing.T) {
	if !IsRequestTimeout(ErrRequestTimeout{Phase: "ttfb"}) {
		t.Fatal("ErrRequestTimeout should be recognized")
	}
	if !IsRequestTimeout(fmt.Errorf("wrapped: %w", ErrRequestTimeout{Phase: "idle"})) {
		t.Fatal("wrapped ErrRequestTimeout should be recognized")
	}
	if IsRequestTimeout(context.DeadlineExceeded) {
		t.Fatal("raw DeadlineExceeded is not an agent-side request timeout")
	}
	if IsRequestTimeout(errors.New("boom")) {
		t.Fatal("unrelated error must not be recognized")
	}
}

func stringsContains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

// TestClassifyProviderError_ToolCallIncomplete pins the user-facing message for
// a tool call whose arguments are not usable JSON. The category is its own
// (tool_call_incomplete) rather than provider_unavailable: the model wrote those
// arguments, and by the time this surfaces the recovery already ran, so telling
// the user "the assistant service is temporarily unavailable" would be false.
func TestClassifyProviderError_ToolCallIncomplete(t *testing.T) {
	category, detail := ClassifyProviderError(&ErrToolCallIncomplete{
		Calls:        []IncompleteToolCall{{Index: 0, Tool: "write", Bytes: 21504, Kind: ToolCallKindTruncated}},
		FinishReason: "length",
	}, "")
	if category != nanoprotocol.TurnErrorToolCallIncomplete {
		t.Errorf("category = %q, want %q", category, nanoprotocol.TurnErrorToolCallIncomplete)
	}
	for _, want := range []string{"output limit", `finish_reason="length"`, `"write"`, "21504", "never executed"} {
		if !stringsContains(detail, want) {
			t.Errorf("detail %q should contain %q", detail, want)
		}
	}

	// A malformed document is a different failure and says so.
	_, detail = ClassifyProviderError(&ErrToolCallIncomplete{
		Calls:        []IncompleteToolCall{{Index: 0, Tool: "parallel_execution", Bytes: 616, Kind: ToolCallKindMalformed}},
		FinishReason: "tool_calls",
	}, "")
	if !stringsContains(detail, `"parallel_execution"`) || !stringsContains(detail, "invalid JSON") {
		t.Errorf("detail for a malformed document = %q", detail)
	}
	if !stringsContains(detail, "Retry") {
		t.Errorf("detail should stay actionable, got %q", detail)
	}
}

// TestRetryableTurnError_ToolCallIncompleteNotRetried pins that the category is
// not auto-retried: the turn already answered the bad call and let the model
// re-emit it, so retrying the whole request would repeat the same work.
func TestRetryableTurnError_ToolCallIncompleteNotRetried(t *testing.T) {
	if RetryableTurnError(nanoprotocol.TurnErrorToolCallIncomplete) {
		t.Error("tool_call_incomplete must not be auto-retried")
	}
}
