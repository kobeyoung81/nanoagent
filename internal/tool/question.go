package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"gitee.com/kobeyoung81/nanosec/internal/provider"
	"gitee.com/kobeyoung81/nanosec/internal/question"
)

// QuestionWaiter blocks until an answer for the given question is delivered.
// Implemented by question.Broker.
type QuestionWaiter interface {
	Wait(ctx context.Context, id, sessionID string) ([]string, error)
}

// QuestionTool asks the user one or more questions and blocks the turn until
// they answer. The user's reply arrives over the bus carrying the question id
// in its metadata; the agent poll loop routes it to the broker.
type QuestionTool struct {
	broker QuestionWaiter
	// writer is set when the goal surfaces are on: a question that names a
	// check writes its answer into the draft the turn is filling (the draft
	// itself comes from the turn, see turn.go).
	writer *DraftWriter

	mu        sync.RWMutex
	sessionID string
	// headless marks a turn nobody is watching (a scheduled task): a question
	// would block the turn — and the conversation's lock — forever, because
	// there is no user to answer it.
	headless bool
}

// NewQuestionTool creates a QuestionTool backed by the given broker.
func NewQuestionTool(broker QuestionWaiter) *QuestionTool {
	return &QuestionTool{broker: broker}
}

// NewQuestionToolWithIntake adds the intake writer: answers to questions that
// carry a check land in the goal draft without another model round.
func NewQuestionToolWithIntake(broker QuestionWaiter, writer *DraftWriter) *QuestionTool {
	return &QuestionTool{broker: broker, writer: writer}
}

// SetHeadless implements HeadlessTool.
func (t *QuestionTool) SetHeadless(headless bool) {
	t.mu.Lock()
	t.headless = headless
	t.mu.Unlock()
}

// SetSessionContext implements SessionContextTool.
func (t *QuestionTool) SetSessionContext(sessionID, _, _ string) {
	t.mu.Lock()
	t.sessionID = sessionID
	t.mu.Unlock()
}

func (t *QuestionTool) Name() string { return "question" }

func (t *QuestionTool) Description() string {
	return "Ask the user one or more questions when you need a decision, clarification, or preference before continuing. " +
		"Provide options for quick selection (set multiple for multi-select, custom to also show a free-text field) or omit options for a free-text answer. " +
		"Ask at most 5 questions per call, each with at most 8 options. " +
		"The turn blocks until the user answers."
}

// Guidance caps for one call. The card scrolls, so an oversized call is not
// unusable — but a form nobody wants to fill in is not a decision either. They
// are carried in the schema as maxItems; a payload that ignores them is left
// alone (answers come back one per question, so dropping questions would
// misalign them) and only logged.
const (
	MaxQuestionsPerCall   = 5
	MaxOptionsPerQuestion = 8
)

func (t *QuestionTool) Parameters() provider.ParameterSchema {
	return provider.ParameterSchema{
		Type: "object",
		Properties: map[string]provider.PropertySchema{
			"questions": {Type: "array", Description: "Questions to ask (array of {question, header, options, multiple, custom}); at most 5 questions, each with at most 8 options"},
		},
		Required: []string{"questions"},
	}
}

// RawParameters returns the questions JSON schema verbatim.
func (t *QuestionTool) RawParameters() []byte {
	return []byte(`{
  "type": "object",
  "properties": {
    "questions": {
      "type": "array",
      "description": "Questions to ask the user (at most 5)",
      "maxItems": 5,
      "items": {
        "type": "object",
        "properties": {
          "question": {"type": "string", "description": "The question to ask"},
          "header": {"type": "string", "description": "Very short label (max 30 chars)"},
          "options": {
            "type": "array",
            "description": "Selectable answers (at most 8); omit for a free-text question",
            "maxItems": 8,
            "items": {
              "type": "object",
              "properties": {
                "label": {"type": "string", "description": "Display text (1-5 words)"},
                "description": {"type": "string", "description": "Explanation of the choice"}
              },
              "required": ["label"]
            }
          },
          "multiple": {"type": "boolean", "description": "Allow selecting more than one option"},
          "custom": {"type": "boolean", "description": "Also allow a free-text answer; the card shows a free-text field next to the options"},
          "check": {"type": "string", "enum": ["condition", "proof", "approach", "constraints", "budget"], "description": "Goal-intake only: the draft field this question fills. The answer is saved as soon as the user submits, so ask up to three of these per round and do not call goal_intake_answer for them"}
        },
        "required": ["question"]
      }
    }
  },
  "required": ["questions"]
}`)
}

