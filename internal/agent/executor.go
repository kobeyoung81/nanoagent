package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"gitee.com/kobeyoung81/nanosec/internal/messageid"
	"gitee.com/kobeyoung81/nanosec/internal/provider"
)

// reactLoop is the single ReAct (reasoning + tool use) loop shared by the main
// session agent and isolated subagents. Callers adapt it with hooks so their
// own persistence, streaming and permission behavior is preserved while the
// iteration/retry structure lives in one place.
//
// It returns the final assistant content, the message id of the last LLM call
// and the last response (so callers can finalize error turns).

var (
	errReactMaxIterations = errors.New("react: max iterations reached")
	errReactEmpty         = errors.New("react: empty completion from provider after retry")
)

// maxToolCallRecoveries bounds how many times one turn may answer the model's
// unusable tool-call arguments with an error and let it try again. Two is
// enough for a model that wrote one call badly; more means it cannot express
// the call at all and the turn should fail with a clear reason instead of
// burning the iteration budget.
const maxToolCallRecoveries = 2

type reactHooks struct {
	// beforeIteration runs at the top of each iteration (e.g. summarization).
	// iteration is the 0-based loop index.
	beforeIteration func(iteration int)
	// buildMessages returns the LLM message list for the current iteration.
	buildMessages func() []provider.Message
	// chat performs one completion; messageID identifies the streamed turn.
	chat func(ctx context.Context, req provider.ChatRequest, messageID string) (*provider.ChatResponse, error)
	// onUsage receives token usage after each completion (optional).
	onUsage func(provider.Usage)
	// onAssistant persists the assistant message for tool-call and final
	// responses (optional).
	onAssistant func(messageID string, resp *provider.ChatResponse)
	// onToolUse runs before a tool executes (optional; e.g. stream an event).
	onToolUse func(messageID string, tc provider.ToolCall)
	// executeTool runs one tool and returns its result for the model.
	executeTool func(ctx context.Context, messageID string, tc provider.ToolCall) (string, error)
	// onToolResult runs after a tool executes (optional; e.g. stream an event).
	onToolResult func(messageID string, tc provider.ToolCall, result string)
	// recordToolResult stores the answer the loop gives to a call it did not
	// execute — a call whose arguments were unusable JSON. The answer is an
	// error result exactly like a failing tool's, so the model can re-emit the
	// call; the caller owns where that row lives (optional).
	recordToolResult func(messageID string, tc provider.ToolCall, result string)
	// afterToolIteration runs after a tool-call iteration's tools complete
	// (optional; e.g. persist the session).
	afterToolIteration func(messageID string)
}

type reactRequest struct {
	model       string
	reasoning   string
	temperature *float32
	topP        float32
	sessionID   string
	tools       []provider.ToolDef
	maxIter     int
}

