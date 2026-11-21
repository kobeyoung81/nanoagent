package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"gitee.com/kobeyoung81/nanosec/internal/provider"
)

// GoalReportTool is how a round of a long-running goal ends.
//
// It is the only path from the agent's own account of its work to the host: the
// model says what it believes it achieved, every claim has to carry evidence the
// host can check, and a separate judge — a one-shot utility with no tools —
// turns the evidence into one of three verdicts. The report then goes back to
// the workspace, which owns the goal state.
type GoalReportTool struct {
	mu  sync.Mutex
	ctx GoalContext

	// judge runs the goal_judge utility. nil means the utility is not
	// registered, and the tool refuses rather than self-certifying.
	judge func(ctx context.Context, input string) (string, bool)

	workspaceURL string
	token        string
	http         *http.Client
}

// GoalReportOptions is where a report is posted.
type GoalReportOptions struct {
	WorkspaceURL string
	Token        string
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
	// Instruction is the text this round was started with: the condition, the
	// constraints and the context the scheduler injected. The judge reads it as
	// the definition of "done".
	Instruction string
}

// NewGoalReportTool creates the tool.
func NewGoalReportTool(opts ...GoalReportOptions) *GoalReportTool {
	t := &GoalReportTool{http: &http.Client{Timeout: 15 * time.Second}}
	if len(opts) > 0 {
		t.workspaceURL = strings.TrimRight(opts[0].WorkspaceURL, "/")
		t.token = opts[0].Token
	}
	return t
}

// SetGoalJudge installs the utility runner used to judge a round.
func (t *GoalReportTool) SetGoalJudge(fn func(ctx context.Context, input string) (string, bool)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.judge = fn
}

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

func (t *GoalReportTool) judgeFunc() func(context.Context, string) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.judge
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

