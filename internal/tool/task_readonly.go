package tool

import (
	"context"
	"fmt"
	"strings"

	"gitee.com/kobeyoung81/nanosec/internal/provider"
)

// ReadOnlyTaskTool is the read-only counterpart of TaskTool: it launches
// subagents the caller's own policy cannot turn into writers. The allowed
// subagent types are derived from the profiles (see agent.ReadOnlyProfileIDs)
// and fixed when the tool is built, so a read-only session agent gets an
// explicit, model-visible way to delegate research instead of a delegation that
// is refused at execution time.
type ReadOnlyTaskTool struct {
	base    *TaskTool
	allowed []string
}

// NewReadOnlyTaskTool wraps runner with a fixed set of permitted subagent types.
// An empty set means the tool is not registered at all (the caller decides).
func NewReadOnlyTaskTool(runner TaskRunner, allowed []string) *ReadOnlyTaskTool {
	return &ReadOnlyTaskTool{
		base:    NewTaskTool(runner),
		allowed: append([]string(nil), allowed...),
	}
}

// SetContext implements ContextualTool.
func (t *ReadOnlyTaskTool) SetContext(channel, chatID string) {
	t.base.SetContext(channel, chatID)
}

// SetSessionContext implements SessionContextTool.
func (t *ReadOnlyTaskTool) SetSessionContext(sessionID, channel, chatID string) {
	t.base.SetSessionContext(sessionID, channel, chatID)
}

// SetPolicy implements PolicyContextTool: the caller's policy still attenuates
// the subagents this tool starts.
func (t *ReadOnlyTaskTool) SetPolicy(policy CallerPolicy) {
	t.base.SetPolicy(policy)
}

func (t *ReadOnlyTaskTool) Name() string { return "readonly_task" }

func (t *ReadOnlyTaskTool) Description() string {
	return fmt.Sprintf(`Launch a read-only subagent to research or explore something.

This is the delegation tool for read-only work: the subagents it can start
cannot modify files or run write commands. Available subagent types: %s.
Anything requiring a change belongs in another mode (switch to Build).

It starts exactly one subagent and waits for it. To run several at the same
time, wrap the calls in parallel_execution.

Parameters:
- prompt: the task, complete and self-contained (a subagent starts with no conversation history).
- description: a short (3-5 word) label for its result block.
- subagent_type selects the agent (%s).`,
		strings.Join(t.allowed, ", "), strings.Join(t.allowed, ", "))
}

func (t *ReadOnlyTaskTool) Parameters() provider.ParameterSchema {
	params := t.base.Parameters()
	if desc, ok := params.Properties["subagent_type"]; ok {
		desc.Description = fmt.Sprintf("Subagent type: one of %s", quotedList(t.allowed))
		params.Properties["subagent_type"] = desc
	}
	return params
}

// RawParameters returns the schema with the allowed subagent types inlined, so a
// provider that reads the raw schema cannot be told about a type this mode does
// not offer.
func (t *ReadOnlyTaskTool) RawParameters() []byte {
	types := strings.Join(t.allowed, ", ")
	return []byte(fmt.Sprintf(`{
  "type": "object",
  "properties": {
    "prompt": { "type": "string", "description": "The task for the subagent (complete and self-contained)" },
    "description": { "type": "string", "description": "A short (3-5 word) description of the task" },
    "subagent_type": { "type": "string", "description": "Read-only subagent type, one of: %s" }
  },
  "required": ["prompt"]
}`, types))
}

func (t *ReadOnlyTaskTool) Execute(ctx context.Context, params map[string]any) (string, error) {
	if err := t.checkType(params); err != nil {
		return "", err
	}
	return t.base.Execute(ctx, params)
}

// checkType refuses a requested subagent type outside the read-only set. The
// error names the alternatives so the model can correct itself in one step.
func (t *ReadOnlyTaskTool) checkType(params map[string]any) error {
	requested, _ := params["subagent_type"].(string)
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return nil
	}
	if !containsString(t.allowed, requested) {
		return fmt.Errorf("subagent type %q is not available in this mode; read-only types are: %s",
			requested, strings.Join(t.allowed, ", "))
	}
	return nil
}

func quotedList(items []string) string {
	quoted := make([]string, 0, len(items))
	for _, item := range items {
		quoted = append(quoted, fmt.Sprintf("%q", item))
	}
	return strings.Join(quoted, ", ")
}
