package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// roundTripperFunc adapts a function to http.RoundTripper.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// TestLoggingTransport_InjectsSessionID verifies the X-Session-Id header is
// emitted from the request context so the gateway can pin prompt-cache/routing
// affinity to a stable conversation.
func TestLoggingTransport_InjectsSessionID(t *testing.T) {
	var got string
	base := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		got = req.Header.Get("X-Session-Id")
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{}`)),
		}, nil
	})
	tr := &loggingTransport{base: base}

	req, err := http.NewRequest(http.MethodPost, "http://example.test/v1/chat/completions", strings.NewReader(`{"stream":false}`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req = req.WithContext(withSessionID(req.Context(), "sess-abc"))
	if _, err := tr.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if got != "sess-abc" {
		t.Fatalf("X-Session-Id = %q, want sess-abc", got)
	}

	// No session ID => no header injected.
	got = "unset"
	req2, _ := http.NewRequest(http.MethodPost, "http://example.test/v1/chat/completions", strings.NewReader(`{}`))
	if _, err := tr.RoundTrip(req2); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if got != "" {
		t.Fatalf("X-Session-Id = %q, want empty when no session is set", got)
	}
}

func TestNewOpenAI(t *testing.T) {
	p := NewOpenAI("test-key", "http://localhost:8080/v1", "gpt-4-test", 0)

	if p.defaultModel != "gpt-4-test" {
		t.Errorf("Expected default model 'gpt-4-test', got '%s'", p.defaultModel)
	}

	// We can't easily test the client internals without reflection or mocking the transport,
	// but verifying the constructor sets the fields we access is a good start.
}

func TestBuildChatRequest_ReasoningEffortPassthrough(t *testing.T) {
	base := []Message{{Role: "user", Content: "hi"}}

	// Empty: the parameter must never be sent (strict providers reject it for
	// non-reasoning models).
	req, err := buildChatRequest(ChatRequest{Model: "m", Messages: base}, "", false)
	if err != nil {
		t.Fatalf("buildChatRequest: %v", err)
	}
	if req.ReasoningEffort != "" {
		t.Fatalf("expected empty reasoning_effort, got %q", req.ReasoningEffort)
	}

	// Set: passed through verbatim so any gateway-supported value works.
	req, err = buildChatRequest(ChatRequest{Model: "m", Messages: base, ReasoningEffort: "low"}, "", false)
	if err != nil {
		t.Fatalf("buildChatRequest: %v", err)
	}
	if req.ReasoningEffort != "low" {
		t.Fatalf("reasoning_effort = %q, want low", req.ReasoningEffort)
	}
}

func TestBuildChatRequest_ReasoningContentPassthrough(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "hi"},
		{
			Role:      "assistant",
			ToolCalls: []ToolCall{{ID: "c1", Name: "bash", Arguments: `{}`}},
			Reasoning: "I should run a command.",
		},
		{Role: "tool", ToolCallID: "c1", Content: "ok"},
	}
	req, err := buildChatRequest(ChatRequest{Model: "m", Messages: msgs}, "", false)
	if err != nil {
		t.Fatalf("buildChatRequest: %v", err)
	}
	if got := req.Messages[1].ReasoningContent; got != "I should run a command." {
		t.Fatalf("reasoning_content = %q, want the persisted reasoning", got)
	}

	// A message without reasoning must not carry the field (omitempty), so
	// non-thinking models and strict providers are unaffected.
	plain, err := buildChatRequest(ChatRequest{Model: "m", Messages: []Message{{Role: "assistant", Content: "x"}}}, "", false)
	if err != nil {
		t.Fatalf("buildChatRequest: %v", err)
	}
	if plain.Messages[0].ReasoningContent != "" {
		t.Fatalf("unexpected reasoning_content %q", plain.Messages[0].ReasoningContent)
	}
}

// TestBuildChatRequest_StreamOptions verifies stream_options is only attached
// to streaming requests. Non-streaming requests must not carry it: some
// OpenAI-compatible gateways (e.g. opencode Go) reject stream_options when
// stream is not true.
func TestBuildChatRequest_StreamOptions(t *testing.T) {
	msgs := []Message{{Role: "user", Content: "hi"}}

	nonStream, err := buildChatRequest(ChatRequest{Model: "m", Messages: msgs}, "", false)
	if err != nil {
		t.Fatalf("buildChatRequest: %v", err)
	}
	if nonStream.StreamOptions != nil {
		t.Fatalf("non-streaming StreamOptions = %+v, want nil", nonStream.StreamOptions)
	}
	body, err := json.Marshal(nonStream)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), "stream_options") {
		t.Fatalf("non-streaming body must not contain stream_options: %s", body)
	}

	stream, err := buildChatRequest(ChatRequest{Model: "m", Messages: msgs}, "", true)
	if err != nil {
		t.Fatalf("buildChatRequest: %v", err)
	}
	if stream.StreamOptions == nil || !stream.StreamOptions.IncludeUsage {
		t.Fatalf("streaming StreamOptions = %+v, want include_usage true", stream.StreamOptions)
	}
}

// TestChatStream_EmitsThoughtBeforeContent pins the order of the two fields a
// single boundary chunk can carry. A short reasoning turn often ends with one
// frame holding both the tail of reasoning_content and the first answer tokens;
// the client renders the reasoning block above the answer, so the thought must
// arrive first or it opens a second block holding only the tail.
func TestChatStream_EmitsThoughtBeforeContent(t *testing.T) {
	frames := []string{
		`{"choices":[{"delta":{"reasoning_content":"thinking hard"},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"reasoning_content":" tail","content":"The answer"},"finish_reason":null}]}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, frame := range frames {
			fmt.Fprintf(w, "data: %s\n\n", frame)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer srv.Close()

	p := NewOpenAI("test-key", srv.URL, "test-model", 5*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := p.ChatStream(ctx, ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	var got []string
	for ev := range stream {
		if ev.Err != nil {
			t.Fatalf("stream error: %v", ev.Err)
		}
		if ev.Delta != "" {
			got = append(got, ev.Kind+"="+ev.Delta)
		}
	}
	want := []string{"thought=thinking hard", "thought= tail", "content=The answer"}
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("events = %v, want %v", got, want)
		}
	}
}