// Report is the normalised round report, and the body posted to the workspace.
type Report struct {
	GoalID    string     `json:"goal_id,omitempty"`
	Round     int        `json:"round"`
	Verify    bool       `json:"verify,omitempty"`
	Summary   string     `json:"summary"`
	Evidence  []Evidence `json:"evidence"`
	Remaining []string   `json:"remaining,omitempty"`
	// Verdict, Reason and Missing come from the judge.
	Verdict string   `json:"verdict"`
	Reason  string   `json:"reason,omitempty"`
	Missing []string `json:"missing,omitempty"`
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

func (t *GoalReportTool) Execute(ctx context.Context, params map[string]any) (string, error) {
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

	judge := t.judgeFunc()
	if judge == nil {
		return "", fmt.Errorf("goal_report is unavailable: the goal judge is not configured")
	}
	raw, ok := judge(ctx, judgeInput(gc, report))
	if !ok {
		return "", fmt.Errorf("goal_report is unavailable: the goal judge could not be run")
	}
	verdict, reason, missing := parseVerdict(raw)
	report.Verdict, report.Reason, report.Missing = verdict, reason, missing

	// A judged report that is not delivered is a lost round, not a success: the
	// scheduler would re-dispatch the same round after the claim expires. An
	// unconfigured URL therefore fails instead of returning quietly.
	if t.workspaceURL == "" {
		return "", fmt.Errorf("goal_report is unavailable: the workspace url is not configured")
	}
	if err := t.post(ctx, report); err != nil {
		return "", fmt.Errorf("the report was judged but not delivered: %w", err)
	}
	return fmt.Sprintf("第 %d 轮报告已提交，判定：%s。%s", gc.Round+1, verdict, reason), nil
}

// judgeInput renders the four sections the judge prompt expects.
func judgeInput(gc GoalContext, r Report) string {
	condition, constraints := splitInstruction(gc.Instruction)
	var b strings.Builder
	b.WriteString("CONDITION\n")
	b.WriteString(condition)
	b.WriteString("\n\nCONSTRAINTS\n")
	b.WriteString(constraints)
	b.WriteString("\n\nTHIS ROUND\n")
	fmt.Fprintf(&b, "round: %d\n", gc.Round+1)
	if gc.Verify {
		b.WriteString("kind: verification — the condition was reported met; this round re-proves it\n")
	} else {
		b.WriteString("kind: work\n")
	}
	fmt.Fprintf(&b, "summary: %s\n", r.Summary)
	b.WriteString("evidence:\n")
	for _, e := range r.Evidence {
		switch e.Kind {
		case "command":
			exit := "?"
			if e.Exit != nil {
				exit = fmt.Sprintf("%d", *e.Exit)
			}
			fmt.Fprintf(&b, "  - command `%s` exit %s\n", e.Cmd, exit)
		case "file":
			fmt.Fprintf(&b, "  - file %s", e.Path)
			if e.Note != "" {
				fmt.Fprintf(&b, " (%s)", e.Note)
			}
			b.WriteString("\n")
		case "list":
			for _, it := range e.Items {
				state := "todo"
				if it.Done {
					state = "done"
				}
				fmt.Fprintf(&b, "  - list [%s] %s\n", state, it.Text)
			}
		}
	}
	if len(r.Remaining) > 0 {
		fmt.Fprintf(&b, "remaining: %s\n", strings.Join(r.Remaining, "; "))
	}
	b.WriteString("\nRECENT\n(no earlier rounds are included)\n")
	return b.String()
}

// splitInstruction pulls the condition and the constraint list out of the text
// the scheduler injected. The wording is this platform's own, so the headings
// are stable; an unrecognised shape keeps the whole text as the condition rather
// than losing it.
func splitInstruction(instruction string) (condition, constraints string) {
	text := strings.TrimSpace(instruction)
	if text == "" {
		return "(the round carried no instructions)", "(none)"
	}
	condition = text
	constraints = "(see CONDITION)"

	if i := strings.Index(text, "判据（全部满足才算完成）："); i >= 0 {
		rest := text[i+len("判据（全部满足才算完成）："):]
		if j := strings.Index(rest, "\n约束："); j >= 0 {
			condition = strings.TrimSpace(rest[:j])
			tail := rest[j+len("\n约束："):]
			if k := strings.Index(tail, "\n\n"); k >= 0 {
				tail = tail[:k]
			}
			if c := strings.TrimSpace(tail); c != "" {
				constraints = c
			}
		} else if k := strings.Index(rest, "\n\n"); k >= 0 {
			condition = strings.TrimSpace(rest[:k])
		} else {
			condition = strings.TrimSpace(rest)
		}
	}
	return condition, constraints
}

// parseVerdict reads the judge's JSON answer. A malformed answer is a "not_yet"
// — the goal keeps moving, and the host never mistakes an unreadable reply for
// success.
func parseVerdict(raw string) (verdict, reason string, missing []string) {
	text := strings.TrimSpace(raw)
	if i := strings.Index(text, "{"); i >= 0 {
		text = text[i:]
	}
	if j := strings.LastIndex(text, "}"); j >= 0 {
		text = text[:j+1]
	}
	var payload struct {
		Verdict string   `json:"verdict"`
		Reason  string   `json:"reason"`
		Missing []string `json:"missing"`
	}
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		return "not_yet", "judge answer unreadable: " + truncate(raw, 80), nil
	}
	switch payload.Verdict {
	case "met", "not_yet", "impossible":
		return payload.Verdict, truncate(payload.Reason, 200), payload.Missing
	default:
		return "not_yet", "judge answered an unknown verdict: " + truncate(raw, 80), payload.Missing
	}
}

func truncate(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}

// post sends the report to the workspace, which owns the goal.
func (t *GoalReportTool) post(ctx context.Context, r Report) error {
	if t.workspaceURL == "" {
		return fmt.Errorf("the workspace url is not configured")
	}
	body, err := json.Marshal(r)
	if err != nil {
		return err
	}
	endpoint := t.workspaceURL + "/api/v1/workspace/goals/" + url.PathEscape(r.GoalID) + "/report"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if t.token != "" {
		req.Header.Set("Authorization", "Bearer "+t.token)
	}
	resp, err := t.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("workspace answered %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
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
