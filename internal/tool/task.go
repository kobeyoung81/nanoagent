package tool

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"gitee.com/kobeyoung81/nanosec/internal/provider"
	"github.com/google/uuid"
)

// CallerPolicy is what the calling agent is allowed to use this turn. It is the
// backstop against privilege escalation through delegation: whatever a subagent
// could do, it may never do more than the agent that launched it.
type CallerPolicy struct {
	// AllowedTools is the set of tool names the calling agent can use this turn
	// (nil = unrestricted).
	AllowedTools map[string]bool
	// DeniesWrite reports that the caller's rules deny the file-writing tools,
	// i.e. the caller is a read-only mode. Such a caller must not hand a
	// write-capable subagent type the work.
	DeniesWrite bool
}

// TaskRequest is one subagent invocation.
type TaskRequest struct {
	SubagentType string
	Description  string
	Prompt       string
}

// TaskRunner runs one subagent. A subagent is an ephemeral "newborn agent" with
// its own context, permissions and turn limit; its result is returned to the
// calling agent.
type TaskRunner interface {
	// Run executes one subagent and blocks until it finishes. channel/chatID
	// identify where its progress events go.
	Run(ctx context.Context, policy CallerPolicy, req TaskRequest, channel, chatID string) (string, error)
}

// AttenuateEnabled narrows a subagent's enabled-tool allowlist by the tool names
// its caller may use: a delegation can never hand the subagent a capability the
// caller lacks. A nil allowed set means the caller is unrestricted (the
// allowlist passes through); a nil enabled set means the profile has no
// allowlist of its own, so the caller's set becomes the bound.
func AttenuateEnabled(enabled, allowed map[string]bool) map[string]bool {
	if allowed == nil {
		return enabled
	}
	if enabled == nil {
		out := make(map[string]bool, len(allowed))
		for name, ok := range allowed {
			if ok {
				out[name] = true
			}
		}
		return out
	}
	out := make(map[string]bool, len(enabled))
	for name := range enabled {
		if allowed[name] {
			out[name] = true
		}
	}
	return out
}

// TaskTool launches one subagent for complex multi-step work. Concurrency is not
// its business: several task calls written in one assistant message run one
// after another, so a caller that wants them at the same time puts them inside
// parallel_execution.
type TaskTool struct {
	runner TaskRunner

	mu        sync.RWMutex
	sessionID string
	channel   string
	chatID    string
	policy    CallerPolicy
}

// NewTaskTool creates a TaskTool backed by the given runner.
func NewTaskTool(runner TaskRunner) *TaskTool {
	return &TaskTool{runner: runner}
}

// SetContext implements ContextualTool.
func (t *TaskTool) SetContext(channel, chatID string) {
	t.mu.Lock()
	t.channel, t.chatID = channel, chatID
	t.mu.Unlock()
}

// SetSessionContext implements SessionContextTool.
func (t *TaskTool) SetSessionContext(sessionID, channel, chatID string) {
	t.mu.Lock()
	t.sessionID, t.channel, t.chatID = sessionID, channel, chatID
	t.mu.Unlock()
}

// SetPolicy implements PolicyContextTool: the calling agent's effective tool set
// and whether it is a read-only mode, refreshed every turn.
func (t *TaskTool) SetPolicy(policy CallerPolicy) {
	t.mu.Lock()
	t.policy = policy
	t.mu.Unlock()
}

func (t *TaskTool) Name() string { return "task" }

func (t *TaskTool) Description() string {
	return `Launch a subagent to handle a complex, multistep task autonomously.

This tool starts exactly one subagent and waits for it. To run several at the
same time, wrap the calls in parallel_execution: several task calls written in
one assistant message run one after another, and a subagent can take minutes.

Parameters:
- prompt: the task, complete and self-contained (a subagent starts with no conversation history).
- description: a short (3-5 word) label for its result block.
- subagent_type selects the agent: "general" (read/write/shell), "explore" (read-only codebase search), "scout" (web research, read-only).

Tell the subagent whether you expect code changes or just research, and how to
verify its work. A subagent cannot delegate further.`
}

func (t *TaskTool) Parameters() provider.ParameterSchema {
	return provider.ParameterSchema{
		Type: "object",
		Properties: map[string]provider.PropertySchema{
			"prompt":        {Type: "string", Description: "The task for the subagent (complete and self-contained)"},
			"description":   {Type: "string", Description: "A short (3-5 word) description of the task"},
			"subagent_type": {Type: "string", Description: `Subagent type: "general", "explore", or "scout"`},
		},
		Required: []string{"prompt"},
	}
}

// RawParameters returns the schema verbatim for providers that read the raw
// form.
func (t *TaskTool) RawParameters() []byte {
	return []byte(`{
  "type": "object",
  "properties": {
    "prompt": { "type": "string", "description": "The task for the subagent (complete and self-contained)" },
    "description": { "type": "string", "description": "A short (3-5 word) description of the task" },
    "subagent_type": { "type": "string", "description": "Subagent type: \"general\", \"explore\", or \"scout\"" }
  },
  "required": ["prompt"]
}`)
}

func (t *TaskTool) Execute(ctx context.Context, params map[string]any) (string, error) {
	req, err := t.parseRequest(params)
	if err != nil {
		return "", err
	}

	t.mu.RLock()
	channel, chatID, policy := t.channel, t.chatID, t.policy
	t.mu.RUnlock()

	id := uuid.NewString()[:8]
	start := time.Now()
	result, err := t.runner.Run(ctx, policy, req, channel, chatID)
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		return RenderTaskResult(id, req.Description, err.Error(), true, elapsed), nil
	}
	return RenderTaskResult(id, req.Description, result, false, elapsed), nil
}

// parseRequest reads the single-task form. A request without a prompt is
// rejected.
func (t *TaskTool) parseRequest(params map[string]any) (TaskRequest, error) {
	prompt, _ := params["prompt"].(string)
	if strings.TrimSpace(prompt) == "" {
		return TaskRequest{}, fmt.Errorf("missing prompt")
	}
	description, _ := params["description"].(string)
	subagentType, _ := params["subagent_type"].(string)
	return TaskRequest{
		SubagentType: defaultSubagentType(subagentType),
		Description:  taskDescription(description, prompt),
		Prompt:       prompt,
	}, nil
}

func defaultSubagentType(t string) string {
	if strings.TrimSpace(t) == "" {
		return "general"
	}
	return strings.TrimSpace(t)
}

// taskDescription falls back to the head of the prompt so every result block
// carries a human-readable label.
func taskDescription(description, prompt string) string {
	if strings.TrimSpace(description) != "" {
		return strings.TrimSpace(description)
	}
	r := []rune(strings.TrimSpace(prompt))
	if len(r) > 40 {
		return string(r[:40]) + "..."
	}
	return string(r)
}

// RenderTaskResult formats a subagent result in the opencode-style <task> block,
// with the wall-clock time it took.
func RenderTaskResult(id, summary, text string, isError bool, elapsedMs int64) string {
	tag := "task_result"
	state := "completed"
	if isError {
		tag = "task_error"
		state = "error"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "<task id=%q state=%q elapsed_ms=%q>\n", id, state, strconv.FormatInt(elapsedMs, 10))
	if summary != "" {
		fmt.Fprintf(&b, "<summary>%s</summary>\n", summary)
	}
	fmt.Fprintf(&b, "<%s>\n%s\n</%s>\n", tag, text, tag)
	b.WriteString("</task>")
	return b.String()
}