// TestChatStream_CarriesFinishReason pins that the upstream stop reason reaches
// the Done event. The agent needs it to explain a cut-off completion: a
// truncated tool call arrives with finish_reason "length", and the user must be
// told that instead of a bare "provider unavailable".
func TestChatStream_CarriesFinishReason(t *testing.T) {
	frames := []string{
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"write","arguments":"{\"path\":\"/tmp/a\""}}]},"finish_reason":null}]}`,
		`{"choices":[{"delta":{},"finish_reason":"length"}]}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, frame := range frames {
			fmt.Fprintf(w, "data: %s\n\n", frame)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer srv.Close()

	p := NewOpenAI("test-key", srv.URL, "test-model", 5*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := p.ChatStream(ctx, ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	var got string
	sawDone := false
	for ev := range stream {
		if ev.Err != nil {
			t.Fatalf("stream error: %v", ev.Err)
		}
		if ev.Done {
			sawDone = true
			got = ev.FinishReason
		}
	}
	if !sawDone {
		t.Fatal("stream ended without a Done event")
	}
	if got != "length" {
		t.Fatalf("Done FinishReason = %q, want %q", got, "length")
	}
}

// streamFrames serves the given SSE frames and returns the provider plus a
// collector for the Done event's tool calls.
func streamFrames(t *testing.T, frames []string) (*OpenAIProvider, func() ([]ToolCall, string)) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, frame := range frames {
			fmt.Fprintf(w, "data: %s\n\n", frame)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	t.Cleanup(srv.Close)

	p := NewOpenAI("test-key", srv.URL, "test-model", 5*time.Second)
	collect := func() ([]ToolCall, string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stream, err := p.ChatStream(ctx, ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}})
		if err != nil {
			t.Fatalf("ChatStream: %v", err)
		}
		var calls []ToolCall
		var reason string
		for ev := range stream {
			if ev.Err != nil {
				t.Fatalf("stream error: %v", ev.Err)
			}
			if ev.Done {
				calls = ev.ToolCalls
				reason = ev.FinishReason
			}
		}
		return calls, reason
	}
	return p, collect
}

