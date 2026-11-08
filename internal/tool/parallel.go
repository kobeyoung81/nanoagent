package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanosec/internal/provider"
)

// maxParallelCalls bounds one batch. The loop runs the calls of an assistant
// message one after another, which is why a model that wants N reads or N
// subagents at the same time asks for them here; the cap keeps one request from
// fanning out without limit.
const maxParallelCalls = 8

// defaultParallelBudgetChars caps the merged result. It mirrors the read tool's
// budget rationale: the global tool-result cap keeps the head of a result, so a
// merged block must stay inside it or its last calls are silently cut off.
const defaultParallelBudgetChars = 12000

// parallelRefusedTools are the calls a batch may not carry: a blocking prompt,
// session-singleton state, or a nested batch. Each entry says why, so the model
// learns the rule instead of retrying.
var parallelRefusedTools = map[string]string{
	"question":           "asks the user and blocks the turn, so it cannot run inside a batch",
	"todowrite":          "updates session state the batch cannot sequence",
	"plan_write":         "writes the conversation's plan artefact",
	"plan_execute":       "closes the conversation's plan artefact",
	"cron":               "schedules future work",
	"download_media":     "changes media state",
	"upload_media":       "changes media state",
	"parallel_execution": "batches cannot be nested",
}

// pathWritingTools are the tools whose call rewrites a file. Two of them on one
// path inside a batch are refused: they depend on each other's result, and the
// batch is for independent calls. The alternative — serializing them — was tried
// and reverted: it silently ordered a concurrent tool, which contradicts what the
// tool is for and hides the caller's mistake instead of naming it.
var pathWritingTools = map[string]bool{"write": true, "edit": true, "append_note": true}

// ParallelDispatch is what the loop injects into ParallelTool. Run executes one
// inner call with exactly the semantics of a top-level call — permission check,
// approval flow, question wiring, session side effects and result capping all
// live there — and Decision reports the caller's permission for a tool name so
// an "ask" entry can be left out of the batch instead of raising an approval
// card a batch cannot present.
type ParallelDispatch struct {
	Run      func(ctx context.Context, name, argumentsJSON string) (string, error)
	Decision func(name string) nanoprotocol.PermissionAction
}

// ParallelDispatchTool is implemented by tools that need the loop's dispatch.
type ParallelDispatchTool interface {
	SetParallelDispatch(d ParallelDispatch)
}

// ParallelTool runs several tool calls concurrently. It is the only concurrency
// entry point: without it every call of a turn is sequential, which is what made
// four subagents in one message cost two hours. The tool never touches the
// registry itself — a denied tool is still registered, so dispatching directly
// would be a permission bypass.
type ParallelTool struct {
	mu       sync.RWMutex
	dispatch ParallelDispatch
	// MaxChars caps the merged result (0 = defaultParallelBudgetChars).
	MaxChars int
}

// NewParallelTool creates the tool; the loop injects its dispatch per turn.
func NewParallelTool() *ParallelTool { return &ParallelTool{} }

func (t *ParallelTool) Name() string { return "parallel_execution" }

func (t *ParallelTool) Description() string {
	return `Run several tool calls concurrently, in one call.

Use it whenever one turn needs more than one independent thing done — several
reads or greps, or several subagents — because tool calls written in the same
assistant message run one after another. Pass:

- calls: an array of { tool, arguments }. arguments is that tool's own arguments
  as a JSON object — pass the object itself, never a JSON string of one. A
  newline inside a command is written as \n, and a quote or a backslash inside
  a command is escaped (\", \\).

Semantics: results come back in request order, one <call> block each, with its
state and elapsed_ms; one failing call never stops the others; the whole batch
shares a time budget and a result budget. Calls that cannot be batched are
answered with an error inside their own block: question, todowrite, plan_write,
plan_execute, cron and the media tools, plus any call the mode would ask approval
for (call those on their own so the approval card can be shown). A batch is for
independent calls: if two entries write the same path (write, edit, append_note),
the second is refused — run those in order, one message each.`
}