func reactLoop(ctx context.Context, req reactRequest, hooks reactHooks) (string, string, *provider.ChatResponse, error) {
	retriedEmpty := false
	recoveries := 0
	var resp *provider.ChatResponse
	var messageID string
	maxIter := req.maxIter
	if maxIter <= 0 {
		maxIter = 1
	}
	for i := 0; i < maxIter; i++ {
		if hooks.beforeIteration != nil {
			hooks.beforeIteration(i)
		}
		messageID = messageid.New()
		r, err := hooks.chat(ctx, provider.ChatRequest{
			Model:           req.model,
			Messages:        hooks.buildMessages(),
			Tools:           req.tools,
			ReasoningEffort: req.reasoning,
			Temperature:     req.temperature,
			TopP:            req.topP,
			SessionID:       req.sessionID,
		}, messageID)
		resp = r
		// A response whose tool-call arguments are not usable JSON is not a
		// failed turn: the calls are answered with an error result — the same
		// shape as a tool that failed — so the model sees what it wrote and can
		// re-emit it. Only an exhausted recovery budget fails the turn.
		var incomplete *provider.ErrToolCallIncomplete
		recovering := err != nil && errors.As(err, &incomplete) && r != nil && len(r.ToolCalls) > 0
		if err != nil && !recovering {
			return "", messageID, resp, err
		}
		if recovering {
			recoveries++
			if recoveries > maxToolCallRecoveries {
				return "", messageID, resp, err
			}
		}
		if hooks.onUsage != nil {
			hooks.onUsage(resp.Usage)
		}

		if len(resp.ToolCalls) > 0 {
			// Unusable calls are answered, never executed, and are never
			// persisted as the model wrote them: arguments the provider would
			// reject on every later request must not reach the transcript.
			dropped := map[int]string{}
			asWritten := map[int]provider.ToolCall{}
			if recovering {
				for _, c := range incomplete.Calls {
					if c.Index < 0 || c.Index >= len(resp.ToolCalls) {
						continue
					}
					asWritten[c.Index] = provider.ToolCall{
						ID:        resp.ToolCalls[c.Index].ID,
						Name:      resp.ToolCalls[c.Index].Name,
						Arguments: resp.ToolCalls[c.Index].Arguments,
					}
					dropped[c.Index] = incompleteToolCallNotice(c, incomplete.FinishReason)
					resp.ToolCalls[c.Index].Arguments = "{}"
				}
			}
			if hooks.onAssistant != nil {
				hooks.onAssistant(messageID, resp)
			}
			for idx, tc := range resp.ToolCalls {
				if hooks.onToolUse != nil {
					shown := tc
					if original, ok := asWritten[idx]; ok {
						// The card shows what the model actually wrote; the
						// transcript keeps the legal placeholder.
						shown = original
					}
					hooks.onToolUse(messageID, shown)
				}
				if notice, isDropped := dropped[idx]; isDropped {
					if hooks.recordToolResult != nil {
						hooks.recordToolResult(messageID, tc, notice)
					}
					if hooks.onToolResult != nil {
						hooks.onToolResult(messageID, tc, notice)
					}
					continue
				}
				result, execErr := hooks.executeTool(ctx, messageID, tc)
				if execErr != nil {
					if result == "" {
						result = fmt.Sprintf("Error: %v", execErr)
					}
				}
				if result == "" {
					result = "(no output)"
				}
				if hooks.onToolResult != nil {
					hooks.onToolResult(messageID, tc, result)
				}
			}
			if hooks.afterToolIteration != nil {
				hooks.afterToolIteration(messageID)
			}
			continue
		}

		// A wholly empty completion (no content, no tool calls) is retried once.
		if strings.TrimSpace(resp.Content) == "" {
			if !retriedEmpty {
				retriedEmpty = true
				continue
			}
			return "", messageID, resp, errReactEmpty
		}

		if hooks.onAssistant != nil {
			hooks.onAssistant(messageID, resp)
		}
		return resp.Content, messageID, resp, nil
	}
	return "", messageID, resp, errReactMaxIterations
}

// incompleteToolCallNotice is what the model gets back in place of a tool
// result when its arguments were not usable JSON. It names the failure, says
// that nothing ran, and states the two rules that produced it in practice:
// nested calls carry their arguments as objects, and quotes/backslashes in the
// strings inside them have to be escaped. The recovery is the same as any tool
// that failed — the model may simply re-emit the call.
//
// write and edit carry their payload inline, so a cut-off argument is usually
// length, not syntax: telling them to re-emit the same body invites the same
// truncation. They get the smaller route instead.
func incompleteToolCallNotice(c provider.IncompleteToolCall, finishReason string) string {
	what := "was not valid JSON"
	if c.Kind == provider.ToolCallKindTruncated {
		what = "was cut off before it was complete JSON"
	}
	switch c.Tool {
	case "write":
		return fmt.Sprintf(`Error: the arguments for this write call %s (%d bytes, finish_reason=%q), so nothing ran. The content was too long for one response. Re-emit it in pieces: send the first chunk (at most 16 KiB) with write, then continue the same file with write and append=true, one chunk per call.`,
			what, c.Bytes, finishReason)
	case "edit":
		return fmt.Sprintf(`Error: the arguments for this edit call %s (%d bytes, finish_reason=%q), so nothing ran. The texts were too long for one response. Re-emit it with old_text as the shortest unique fragment and new_text no larger than 16 KiB; for a larger rewrite, use write (first chunk) followed by append=true.`,
			what, c.Bytes, finishReason)
	}
	return fmt.Sprintf(`Error: the arguments for this %s call %s (%d bytes, finish_reason=%q), so nothing ran. Re-emit this call now with valid arguments: pass each call's "arguments" as a JSON object (never a JSON string of one), escape quotes, backslashes and newlines inside command strings (\", \\, \n), and send at most 4 calls per parallel_execution batch.`,
		c.Tool, what, c.Bytes, finishReason)
}

// isContextLengthError reports whether a provider error is a context overflow.
func isContextLengthError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "context_length_exceeded") || strings.Contains(msg, "maximum context length")
}
