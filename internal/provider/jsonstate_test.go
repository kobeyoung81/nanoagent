package provider

import "testing"

// TestClassifyJSON pins the three-way classification the loop depends on: the
// message the model gets back differs between a document that was cut off and
// one it finished writing wrong, and a tool call with no arguments at all is
// legitimate rather than an error.
func TestClassifyJSON(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want JSONState
	}{
		{"object", `{"command":"ls"}`, JSONValid},
		{"empty is a legal no-argument call", ``, JSONValid},
		{"blank is a legal no-argument call", "  \n ", JSONValid},
		{"cut inside a string", `{"command":"ls /tmp/`, JSONTruncated},
		{"cut after a colon", `{"calls":`, JSONTruncated},
		{"cut with a bracket open", `{"calls":[{"tool":"bash"}`, JSONTruncated},
		{"cut mid number", `{"n":12`, JSONTruncated},
		{"cut mid literal", `{"ok":tru`, JSONTruncated},
		{"raw newline inside a string is not JSON", "{\"command\":\"line one\nline two\"}", JSONMalformed},
		{"two calls concatenated", `{"a":1}{"b":2}`, JSONMalformed},
		{"trailing content after a value", `{"a":1} oops`, JSONMalformed},
		{"missing key quotes", `{command:"ls"}`, JSONMalformed},
		{"bad escape", `{"a":"\q"}`, JSONMalformed},
		{"truncated \\u escape is a cut", `{"a":"\u00`, JSONTruncated},
		{"bad \\u escape is malformed", `{"a":"\uZZZZ"}`, JSONMalformed},
		{"mismatched closer", `{"a":1]`, JSONMalformed},
	}
	for _, tc := range cases {
		if got := ClassifyJSON(tc.in); got != tc.want {
			t.Errorf("%s: ClassifyJSON(%q) = %v, want %v", tc.name, tc.in, got, tc.want)
		}
	}
}

// TestNormalizeToolCalls pins the boundary fix: json.Valid("") is false, so a
// tool called with no arguments used to be reported as a truncated call of 0
// bytes, failing the turn for a call that was perfectly legal.
func TestNormalizeToolCalls(t *testing.T) {
	calls := []ToolCall{
		{ID: "a", Name: "read", Arguments: ""},
		{ID: "b", Name: "write", Arguments: `{"path":"/tmp/a","content":"x`},
		{ID: "c", Name: "bash", Arguments: `{"command":"echo hi"}`},
		{ID: "d", Name: "parallel_execution", Arguments: `{"calls":[{"tool":"bash"}]} oops`},
	}
	bad := NormalizeToolCalls(calls)
	if calls[0].Arguments != "{}" {
		t.Errorf("empty arguments should become {} , got %q", calls[0].Arguments)
	}
	if len(bad) != 2 {
		t.Fatalf("got %d unusable calls, want 2: %+v", len(bad), bad)
	}
	if bad[0].Index != 1 || bad[0].Kind != ToolCallKindTruncated || bad[0].Bytes != len(calls[1].Arguments) {
		t.Errorf("truncated call misreported: %+v", bad[0])
	}
	if bad[1].Index != 3 || bad[1].Kind != ToolCallKindMalformed {
		t.Errorf("malformed call misreported: %+v", bad[1])
	}
}
