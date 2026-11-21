package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"gitee.com/kobeyoung81/nanosec/internal/provider"
)

// GoalReportTool is how a round of a long-running goal ends.
//
// The tool is the only path from the agent's own account of its work to the
// host: the model says what it believes it achieved, and every claim has to
// carry evidence the host can check. In this batch the tool validates the report
// and echoes the normalised form back to the model; the judge call and the POST
// to the workspace arrive with the scheduler, and the tool's contract does not
// change when they do.
type GoalReportTool struct {
	mu  sync.Mutex
	ctx GoalContext
}

// GoalContext is the turn's goal binding, injected by the loop.
//
// It is per turn and per tool instance on purpose: the agent runs several
// messages concurrently, so a value held on the agent would let one goal's round
// number leak into another conversation's report.
type GoalContext struct {
	GoalID string
	Round  int
	Verify bool
	// WorkspaceURL is where the report is posted.
	WorkspaceURL string
	// Token authenticates that POST.
	Token string
}

// NewGoalReportTool creates the tool.
func NewGoalReportTool() *GoalReportTool { return &GoalReportTool{} }

// SetGoalContext implements GoalContextTool.
func (t *GoalReportTool) SetGoalContext(c GoalContext) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ctx = c
}

// context returns the current binding, or the zero value outside a goal round.
func (t *GoalReportTool) context() GoalContext {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ctx
}

func (t *GoalReportTool) Name() string { return "goal_report" }

func (t *GoalReportTool) Description() string {
	return `End the current round of a long-running goal by reporting what this round achieved.

Call this exactly once, as the last action of the round. The report is checked by a separate judge against the goal's condition; a claim without evidence is rejected, and a round that produces no report simply leaves the goal waiting.

Evidence kinds:
- command: a command you ran this round — the command line, its exit code and the tail of its output. The judge compares each command with what this round actually ran.
- file: a file you changed, by path, with a one-line note.
- list: a checklist you worked through (items with done/not-done).

Do not report an interrupted round: only the platform may record that, and it does so itself.`
}

func (t *GoalReportTool) Parameters() provider.ParameterSchema {
	return provider.ParameterSchema{
		Type: "object",
		Properties: map[string]provider.PropertySchema{
			"summary":   {Type: "string", Description: "What this round achieved, in one or two sentences"},
			"evidence":  {Type: "array", Description: "What backs the summary (array of {kind, …}: command|file|list)"},
			"remaining": {Type: "array", Description: "What is still missing, one line each"},
		},
		Required: []string{"summary", "evidence"},
	}
}

// RawParameters returns the nested schema verbatim: the structured schema type
// cannot express an array of objects.
func (t *GoalReportTool) RawParameters() []byte {
	return []byte(`{
  "type": "object",
  "properties": {
    "summary": {"type": "string", "description": "What this round achieved, in one or two sentences"},
    "evidence": {
      "type": "array",
      "description": "What backs the summary; at least one item",
      "items": {
        "type": "object",
        "properties": {
          "kind": {"type": "string", "enum": ["command", "file", "list"]},
          "cmd": {"type": "string", "description": "The command line, for kind=command"},
          "exit": {"type": "integer", "description": "Its exit code, for kind=command"},
          "tail": {"type": "string", "description": "The last lines of its output, for kind=command"},
          "path": {"type": "string", "description": "The file path, for kind=file"},
          "note": {"type": "string", "description": "One line about the file, for kind=file"},
          "items": {
            "type": "array",
            "description": "The checklist, for kind=list",
            "items": {
              "type": "object",
              "properties": {
                "text": {"type": "string"},
                "done": {"type": "boolean"}
              },
              "required": ["text", "done"]
            }
          }
        },
        "required": ["kind"]
      }
    },
    "remaining": {"type": "array", "items": {"type": "string"}, "description": "What is still missing, one line each"}
  },
  "required": ["summary", "evidence"]
}`)
}

// Evidence is one item of a round report (design §09).
type Evidence struct {
	Kind string `json:"kind"`
	// command
	Cmd  string `json:"cmd,omitempty"`
	Exit *int   `json:"exit,omitempty"`
	Tail string `json:"tail,omitempty"`
	// file
	Path string `json:"path,omitempty"`
	Note string `json:"note,omitempty"`
	// list
	Items []ListItem `json:"items,omitempty"`
}

