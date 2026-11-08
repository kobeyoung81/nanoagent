package provider

import (
	"encoding/json"
	"reflect"
	"testing"
)

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func TestSanitizeMessages_CleanHistoryUnchanged(t *testing.T) {
	in := []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{
			{ID: "c1", Name: "bash", Arguments: `{"command":"ls"}`},
		}},
		{Role: "tool", ToolCallID: "c1", Content: "out"},
		{Role: "assistant", Content: "done"},
	}
	got := SanitizeMessages(in)
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("clean history was modified:\n got: %+v\nwant: %+v", got, in)
	}
}

func TestSanitizeMessages_DropsTruncatedCallAndOrphanResult(t *testing.T) {
	// Reproduces the poisoned session: a stream cut mid-arguments persisted an
	// assistant message whose function.arguments is not valid JSON.
	in := []Message{
		{Role: "user", Content: "please continue"},
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "bad", Name: "bash", Arguments: `{"command":" /root/.na`},
		}},
		{Role: "tool", ToolCallID: "bad", Content: "Error: unmarshal arguments: unexpected end of JSON input"},
		{Role: "user", Content: "again"},
	}
	got := SanitizeMessages(in)
	want := []Message{
		{Role: "user", Content: "please continue"},
		{Role: "user", Content: "again"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sanitized = %+v, want %+v", got, want)
	}
}

func TestSanitizeMessages_KeepsValidCallsAmongInvalid(t *testing.T) {
	in := []Message{
		{Role: "assistant", Content: "running checks", ToolCalls: []ToolCall{
			{ID: "ok1", Name: "ls", Arguments: mustJSON(t, map[string]string{"path": "/tmp"})},
			{ID: "bad", Name: "bash", Arguments: `{"command":`},
			{ID: "ok2", Name: "read_file", Arguments: mustJSON(t, map[string]string{"path": "/etc/hosts"})},
		}},
		{Role: "tool", ToolCallID: "ok1", Content: "a"},
		{Role: "tool", ToolCallID: "bad", Content: "orphan"},
		{Role: "tool", ToolCallID: "ok2", Content: "b"},
	}
	got := SanitizeMessages(in)
	if len(got) != 3 {
		t.Fatalf("expected 3 messages (assistant + 2 kept tool results), got %d: %+v", len(got), got)
	}
	if len(got[0].ToolCalls) != 2 {
		t.Fatalf("expected 2 kept tool calls, got %d", len(got[0].ToolCalls))
	}
	if got[0].ToolCalls[0].ID != "ok1" || got[0].ToolCalls[1].ID != "ok2" {
		t.Fatalf("wrong calls kept: %+v", got[0].ToolCalls)
	}
	if got[1].ToolCallID != "ok1" || got[2].ToolCallID != "ok2" {
		t.Fatalf("orphan result not dropped: %+v", got)
	}
	if got[0].Content != "running checks" {
		t.Fatalf("narration content lost: %q", got[0].Content)
	}
}

func TestSanitizeMessages_DropsEmptyPureToolCallAssistant(t *testing.T) {
	in := []Message{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "bad", Name: "bash", Arguments: `{"x`}}},
	}
	got := SanitizeMessages(in)
	if len(got) != 0 {
		t.Fatalf("expected empty output, got %+v", got)
	}
}

func TestSanitizeMessages_PreservesNarrationWhenCallsInvalid(t *testing.T) {
	in := []Message{
		{Role: "assistant", Content: "let me check that", ToolCalls: []ToolCall{
			{ID: "bad", Name: "bash", Arguments: `{"comm`},
		}},
	}
	got := SanitizeMessages(in)
	if len(got) != 1 || got[0].Role != "assistant" || got[0].Content != "let me check that" {
		t.Fatalf("narration not preserved: %+v", got)
	}
	if len(got[0].ToolCalls) != 0 {
		t.Fatalf("invalid calls should be stripped: %+v", got[0].ToolCalls)
	}
}

func TestSanitizeMessages_PreservesReasoningWhenCallsInvalid(t *testing.T) {
	in := []Message{
		{Role: "assistant", Content: "narration", Reasoning: "the why", ToolCalls: []ToolCall{
			{ID: "bad", Name: "bash", Arguments: `{"comm`},
		}},
	}
	got := SanitizeMessages(in)
	if len(got) != 1 || got[0].Reasoning != "the why" {
		t.Fatalf("reasoning not preserved when narration kept: %+v", got)
	}
}

func TestSanitizeMessages_TimestampsAndOtherRolesUntouched(t *testing.T) {
	in := []Message{
		{Role: "thought", Content: "thinking", ThoughtElapsedMs: 42},
		{Role: "error", Content: "rate_limited", Detail: "429"},
	}
	got := SanitizeMessages(in)
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("non-tool rows were modified: %+v", got)
	}
}

func TestSanitizeMessages_DropsBirthEmptyAssistant(t *testing.T) {
	in := []Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant"},
		{Role: "user", Content: "continue"},
	}
	got := SanitizeMessages(in)
	want := []Message{
		{Role: "user", Content: "hi"},
		{Role: "user", Content: "continue"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sanitized = %+v, want %+v", got, want)
	}
}