func (t *QuestionTool) Execute(ctx context.Context, params map[string]any) (string, error) {
	if _, ok := params["questions"].([]any); !ok {
		return "", fmt.Errorf("missing or invalid questions array")
	}

	t.mu.RLock()
	sessionID := t.sessionID
	headless := t.headless
	t.mu.RUnlock()

	if headless {
		return "", fmt.Errorf("no user is available in this scheduled run; finish the task and report the result instead of asking")
	}

	id, ok := question.QuestionID(ctx)
	if !ok || id == "" {
		return "", fmt.Errorf("question id missing from context")
	}

	answers, err := t.broker.Wait(ctx, id, sessionID)
	if err != nil {
		return "", fmt.Errorf("waiting for answer: %w", err)
	}
	if len(answers) == 0 {
		return "The user did not provide an answer.", nil
	}
	out, _ := json.Marshal(answers)
	if note := t.writeMarkedAnswers(ctx, id, params, answers); note != "" {
		return string(out) + "\n\n" + note, nil
	}
	return string(out), nil
}

// writeMarkedAnswers saves the answers of the questions that named a check.
//
// One write per round, and the result is handed back to the model as part of the
// tool output: without it the model would have to ask again whether the answers
// landed, which is exactly the loop that filled the transcript with the same
// three questions.
func (t *QuestionTool) writeMarkedAnswers(ctx context.Context, id string, params map[string]any, answers []string) string {
	if t.writer == nil {
		return ""
	}
	turn := TurnFrom(ctx)
	if turn == nil || turn.Intake.DraftID == "" {
		return ""
	}
	draft := turn.Intake.DraftID
	raw, err := json.Marshal(params["questions"])
	if err != nil {
		return ""
	}
	req, ok := ParseQuestions(id, `{"questions":`+string(raw)+`}`)
	if !ok {
		return ""
	}
	marked := make([]DraftAnswer, 0, len(req.Questions))
	for i, q := range req.Questions {
		if q.Check == "" || i >= len(answers) {
			continue
		}
		if strings.TrimSpace(answers[i]) == "" {
			continue
		}
		marked = append(marked, DraftAnswer{Check: q.Check, Value: answers[i]})
	}
	if len(marked) == 0 {
		return ""
	}
	note, err := t.writer.Write(ctx, draft, marked)
	if err != nil {
		return "写入草稿失败：" + err.Error()
	}
	return note
}

// ExceedsGuidanceCaps reports whether a parsed call went past the guidance
// caps, so the caller can log it. Nothing is dropped here: answers come back
// one per question, so trimming the list would misalign them.
func ExceedsGuidanceCaps(req question.Request) bool {
	if len(req.Questions) > MaxQuestionsPerCall {
		return true
	}
	for _, q := range req.Questions {
		if len(q.Options) > MaxOptionsPerQuestion {
			return true
		}
	}
	return false
}

// ParseQuestions extracts a normalized question request from raw tool arguments.
func ParseQuestions(id, arguments string) (question.Request, bool) {
	var req question.Request
	if err := json.Unmarshal([]byte(arguments), &req); err != nil {
		return question.Request{}, false
	}
	if len(req.Questions) == 0 {
		return question.Request{}, false
	}
	for i := range req.Questions {
		req.Questions[i].Header = strings.TrimSpace(req.Questions[i].Header)
	}
	req.ID = id
	return req, true
}
