package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"gitee.com/kobeyoung81/nanosec/internal/provider"
)

// TodoWriteTool creates and maintains a structured task list for the session.
// The agent loop persists the returned list to the session metadata and emits
// a todo chunk so the portal can render progress. The full JSON schema is
// supplied via RawParameters because the structured schema type cannot express
// an array of objects.
type TodoWriteTool struct{}

// NewTodoWriteTool creates a TodoWriteTool.
func NewTodoWriteTool() *TodoWriteTool { return &TodoWriteTool{} }

func (t *TodoWriteTool) Name() string { return "todowrite" }

func (t *TodoWriteTool) Description() string {
	return `Create and maintain a structured task list for the current session. Tracks progress, organizes multi-step work, and surfaces status to the user.

Use proactively when the task requires 3+ distinct steps, when the work is non-trivial, when the user provides multiple tasks, or when new instructions arrive. Skip for single, straightforward tasks or purely informational requests.

States: pending (not started), in_progress (exactly ONE at a time), completed (finished and verified), cancelled (no longer needed).

Rules:
- Update status in real time; don't batch completions.
- Mark completed only after the work is actually done.
- Keep exactly one in_progress while work remains.
- Items should be specific and actionable.`
}

func (t *TodoWriteTool) Parameters() provider.ParameterSchema {
	return provider.ParameterSchema{
		Type: "object",
		Properties: map[string]provider.PropertySchema{
			"todos": {Type: "array", Description: "The updated todo list (array of {content, status, priority})"},
		},
		Required: []string{"todos"},
	}
}

// RawParameters returns the array-of-objects JSON schema verbatim.
func (t *TodoWriteTool) RawParameters() []byte {
	return []byte(`{
  "type": "object",
  "properties": {
    "todos": {
      "type": "array",
      "description": "The updated todo list",
      "items": {
        "type": "object",
        "properties": {
          "content": {"type": "string", "description": "Brief description of the task"},
          "status": {"type": "string", "enum": ["pending", "in_progress", "completed", "cancelled"], "description": "Current status of the task"},
          "priority": {"type": "string", "enum": ["high", "medium", "low"], "description": "Priority level of the task"}
        },
        "required": ["content", "status"]
      }
    }
  },
  "required": ["todos"]
}`)
}

// TodoItem is a normalized todo entry persisted to the session.
type TodoItem struct {
	Content  string `json:"content"`
	Status   string `json:"status"`
	Priority string `json:"priority,omitempty"`
}

var validTodoStatus = map[string]bool{
	"pending":     true,
	"in_progress": true,
	"completed":   true,
	"cancelled":   true,
}

func (t *TodoWriteTool) Execute(_ context.Context, params map[string]any) (string, error) {
	raw, ok := params["todos"].([]any)
	if !ok {
		return "", fmt.Errorf("missing or invalid todos array")
	}

	items := make([]TodoItem, 0, len(raw))
	inProgress := 0
	for i, r := range raw {
		m, ok := r.(map[string]any)
		if !ok {
			return "", fmt.Errorf("todos[%d] must be an object", i)
		}
		content, _ := m["content"].(string)
		if strings.TrimSpace(content) == "" {
			return "", fmt.Errorf("todos[%d].content is required", i)
		}
		status, _ := m["status"].(string)
		if !validTodoStatus[status] {
			return "", fmt.Errorf("todos[%d].status %q is invalid (pending|in_progress|completed|cancelled)", i, status)
		}
		if status == "in_progress" {
			inProgress++
		}
		priority, _ := m["priority"].(string)
		items = append(items, TodoItem{Content: content, Status: status, Priority: priority})
	}
	if inProgress > 1 {
		return "", fmt.Errorf("only one todo may be in_progress (got %d)", inProgress)
	}

	out, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return "", err
	}
	return string(out), nil
}