func TestSanitizeMessages_DropsWhitespaceOnlyAssistant(t *testing.T) {
	in := []Message{
		{Role: "assistant", Content: "   \n\t "},
	}
	got := SanitizeMessages(in)
	if len(got) != 0 {
		t.Fatalf("expected empty output, got %+v", got)
	}
}

func TestSanitizeMessages_DropsUnansweredToolCall(t *testing.T) {
	// Reproduces the "insufficient tool messages following tool_calls" 400: an
	// assistant tool call whose result was never persisted (crash/stop between
	// the assistant row and its tool results).
	in := []Message{
		{Role: "user", Content: "run it"},
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "c1", Name: "bash", Arguments: `{"command":"ls"}`},
		}},
		{Role: "user", Content: "next"},
	}
	got := SanitizeMessages(in)
	want := []Message{
		{Role: "user", Content: "run it"},
		{Role: "user", Content: "next"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sanitized = %+v, want %+v", got, want)
	}
}

func TestSanitizeMessages_DropsUnansweredCallKeepsNarration(t *testing.T) {
	in := []Message{
		{Role: "user", Content: "run it"},
		{Role: "assistant", Content: "let me check that", Reasoning: "the why", ToolCalls: []ToolCall{
			{ID: "c1", Name: "bash", Arguments: `{"command":"ls"}`},
		}},
		{Role: "user", Content: "next"},
	}
	got := SanitizeMessages(in)
	if len(got) != 3 {
		t.Fatalf("expected 3 messages, got %d: %+v", len(got), got)
	}
	if got[1].Role != "assistant" || got[1].Content != "let me check that" || len(got[1].ToolCalls) != 0 {
		t.Fatalf("narration not preserved with calls stripped: %+v", got[1])
	}
	if got[1].Reasoning != "the why" {
		t.Fatalf("reasoning not preserved: %+v", got[1])
	}
}

func TestSanitizeMessages_KeepsOnlyAnsweredCalls(t *testing.T) {
	in := []Message{
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "c1", Name: "ls", Arguments: `{}`},
			{ID: "c2", Name: "bash", Arguments: `{"command":"uname"}`},
		}},
		{Role: "tool", ToolCallID: "c1", Content: "a"},
	}
	got := SanitizeMessages(in)
	want := []Message{
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "c1", Name: "ls", Arguments: `{}`},
		}},
		{Role: "tool", ToolCallID: "c1", Content: "a"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sanitized = %+v, want %+v", got, want)
	}
}

func TestSanitizeMessages_DropsNonAdjacentResult(t *testing.T) {
	// A result that is not in the contiguous tool block right after its call
	// does not satisfy the provider, so both sides are dropped.
	in := []Message{
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "c1", Name: "ls", Arguments: `{}`},
		}},
		{Role: "user", Content: "interrupt"},
		{Role: "tool", ToolCallID: "c1", Content: "late"},
	}
	got := SanitizeMessages(in)
	want := []Message{
		{Role: "user", Content: "interrupt"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sanitized = %+v, want %+v", got, want)
	}
}

func TestSanitizeMessages_DuplicateIDsKeptOnce(t *testing.T) {
	in := []Message{
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "c1", Name: "ls", Arguments: `{}`},
			{ID: "c1", Name: "ls", Arguments: `{}`},
		}},
		{Role: "tool", ToolCallID: "c1", Content: "a"},
	}
	got := SanitizeMessages(in)
	if len(got) != 2 || len(got[0].ToolCalls) != 1 {
		t.Fatalf("expected one call paired with its single result, got %+v", got)
	}
}

func TestSanitizeMessages_DropsEmptyIDCallAndResult(t *testing.T) {
	in := []Message{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "", Name: "ls", Arguments: `{}`}}},
		{Role: "tool", ToolCallID: "", Content: "a"},
	}
	got := SanitizeMessages(in)
	if len(got) != 0 {
		t.Fatalf("expected empty output, got %+v", got)
	}
}

func TestSanitizeMessages_ReproducesWedgedSession(t *testing.T) {
	// Exact shape of the wedged production session: empty assistant rows
	// sandwiched between tool results and the user's "please continue".
	in := []Message{
		{Role: "user", Content: "explore environment"},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{
			{ID: "c1", Name: "bash", Arguments: `{"command":"ls"}`},
		}},
		{Role: "tool", ToolCallID: "c1", Content: "file list"},
		{Role: "assistant"},
		{Role: "user", Content: "continue"},
		{Role: "assistant"},
		{Role: "user", Content: "continue"},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{
			{ID: "c2", Name: "bash", Arguments: `{"command":"uname -a"}`},
		}},
		{Role: "tool", ToolCallID: "c2", Content: "Linux ..."},
		{Role: "user", Content: "continue"},
	}
	got := SanitizeMessages(in)
	want := []Message{
		{Role: "user", Content: "explore environment"},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{
			{ID: "c1", Name: "bash", Arguments: `{"command":"ls"}`},
		}},
		{Role: "tool", ToolCallID: "c1", Content: "file list"},
		{Role: "user", Content: "continue"},
		{Role: "user", Content: "continue"},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{
			{ID: "c2", Name: "bash", Arguments: `{"command":"uname -a"}`},
		}},
		{Role: "tool", ToolCallID: "c2", Content: "Linux ..."},
		{Role: "user", Content: "continue"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sanitized = %+v, want %+v", got, want)
	}
}