// ListItem is one line of a checklist evidence.
type ListItem struct {
	Text string `json:"text"`
	Done bool   `json:"done"`
}

// Report is the normalised round report.
type Report struct {
	GoalID    string     `json:"goal_id"`
	Round     int        `json:"round"`
	Verify    bool       `json:"verify,omitempty"`
	Summary   string     `json:"summary"`
	Evidence  []Evidence `json:"evidence"`
	Remaining []string   `json:"remaining,omitempty"`
}

// evidenceKinds is the closed set of things a round may point at. "interrupted"
// is deliberately absent: an interrupted round is a fact the platform records
// when it stops a turn, and letting the agent submit one would give it a second
// way to end a goal.
var evidenceKinds = map[string]bool{
	"command": true,
	"file":    true,
	"list":    true,
}

func (t *GoalReportTool) Execute(_ context.Context, params map[string]any) (string, error) {
	gc := t.context()
	if gc.GoalID == "" {
		return "", fmt.Errorf("goal_report is only available inside a long-running goal round")
	}
	report, err := parseReport(params)
	if err != nil {
		return "", err
	}
	report.GoalID = gc.GoalID
	report.Round = gc.Round
	report.Verify = gc.Verify

	out, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// parseReport applies the L1 checks: the shape the judge is promised.
func parseReport(params map[string]any) (Report, error) {
	var r Report
	summary, _ := params["summary"].(string)
	if strings.TrimSpace(summary) == "" {
		return r, fmt.Errorf("summary is required")
	}
	r.Summary = strings.TrimSpace(summary)

	raw, ok := params["evidence"].([]any)
	if !ok || len(raw) == 0 {
		return r, fmt.Errorf("evidence is required: report what backs the summary")
	}
	for i, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			return r, fmt.Errorf("evidence[%d] must be an object", i)
		}
		kind, _ := m["kind"].(string)
		if !evidenceKinds[kind] {
			return r, fmt.Errorf("evidence[%d].kind %q is invalid (command|file|list)", i, kind)
		}
		ev := Evidence{Kind: kind}
		switch kind {
		case "command":
			ev.Cmd, _ = m["cmd"].(string)
			if strings.TrimSpace(ev.Cmd) == "" {
				return r, fmt.Errorf("evidence[%d] of kind command needs a cmd", i)
			}
			// The exit code is what makes a command checkable: without it the
			// judge cannot tell "tests pass" from "tests ran".
			v, ok := m["exit"]
			if !ok {
				return r, fmt.Errorf("evidence[%d] of kind command needs an exit code", i)
			}
			f, ok := v.(float64)
			if !ok {
				return r, fmt.Errorf("evidence[%d].exit must be a number", i)
			}
			code := int(f)
			ev.Exit = &code
			ev.Tail, _ = m["tail"].(string)
		case "file":
			ev.Path, _ = m["path"].(string)
			if strings.TrimSpace(ev.Path) == "" {
				return r, fmt.Errorf("evidence[%d] of kind file needs a path", i)
			}
			ev.Note, _ = m["note"].(string)
		case "list":
			rawItems, ok := m["items"].([]any)
			if !ok || len(rawItems) == 0 {
				return r, fmt.Errorf("evidence[%d] of kind list needs items", i)
			}
			for j, item := range rawItems {
				im, ok := item.(map[string]any)
				if !ok {
					return r, fmt.Errorf("evidence[%d].items[%d] must be an object", i, j)
				}
				text, _ := im["text"].(string)
				if strings.TrimSpace(text) == "" {
					return r, fmt.Errorf("evidence[%d].items[%d].text is required", i, j)
				}
				done, _ := im["done"].(bool)
				ev.Items = append(ev.Items, ListItem{Text: strings.TrimSpace(text), Done: done})
			}
		}
		r.Evidence = append(r.Evidence, ev)
	}

	if raw, ok := params["remaining"].([]any); ok {
		for _, item := range raw {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				r.Remaining = append(r.Remaining, strings.TrimSpace(s))
			}
		}
	}
	return r, nil
}