// TestChatStream_SeparatesCallsThatReuseAChunkIndex pins that two tool calls
// sharing one chunk index stay two calls. Relays that always report index 0 used
// to have their arguments concatenated into `{...}{...}` — invalid JSON for a
// call the model had actually written correctly, and the name of the last call
// overwrote the first.
func TestChatStream_SeparatesCallsThatReuseAChunkIndex(t *testing.T) {
	frames := []string{
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"read","arguments":"{\"path\":"}}]},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"arguments":"\"/a\"}"}}]},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c2","function":{"name":"grep","arguments":"{\"pattern\":"}}]},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c2","function":{"arguments":"\"x\"}"}}]},"finish_reason":null}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	}
	_, collect := streamFrames(t, frames)
	calls, _ := collect()
	if len(calls) != 2 {
		t.Fatalf("got %d tool calls, want 2: %+v", len(calls), calls)
	}
	if calls[0].ID != "c1" || calls[0].Name != "read" || !json.Valid([]byte(calls[0].Arguments)) {
		t.Errorf("first call = %+v", calls[0])
	}
	if calls[1].ID != "c2" || calls[1].Name != "grep" || !json.Valid([]byte(calls[1].Arguments)) {
		t.Errorf("second call = %+v", calls[1])
	}
}

// TestChatStream_KeepsSparseChunkIndices pins that a call whose index is not
// part of a dense 0..n-1 range is not dropped. Assembly used to iterate the
// accumulator map by position, so a lone index 1 produced no call at all — the
// model's tool call vanished with no error anywhere.
func TestChatStream_KeepsSparseChunkIndices(t *testing.T) {
	frames := []string{
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"c7","function":{"name":"read","arguments":"{\"path\":\"/a\"}"}}]},"finish_reason":null}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	}
	_, collect := streamFrames(t, frames)
	calls, _ := collect()
	if len(calls) != 1 {
		t.Fatalf("got %d tool calls, want 1: %+v", len(calls), calls)
	}
	if calls[0].ID != "c7" || calls[0].Name != "read" {
		t.Errorf("call = %+v", calls[0])
	}
}

// TestChatStream_ContinuesAnIdlessCall pins the other half: a continuation chunk
// that repeats neither index nor id belongs to the call already open, not to a
// new one.
func TestChatStream_ContinuesAnIdlessCall(t *testing.T) {
	frames := []string{
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"write","arguments":"{\"path\":"}}]},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"\"/a\"}"}}]},"finish_reason":null}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	}
	_, collect := streamFrames(t, frames)
	calls, _ := collect()
	if len(calls) != 1 {
		t.Fatalf("got %d tool calls, want 1: %+v", len(calls), calls)
	}
	if calls[0].Arguments != `{"path":"/a"}` {
		t.Errorf("arguments = %q, want the two fragments joined", calls[0].Arguments)
	}
}

// TestChatStream_AssignsAnIDWhenTheProviderSendsNone pins that every call can be
// answered: the tool message the loop replies with is matched by tool_call_id,
// and a call without one is dropped (with its result) by the request sanitizer.
func TestChatStream_AssignsAnIDWhenTheProviderSendsNone(t *testing.T) {
	frames := []string{
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"read","arguments":"{\"path\":\"/a\"}"}}]},"finish_reason":null}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	}
	_, collect := streamFrames(t, frames)
	calls, _ := collect()
	if len(calls) != 1 {
		t.Fatalf("got %d tool calls, want 1: %+v", len(calls), calls)
	}
	if calls[0].ID == "" {
		t.Error("call has no id; its tool result could never be paired")
	}
}

