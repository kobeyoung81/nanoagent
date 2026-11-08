package provider

import (
	"encoding/json"
	"strings"
)

// SanitizeMessages repairs LLM request history so it satisfies the provider's
// tool-call contract. Providers reject the ENTIRE request when any of these
// hold, so a single poisoned turn would wedge the session forever:
//
//   - an assistant tool call's function.arguments is not valid JSON (e.g. a
//     stream cut mid-generation),
//   - an assistant tool call has no matching "tool" response in the messages
//     that immediately follow it (the provider's "must be followed by tool
//     messages" 400),
//   - a "tool" result's tool_call_id does not refer to a retained call,
//   - an assistant row has neither content nor tool calls.
//
// It is idempotent and cheap; call it when building request history and again
// at the provider boundary (defense in depth).
func SanitizeMessages(msgs []Message) []Message {
	// Phase 1: pair each assistant tool call with the tool result that follows
	// it. Only a result in the contiguous run of "tool" messages immediately
	// after the assistant row counts, mirroring the provider's requirement.
	keptCalls := make([][]ToolCall, len(msgs))
	keptResult := make([]bool, len(msgs))
	for i, m := range msgs {
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			continue
		}
		resultAt := map[string][]int{}
		end := i + 1
		for ; end < len(msgs) && msgs[end].Role == "tool"; end++ {
			if id := msgs[end].ToolCallID; id != "" {
				resultAt[id] = append(resultAt[id], end)
			}
		}
		usedResult := map[int]bool{}
		calls := make([]ToolCall, 0, len(m.ToolCalls))
		for _, tc := range m.ToolCalls {
			if tc.ID == "" || !json.Valid([]byte(tc.Arguments)) {
				continue
			}
			for _, ri := range resultAt[tc.ID] {
				if !usedResult[ri] {
					usedResult[ri] = true
					keptResult[ri] = true
					calls = append(calls, tc)
					break
				}
			}
		}
		keptCalls[i] = calls
	}

	// Phase 2: rebuild, dropping calls/results that could not be paired and
	// birth-empty assistant rows.
	out := make([]Message, 0, len(msgs))
	for i, m := range msgs {
		switch {
		case m.Role == "assistant" && len(m.ToolCalls) > 0:
			calls := keptCalls[i]
			if len(calls) == 0 {
				// Nothing usable remains. A pure tool-call turn (empty content)
				// is dropped; narration-only rows survive as plain assistant
				// text (keeping any reasoning so a thinking model can replay it).
				if strings.TrimSpace(m.Content) == "" {
					continue
				}
				out = append(out, Message{Role: "assistant", Content: m.Content, Reasoning: m.Reasoning, Timestamp: m.Timestamp})
				continue
			}
			m.ToolCalls = calls
			out = append(out, m)
		case m.Role == "tool":
			if keptResult[i] {
				out = append(out, m)
			}
		case m.Role == "assistant" && len(m.ToolCalls) == 0 && strings.TrimSpace(m.Content) == "":
			// Birth-empty assistant row (no content, no tool calls).
			continue
		default:
			out = append(out, m)
		}
	}
	return out
}
