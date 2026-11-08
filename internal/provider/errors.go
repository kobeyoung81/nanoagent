package provider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"gitee.com/kobeyoung81/nanoprotocol"
	openai "github.com/sashabaranov/go-openai"
)

// ErrRequestTimeout marks a failure caused by the AGENT's own timeout policy
// (TTFB, stream idle, or non-streaming request deadline), as opposed to a
// failure reported by the upstream provider. It is classified as
// nanoprotocol.TurnErrorTimeout and is deliberately NOT auto-retried: the
// decision to abort was made locally, so retrying would repeat the same wait.
type ErrRequestTimeout struct {
	// Phase identifies which watchdog fired: "ttfb", "idle", or "request".
	Phase string
	// Duration is the timeout value that was exceeded, for diagnostics.
	Duration time.Duration
}

func (e ErrRequestTimeout) Error() string {
	switch e.Phase {
	case "ttfb":
		return fmt.Sprintf("llm stream timed out awaiting first token (ttfb %s)", e.Duration)
	case "idle":
		return fmt.Sprintf("llm stream idle for %s", e.Duration)
	case "request":
		return fmt.Sprintf("llm request timeout (after %s)", e.Duration)
	default:
		return "llm request timeout"
	}
}

// IsRequestTimeout reports whether err (or its wrapper chain) is an
// agent-side timeout, so classifiers and callers can distinguish it from
// upstream/network failures.
func IsRequestTimeout(err error) bool {
	var rt ErrRequestTimeout
	return errors.As(err, &rt)
}

// IncompleteToolCall is one tool call whose arguments were not usable JSON.
type IncompleteToolCall struct {
	// Index is the call's position in ChatResponse.ToolCalls.
	Index int
	// Tool is the tool the model tried to call.
	Tool string
	// Bytes is how many argument bytes the model produced.
	Bytes int
	// Kind is ToolCallKindTruncated (a cut-off document) or
	// ToolCallKindMalformed (a structurally broken one).
	Kind string
}

// ErrToolCallIncomplete reports tool calls whose arguments are not valid JSON,
// so they were never executed. The two shapes are different failures: a
// truncated document comes from the upstream stopping mid-generation, while a
// malformed one is a document the model finished writing and got wrong —
// writing a nested call's arguments as an escaped string is the usual cause.
//
// The calls are never persisted as the model wrote them. The loop answers each
// one with an error result, exactly like a tool that failed, so the model sees
// what it wrote and can re-emit it. This error surfaces only when that recovery
// is exhausted.
type ErrToolCallIncomplete struct {
	Calls        []IncompleteToolCall
	FinishReason string
}

func (e *ErrToolCallIncomplete) Error() string {
	if len(e.Calls) == 1 {
		c := e.Calls[0]
		return fmt.Sprintf("llm stream ended with unusable arguments for tool call %q (%d bytes, %s, finish_reason=%q)",
			c.Tool, c.Bytes, c.Kind, e.FinishReason)
	}
	return fmt.Sprintf("llm stream ended with unusable arguments for %d tool calls (finish_reason=%q)",
		len(e.Calls), e.FinishReason)
}

// Has reports whether the call at index i in ToolCalls was dropped.
func (e *ErrToolCallIncomplete) Has(i int) bool {
	for _, c := range e.Calls {
		if c.Index == i {
			return true
		}
	}
	return false
}

// UserDetail is the actionable line shown beside the failure category: the
// recovery already ran, so it says what the user can do that the model could not.
func (e *ErrToolCallIncomplete) UserDetail() string {
	c := e.Calls[0]
	what := "the model wrote invalid JSON for its arguments"
	switch {
	case c.Kind == ToolCallKindTruncated && e.FinishReason == "length":
		what = "the upstream stopped at its output limit mid-way"
	case c.Kind == ToolCallKindTruncated:
		what = "the arguments were cut off mid-way"
	case e.FinishReason == "length":
		what = "the upstream stopped at its output limit mid-way"
	}
	return fmt.Sprintf("the %q tool call's arguments were still unusable after the model re-emitted them (%s, %d bytes, finish_reason=%q); the call was never executed. Retry, or split this step into smaller writes.",
		c.Tool, what, c.Bytes, e.FinishReason)
}