// TestChatStream_JoinsIdlessContinuationsByIndex pins the standard OpenAI
// shape: the id is sent only on a call's first delta, and every continuation
// repeats just the index. Keying the first delta by id and the continuation by
// index opened two accumulators — the name landed in one and the arguments in
// an empty-named twin, so the executor ran the named call with empty arguments
// and reported "tool not found: " for the twin.
func TestChatStream_JoinsIdlessContinuationsByIndex(t *testing.T) {
	frames := []string{
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"bash","arguments":""}}]},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"command\":"}}]},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"pwd\"}"}}]},"finish_reason":null}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	}
	_, collect := streamFrames(t, frames)
	calls, _ := collect()
	if len(calls) != 1 {
		t.Fatalf("got %d tool calls, want 1: %+v", len(calls), calls)
	}
	if calls[0].ID != "call_1" || calls[0].Name != "bash" {
		t.Errorf("call = %+v, want id=call_1 name=bash", calls[0])
	}
	if calls[0].Arguments != `{"command":"pwd"}` {
		t.Errorf("arguments = %q, want the fragments joined", calls[0].Arguments)
	}
}

// TestChatStream_IdlessContinuationsFollowTheReusedIndex pins that when one
// index 0 is reused for a second call, an id-less continuation joins the call
// that most recently claimed the index (the second), while the first call keeps
// its own accumulated arguments.
func TestChatStream_IdlessContinuationsFollowTheReusedIndex(t *testing.T) {
	frames := []string{
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"read","arguments":"{\"path\":"}}]},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"/a\"}"}}]},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c2","function":{"name":"grep","arguments":"{\"pattern\":"}}]},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"x\"}"}}]},"finish_reason":null}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	}
	_, collect := streamFrames(t, frames)
	calls, _ := collect()
	if len(calls) != 2 {
		t.Fatalf("got %d tool calls, want 2: %+v", len(calls), calls)
	}
	if calls[0].ID != "c1" || calls[0].Name != "read" || calls[0].Arguments != `{"path":"/a"}` {
		t.Errorf("first call = %+v", calls[0])
	}
	if calls[1].ID != "c2" || calls[1].Name != "grep" || calls[1].Arguments != `{"pattern":"x"}` {
		t.Errorf("second call = %+v", calls[1])
	}
}

// TestChatStream_ExplicitTemperatureReachesTheWire is the goal judge's
// guarantee: a request that asks for 0 must carry "temperature":0, because 0 is
// how the judge is pinned and the SDK's omitempty would otherwise drop it (the
// provider would then use its default, usually 1.0).
//
// The same request without a temperature must NOT carry the key at all: "no
// preference" keeps the provider default and must not be turned into 0.
func TestChatStream_ExplicitTemperatureReachesTheWire(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(raw))
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer srv.Close()

	p := NewOpenAI("test-key", srv.URL, "test-model", 5*time.Second)
	drain := func(req ChatRequest) {
		stream, err := p.ChatStream(context.Background(), req)
		if err != nil {
			t.Fatalf("ChatStream: %v", err)
		}
		for range stream {
		}
	}

	zero := float32(0)
	drain(ChatRequest{Messages: []Message{{Role: "user", Content: "judge this"}}, Temperature: &zero})
	drain(ChatRequest{Messages: []Message{{Role: "user", Content: "answer this"}}})
	third := float32(0.7)
	drain(ChatRequest{Messages: []Message{{Role: "user", Content: "answer this"}}, Temperature: &third})

	if len(bodies) != 3 {
		t.Fatalf("captured %d bodies, want 3", len(bodies))
	}
	if !strings.Contains(bodies[0], `"temperature":0`) {
		t.Errorf("explicit zero did not reach the wire: %s", bodies[0])
	}
	if strings.Contains(bodies[1], `"temperature"`) {
		t.Errorf("an unset temperature must not be sent: %s", bodies[1])
	}
	if !strings.Contains(bodies[2], `"temperature":0.7`) {
		t.Errorf("a non-zero temperature was lost: %s", bodies[2])
	}
}
