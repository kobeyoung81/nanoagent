package provider

import (
	"encoding/json"
	"strings"
)

// JSONState classifies a tool call's arguments string.
//
// A model writing a nested schema (parallel_execution carries each inner call's
// arguments as its own object) has to escape quotes, backslashes and control
// characters correctly. When it gets that wrong the arguments arrive as one of
// two very different things: a document cut off mid-way (a real truncation), or
// a structurally broken document the model considered finished. They read
// differently to whoever has to act on them, so classify rather than merely
// reject.
type JSONState int

const (
	// JSONValid: the text parses as JSON. An empty string counts: a tool with
	// no parameters may legitimately receive no arguments.
	JSONValid JSONState = iota
	// JSONTruncated: a proper prefix of a valid document — the scan ran out
	// inside a string or with brackets still open, having found no structural
	// error. The usual cause is the upstream's output limit.
	JSONTruncated
	// JSONMalformed: structurally broken — an unescaped control character, an
	// invalid escape, a missing key, or trailing content after a complete
	// value (two calls concatenated read as `{...}{...}`).
	JSONMalformed
)

// Tool-call argument failure kinds, carried on IncompleteToolCall.Kind.
const (
	ToolCallKindTruncated = "truncated"
	ToolCallKindMalformed = "malformed"
)

// Kind renders a state as the string carried on IncompleteToolCall.
func (s JSONState) Kind() string {
	if s == JSONTruncated {
		return ToolCallKindTruncated
	}
	return ToolCallKindMalformed
}

// ClassifyJSON reports how s fails (or does not fail) as a JSON document.
func ClassifyJSON(s string) JSONState {
	if strings.TrimSpace(s) == "" {
		return JSONValid
	}
	if json.Valid([]byte(s)) {
		return JSONValid
	}
	if jsonFragmentOK(s) {
		return JSONTruncated
	}
	return JSONMalformed
}

// NormalizeToolCalls rewrites an empty arguments string to "{}" and returns the
// calls whose arguments are not usable JSON. An empty string is a legal call for
// a tool with no parameters (json.Valid("") is false, which used to report such
// a call as truncated).
func NormalizeToolCalls(calls []ToolCall) []IncompleteToolCall {
	var bad []IncompleteToolCall
	for i := range calls {
		if strings.TrimSpace(calls[i].Arguments) == "" {
			calls[i].Arguments = "{}"
			continue
		}
		if state := ClassifyJSON(calls[i].Arguments); state != JSONValid {
			bad = append(bad, IncompleteToolCall{
				Index: i,
				Tool:  calls[i].Name,
				Bytes: len(calls[i].Arguments),
				Kind:  state.Kind(),
			})
		}
	}
	return bad
}

// What the scanner is waiting for at a given nesting level.
const (
	expValue      byte = iota // a value: the start of the document, or after a colon
	expKey                    // an object member's key, or the object's closing brace
	expColon                  // the colon after a member's key
	expAfterValue             // a comma or the enclosing bracket, after a complete value
	expItem                   // an array element, or the array's closing bracket
	expTopEnd                 // only whitespace: the top-level value is complete
)