// ClassifyProviderError maps a provider failure to a nanoprotocol TurnError*
// category plus a sanitized, human-readable detail line (never secrets or raw
// URLs). When category is non-empty it overrides classification; err may then
// be nil (e.g. max iterations).
func ClassifyProviderError(err error, category string) (string, string) {
	if category != "" {
		return category, sanitizeDetail(fmt.Sprint(err))
	}
	if err == nil {
		return nanoprotocol.TurnErrorInternal, ""
	}

	// Agent-side timeouts are a client condition, not an upstream outage:
	// distinct category and never auto-retried.
	if IsRequestTimeout(err) {
		return nanoprotocol.TurnErrorTimeout, sanitizeDetail(err.Error())
	}

	// Unusable tool-call arguments are not an outage: the model wrote them, so
	// say that, and say that the recovery already ran.
	var incomplete *ErrToolCallIncomplete
	if errors.As(err, &incomplete) && len(incomplete.Calls) > 0 {
		return nanoprotocol.TurnErrorToolCallIncomplete, sanitizeDetail(incomplete.UserDetail())
	}

	var apiErr *openai.APIError
	if errors.As(err, &apiErr) {
		detail := sanitizeDetail(apiErr.Message)
		switch {
		case apiErr.HTTPStatusCode == 429:
			return nanoprotocol.TurnErrorRateLimited, detail
		case apiErr.HTTPStatusCode == 401 || apiErr.HTTPStatusCode == 403:
			return nanoprotocol.TurnErrorProviderAuth, "credentials rejected by provider"
		case apiErr.HTTPStatusCode == 404:
			return nanoprotocol.TurnErrorProviderNotFound, detail
		case apiErr.HTTPStatusCode == 400 && isContextLength(apiErr.Message):
			return nanoprotocol.TurnErrorContextTooLong, detail
		case apiErr.HTTPStatusCode == 400 && isReasoningReplay(apiErr.Message):
			// Thinking models require prior reasoning_content to be echoed
			// back once tools are in play. Sessions persisted before that was
			// recorded cannot be repaired in place, so point the user at a
			// fresh session.
			return nanoprotocol.TurnErrorInternal, "This conversation predates reasoning replay. Start a new session with /new."
		case apiErr.HTTPStatusCode == 400 && isUnpairedToolCall(apiErr.Message):
			// An assistant tool call had no matching tool result. The
			// request-time sanitizer repairs such history, so this is a
			// defensive classification for shapes it did not catch.
			return nanoprotocol.TurnErrorInternal, "The conversation history was missing a tool result and has been repaired. Please retry."
		case apiErr.HTTPStatusCode >= 500:
			return nanoprotocol.TurnErrorProviderUnavailable, detail
		default:
			return nanoprotocol.TurnErrorInternal, detail
		}
	}

	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		// A deadline/cancellation originating from the agent's own timeout
		// policy surfaces as ErrRequestTimeout (classified above). A raw
		// context.DeadlineExceeded here is an upstream-side timeout and
		// stays categorized as provider-unavailable.
		return nanoprotocol.TurnErrorProviderUnavailable, "request timed out"
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return nanoprotocol.TurnErrorProviderUnavailable, sanitizeDetail(err.Error())
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return nanoprotocol.TurnErrorProviderUnavailable, sanitizeDetail(err.Error())
	}
	return nanoprotocol.TurnErrorProviderUnavailable, sanitizeDetail(err.Error())
}

// RetryableTurnError reports whether a transient category warrants an
// automatic retry (429 and 5xx/timeout/network). Client errors like auth or
// unknown models fail immediately, and the agent's own request_timeout does
// not retry (the abort was a local policy decision).
func RetryableTurnError(category string) bool {
	return category == nanoprotocol.TurnErrorRateLimited ||
		category == nanoprotocol.TurnErrorProviderUnavailable
}

// isContextLength mirrors the checks the agent loop already performs on error
// strings for the compression retry.
func isContextLength(msg string) bool {
	return strings.Contains(msg, "context_length_exceeded") ||
		strings.Contains(msg, "maximum context length")
}

// isReasoningReplay reports the DeepSeek thinking-mode error returned when a
// tool-call turn's reasoning_content was not passed back on a later request.
func isReasoningReplay(msg string) bool {
	return strings.Contains(msg, "reasoning_content")
}

// isUnpairedToolCall reports the provider error returned when an assistant
// message's tool_calls are not followed by a tool message for every call.
// The request-time sanitizer repairs such history, so this is a defensive
// classification.
func isUnpairedToolCall(msg string) bool {
	return strings.Contains(msg, "must be followed by tool messages") ||
		strings.Contains(msg, "insufficient tool messages following tool_calls")
}

// sanitizeDetail renders an error as a single short line, stripping anything
// URL-shaped so provider endpoints or credentials never leak to end users.
func sanitizeDetail(detail string) string {
	detail = strings.ReplaceAll(detail, "\n", " ")
	fields := strings.Fields(detail)
	kept := make([]string, 0, len(fields))
	for _, f := range fields {
		if strings.Contains(f, "://") || strings.Contains(f, "key=") {
			continue
		}
		kept = append(kept, f)
	}
	detail = strings.Join(kept, " ")
	if len(detail) > 300 {
		detail = detail[:300] + "…"
	}
	return detail
}