func (t *ParallelTool) Parameters() provider.ParameterSchema {
	// RawParameters carries the nested schema; this mirrors it for callers that
	// only read the structured form.
	return provider.ParameterSchema{
		Type: "object",
		Properties: map[string]provider.PropertySchema{
			"calls": {Type: "array", Description: "Calls to run concurrently: [{tool, arguments}]"},
		},
		Required: []string{"calls"},
	}
}

// RawParameters returns the nested schema verbatim (the structured schema type
// cannot express an array of objects).
func (t *ParallelTool) RawParameters() []byte {
	return []byte(`{
  "type": "object",
  "properties": {
    "calls": {
      "type": "array",
      "description": "The calls to run concurrently (results come back in request order)",
      "items": {
        "type": "object",
        "properties": {
          "tool": { "type": "string", "description": "Name of the tool to run" },
          "arguments": { "type": "object", "description": "That tool's arguments, as an object" }
        },
        "required": ["tool"]
      }
    }
  },
  "required": ["calls"]
}`)
}

// SetParallelDispatch implements ParallelDispatchTool.
func (t *ParallelTool) SetParallelDispatch(d ParallelDispatch) {
	t.mu.Lock()
	t.dispatch = d
	t.mu.Unlock()
}

// parallelCall is one parsed entry of a batch.
type parallelCall struct {
	Index     int
	Tool      string
	Arguments string
	// Path is the serialization key for the path-writing tools ("" = none).
	Path string
}

// parallelResult is one entry's outcome.
type parallelResult struct {
	Index     int
	Tool      string
	Output    string
	Error     bool
	ElapsedMs int64
	Skip      bool
}

func (t *ParallelTool) Execute(ctx context.Context, params map[string]any) (string, error) {
	t.mu.RLock()
	dispatch := t.dispatch
	t.mu.RUnlock()
	if dispatch.Run == nil {
		return "", fmt.Errorf("parallel execution is not available in this context")
	}

	raw, _ := params["calls"].([]any)
	if len(raw) == 0 {
		return "", fmt.Errorf("missing calls[] (each entry is {tool, arguments})")
	}
	if len(raw) > maxParallelCalls {
		return "", fmt.Errorf("a batch runs at most %d calls, got %d", maxParallelCalls, len(raw))
	}

	calls := make([]parallelCall, len(raw))
	results := make([]parallelResult, len(raw))
	for i, item := range raw {
		calls[i], results[i] = parseParallelCall(i+1, item, dispatch)
	}
	refuseDuplicatePaths(calls, results)

	var wg sync.WaitGroup
	for i := range calls {
		if results[i].Skip {
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			start := time.Now()
			out, err := dispatch.Run(ctx, calls[i].Tool, calls[i].Arguments)
			results[i].ElapsedMs = time.Since(start).Milliseconds()
			if err != nil {
				results[i].Output, results[i].Error = err.Error(), true
				return
			}
			results[i].Output = out
		}(i)
	}
	wg.Wait()

	return t.render(results), nil
}

// refuseDuplicatePaths refuses a call that writes a path another call in the same
// batch already claimed. One of the two would otherwise win silently — either by
// racing (lost update) or by an implicit order the tool does not promise.
func refuseDuplicatePaths(calls []parallelCall, results []parallelResult) {
	claimed := map[string]int{}
	for i := range calls {
		if results[i].Skip || calls[i].Path == "" {
			continue
		}
		first, dup := claimed[calls[i].Path]
		if !dup {
			claimed[calls[i].Path] = calls[i].Index
			continue
		}
		results[i].Skip = true
		results[i].Error = true
		results[i].Output = fmt.Sprintf(
			"Error: this call writes %s, which call %d in the same batch also writes — those two depend on each other, so they cannot be batched. Run them in order, one message each.",
			calls[i].Path, first)
	}
}