// jsonFragmentOK reports whether s is a proper prefix of some valid JSON
// document: it walks the grammar json.Valid does, but tolerates the document
// ending in the middle — inside a string, inside brackets, or right after a
// key's colon — as long as everything seen so far is well formed. Any structural
// error (a raw control character inside a string, a bad escape, a missing key,
// extra content after a complete value) reports false.
func jsonFragmentOK(s string) bool {
	opens := []byte{'('} // the enclosing bracket; '(' is the virtual document root
	expects := []byte{expValue}

	for i := 0; i < len(s); {
		c := s[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			i++
			continue
		}
		top := len(expects) - 1

		switch expects[top] {
		case expValue, expItem:
			switch {
			case c == '{':
				opens = append(opens, '{')
				expects = append(expects, expKey)
				i++
			case c == '[':
				opens = append(opens, '[')
				expects = append(expects, expItem)
				i++
			case c == '"':
				next, closed := advanceString(s, i)
				if next < 0 {
					return false // a bad escape or a raw control character
				}
				if !closed {
					return true // the string never closed: a prefix
				}
				expects[top] = afterValue(top)
				i = next
			case c == 't' || c == 'f' || c == 'n':
				lit := "true"
				if c == 'f' {
					lit = "false"
				} else if c == 'n' {
					lit = "null"
				}
				switch {
				case strings.HasPrefix(s[i:], lit):
					expects[top] = afterValue(top)
					i += len(lit)
				case strings.HasPrefix(lit, s[i:]):
					return true // the literal is still streaming
				default:
					return false
				}
			case c == '-' || (c >= '0' && c <= '9'):
				n := skipJSONNumber(s, i)
				if n == 0 {
					return true // the number is still streaming
				}
				expects[top] = afterValue(top)
				i += n
			default:
				return false
			}
		case expKey:
			switch {
			case c == '}':
				opens = opens[:top]
				expects = expects[:top]
				expects[top-1] = afterValue(top - 1)
				i++
			case c == '"':
				next, closed := advanceString(s, i)
				if next < 0 {
					return false // a bad escape or a raw control character
				}
				if !closed {
					return true
				}
				expects[top] = expColon
				i = next
			default:
				return false // a key must be a string
			}
		case expColon:
			if c != ':' {
				return false
			}
			expects[top] = expValue
			i++
		case expAfterValue:
			switch c {
			case ',':
				if opens[top] == '(' {
					return false // the top-level value takes no comma
				}
				if opens[top] == '{' {
					expects[top] = expKey
				} else {
					expects[top] = expItem
				}
				i++
			case '}', ']':
				if top == 0 || c != closerOf(opens[top]) {
					return false // a closing bracket that does not match
				}
				opens = opens[:top]
				expects = expects[:top]
				expects[top-1] = afterValue(top - 1)
				i++
			default:
				return false // trailing content after a complete value
			}
		case expTopEnd:
			return false
		}
	}
	return true
}

// afterValue is what a frame expects once a value inside it is complete: the
// document root accepts only whitespace from then on.
func afterValue(top int) byte {
	if top == 0 {
		return expTopEnd
	}
	return expAfterValue
}

// closerOf returns the bracket that closes open.
func closerOf(open byte) byte {
	if open == '{' {
		return '}'
	}
	return ']'
}

// advanceString scans the JSON string starting at i (s[i] is the opening quote).
// It returns the index just past the closing quote and whether the string
// closed; a negative index reports a structural error inside the string — a raw
// control character or an invalid escape — which is a malformed document rather
// than a cut.
func advanceString(s string, i int) (int, bool) {
	i++ // opening quote
	for i < len(s) {
		switch s[i] {
		case '"':
			return i + 1, true
		case '\\':
			if i+1 >= len(s) {
				return i, false // the escape is still streaming
			}
			switch s[i+1] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				i += 2
			case 'u':
				if i+5 >= len(s) {
					return i, false // the \u escape is still streaming
				}
				if !isHex4(s[i+2 : i+6]) {
					return -1, false
				}
				i += 6
			default:
				return -1, false
			}
		default:
			if s[i] < 0x20 {
				return -1, false
			}
			i++
		}
	}
	return i, false // ran out of input: a prefix
}

func isHex4(s string) bool {
	if len(s) != 4 {
		return false
	}
	for i := 0; i < 4; i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// skipJSONNumber returns the length of the number literal at i, or 0 when the
// literal is still streaming (a trailing "-", "1e", "1.0e+").
func skipJSONNumber(s string, i int) int {
	j := i
	if j < len(s) && s[j] == '-' {
		j++
	}
	digits := 0
	for j < len(s) && s[j] >= '0' && s[j] <= '9' {
		j++
		digits++
	}
	if j < len(s) && s[j] == '.' {
		j++
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
			digits++
		}
	}
	if digits == 0 {
		return 0
	}
	if j < len(s) && (s[j] == 'e' || s[j] == 'E') {
		j++
		if j < len(s) && (s[j] == '+' || s[j] == '-') {
			j++
		}
		if j >= len(s) || s[j] < '0' || s[j] > '9' {
			return 0
		}
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
	}
	return j - i
}
