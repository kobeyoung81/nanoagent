package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"gitee.com/kobeyoung81/nanosec/internal/provider"
)

// GoalIntakeTool writes a round of answers into the goal draft being created.
//
// The intake turn asks at most three questions with the blocking question tool;
// a question that names its check is written as soon as the user answers, and
// whatever was collected some other way lands here — one call per round, so a
// round costs one request instead of one per answer.
//
// The draft is bound to the turn by the host (see IntakeContext), so the model
// does not have to carry an id it read in a message: passing an id is still
// allowed, and only matters when a turn carries no binding.
type GoalIntakeTool struct {
	writer *DraftWriter
}

// NewGoalIntakeTool creates the tool for one account.
func NewGoalIntakeTool(writer *DraftWriter) *GoalIntakeTool {
	return &GoalIntakeTool{writer: writer}
}

func (t *GoalIntakeTool) Name() string { return "goal_intake_answer" }

func (t *GoalIntakeTool) Description() string {
	return `Save answers into the goal draft being created (the intake step). Call it once per round with every answer you have.

The six checks are condition(完成条件), proof(怎么证明), approach(怎么走), constraints(不许动), budget(预算) and permissions(宿主判定, never asked). A question that carries a check writes itself when the user answers, so use this for the answers you collected any other way, or to correct one.

budget accepts "3 轮 / 10 分钟" or {"rounds":3,"minutes":10}; "你定" takes the default and the result says so. proof accepts lines of text or [{"kind":"command","command":"…"}] steps.

The draft is bound to this turn: draft_id is only needed when no draft is bound. A 404 means the draft is gone — report it and stop, do not retry.
`
}

func (t *GoalIntakeTool) Parameters() provider.ParameterSchema {
	return provider.ParameterSchema{
		Type: "object",
		Properties: map[string]provider.PropertySchema{
			"answers": {
				Type:        "array",
				Description: "One entry per check: {check, value}. At most six. check is the key or its Chinese label.",
			},
			"draft_id": {
				Type:        "string",
				Description: "Only when this turn carries no bound draft (normally omit it)",
			},
		},
		Required: []string{"answers"},
	}
}

// RawParameters returns the answers schema verbatim, with the checks enumerated
// so a model does not have to guess a label.
func (t *GoalIntakeTool) RawParameters() []byte {
	return []byte(`{
  "type": "object",
  "properties": {
    "answers": {
      "type": "array",
      "description": "One entry per check (at most six)",
      "maxItems": 6,
      "items": {
        "type": "object",
        "properties": {
          "check": {"type": "string", "enum": ["condition", "proof", "approach", "constraints", "budget", "permissions"], "description": "condition=完成条件, proof=怎么证明, approach=怎么走, constraints=不许动, budget=预算"},
          "value": {"type": ["string", "array", "object"], "description": "The answer verbatim; a list is one item per entry, budget may be {\"rounds\":n,\"minutes\":m}"}
        },
        "required": ["check", "value"]
      }
    },
    "draft_id": {"type": "string", "description": "Only when no draft is bound to this turn"}
  },
  "required": ["answers"]
}`)
}

func (t *GoalIntakeTool) Execute(ctx context.Context, params map[string]any) (string, error) {
	answers, err := intakeAnswers(params)
	if err != nil {
		return "", err
	}
	draftID := strings.TrimSpace(stringField(params, "draft_id"))
	if draftID == "" {
		// The draft the turn was started with. It is on the turn, not on the
		// tool, because several turns run at once.
		if turn := TurnFrom(ctx); turn != nil {
			draftID = turn.Intake.DraftID
		}
	}
	if draftID == "" {
		return "", fmt.Errorf("no draft is bound to this turn; the user must reopen 「设定长程目标」")
	}
	return t.writer.Write(ctx, draftID, answers)
}

// intakeAnswers reads the batch, and the single {check, value} shape a model may
// still send out of habit.
func intakeAnswers(params map[string]any) ([]DraftAnswer, error) {
	if list, ok := params["answers"].([]any); ok {
		if len(list) == 0 {
			return nil, fmt.Errorf("answers is empty")
		}
		if len(list) > 6 {
			return nil, fmt.Errorf("at most six answers, one per check")
		}
		out := make([]DraftAnswer, 0, len(list))
		for i, item := range list {
			entry, ok := item.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("answers[%d] must be an object with check and value", i)
			}
			check := strings.TrimSpace(fmt.Sprint(entry["check"]))
			if check == "" {
				return nil, fmt.Errorf("answers[%d] has no check", i)
			}
			value, ok := entry["value"]
			if !ok {
				return nil, fmt.Errorf("answers[%d] has no value", i)
			}
			out = append(out, DraftAnswer{Check: check, Value: value})
		}
		return out, nil
	}
	// One answer, the shape this tool had before it could take a round.
	check := strings.TrimSpace(stringField(params, "check"))
	if check == "" {
		return nil, fmt.Errorf("answers is required")
	}
	value, ok := params["value"]
	if !ok {
		return nil, fmt.Errorf("value is required")
	}
	return []DraftAnswer{{Check: check, Value: value}}, nil
}

func stringField(params map[string]any, key string) string {
	if v, ok := params[key].(string); ok {
		return v
	}
	return ""
}

// jsonValue keeps a value as it arrived so the workspace can read a string, a
// list or an object from the same field.
func jsonValue(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return raw
}