// parseParallelCall validates one entry on its own: a malformed or refused entry
// becomes that entry's error, never the whole batch's.
func parseParallelCall(index int, item any, dispatch ParallelDispatch) (parallelCall, parallelResult) {
	call := parallelCall{Index: index}
	result := parallelResult{Index: index}
	fail := func(msg string) (parallelCall, parallelResult) {
		result.Output, result.Error, result.Skip = "Error: "+msg, true, true
		return call, result
	}

	obj, ok := item.(map[string]any)
	if !ok {
		return fail(fmt.Sprintf("calls[%d] must be an object with a tool name", index))
	}
	name, _ := obj["tool"].(string)
	call.Tool, result.Tool = strings.TrimSpace(name), strings.TrimSpace(name)
	if call.Tool == "" {
		return fail(fmt.Sprintf("calls[%d] has no tool name", index))
	}

	switch v := obj["arguments"].(type) {
	case nil:
		call.Arguments = "{}"
	case string:
		// Accepted for compatibility with calls written when the schema still
		// allowed a string. It is deliberately not advertised anywhere: a call
		// written as an escaped string is the most common way a model produces
		// invalid JSON for the whole batch.
		if s := strings.TrimSpace(v); s == "" {
			call.Arguments = "{}"
		} else if !json.Valid([]byte(s)) {
			return fail(fmt.Sprintf("calls[%d].arguments is not valid JSON — pass that call's arguments as an object, not a JSON string", index))
		} else {
			call.Arguments = s
		}
	default:
		encoded, err := json.Marshal(v)
		if err != nil {
			return fail(fmt.Sprintf("calls[%d].arguments cannot be encoded: %v", index, err))
		}
		call.Arguments = string(encoded)
	}

	if why, refused := parallelRefusedTools[call.Tool]; refused {
		return fail(fmt.Sprintf("%s cannot run inside a batch — %s. Call it on its own.", call.Tool, why))
	}
	if dispatch.Decision != nil && dispatch.Decision(call.Tool) == nanoprotocol.PermissionAsk {
		return fail(fmt.Sprintf("%s needs your approval, and an approval card cannot be shown for a batched call. Call %s on its own.",
			call.Tool, call.Tool))
	}
	if pathWritingTools[call.Tool] {
		var args map[string]any
		if err := json.Unmarshal([]byte(call.Arguments), &args); err == nil {
			if p, ok := args["path"].(string); ok {
				// Clean so "./a" and "a" are the same path: the duplicate check
				// must not be fooled by spelling.
				call.Path = filepath.Clean(p)
			}
		}
	}
	return call, result
}

// render formats the batch: one block per call, in request order, each capped to
// an equal share of the merged-result budget.
func (t *ParallelTool) render(results []parallelResult) string {
	budget := t.MaxChars
	if budget <= 0 {
		budget = defaultParallelBudgetChars
	}
	share := budget / len(results)
	if share < 400 {
		share = 400
	}

	okCount := 0
	for _, r := range results {
		if !r.Error {
			okCount++
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "<parallel count=%q ok=%q>\n", strconv.Itoa(len(results)), strconv.Itoa(okCount))
	for _, r := range results {
		state := "ok"
		if r.Error {
			state = "error"
		}
		fmt.Fprintf(&b, "<call index=%q tool=%q state=%q elapsed_ms=%q>\n",
			strconv.Itoa(r.Index), r.Tool, state, strconv.FormatInt(r.ElapsedMs, 10))
		body := r.Output
		if len(body) > share {
			body = body[:share] + fmt.Sprintf("\n[truncated %d characters]", len(r.Output)-share)
		}
		if body == "" {
			body = "(no output)"
		}
		b.WriteString(body)
		if !strings.HasSuffix(body, "\n") {
			b.WriteString("\n")
		}
		b.WriteString("</call>\n")
	}
	b.WriteString("</parallel>")
	return b.String()
}
