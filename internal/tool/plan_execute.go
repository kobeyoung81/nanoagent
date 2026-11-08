package tool

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gitee.com/kobeyoung81/nanosec/internal/provider"
)

// PlanExecuteTool closes out the plan artefact produced in plan mode.
//
// The build agent is told (mode note + prompt) to read the plan it inherits, and
// this is the other half of that contract: mark it executed, record what was
// actually done next to it, and let the loop retire the mode note. Without it
// the note was injected on every turn for the rest of the session, still
// pointing at finished work.
type PlanExecuteTool struct {
	Workspace       string
	SharedWorkspace string

	// planPath is the session's current plan artefact (last_plan_path). It is
	// set per turn by the agent loop through the registry — the model never
	// names the file, so it cannot close out a plan it was not given.
	mu       sync.Mutex
	planPath string
}

// NewPlanExecuteTool creates a PlanExecuteTool rooted at the agent workspace.
func NewPlanExecuteTool(workspace, sharedWorkspace string) *PlanExecuteTool {
	return &PlanExecuteTool{Workspace: workspace, SharedWorkspace: sharedWorkspace}
}

func (t *PlanExecuteTool) Name() string { return "plan_execute" }

func (t *PlanExecuteTool) Description() string {
	return `Mark the plan artefact you were given as executed and record what you did.

Call this when the plan's work is finished (or when you stop): it appends an
execution report next to the plan file (plans/<slug>.report.md) and retires the
mode note that points at it, so the conversation stops being anchored to a
finished plan.

status: "done" when every item is complete, "partial" when work remains or you
deviated, "abandoned" when the plan was dropped. Say so explicitly in the
summary when you deviated from the plan.`
}

func (t *PlanExecuteTool) Parameters() provider.ParameterSchema {
	return provider.ParameterSchema{
		Type: "object",
		Properties: map[string]provider.PropertySchema{
			"status":  {Type: "string", Description: `One of "done", "partial", "abandoned"`},
			"summary": {Type: "string", Description: "One-line outcome, including any deviation from the plan"},
			"report":  {Type: "string", Description: "Optional evidence: commands run, what they printed, anything left open"},
		},
		Required: []string{"status", "summary"},
	}
}

// SetPlanPath records the session's current plan artefact path.
func (t *PlanExecuteTool) SetPlanPath(path string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.planPath = path
}

func (t *PlanExecuteTool) currentPlanPath() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.planPath
}

// planExecutedMarker is the prefix of the tool result carrying the plan that was
// closed; the agent loop reads it back to retire the mode note.
const planExecutedMarker = "Plan marked executed: "

// PlanExecutedPath extracts the plan path from a plan_execute result (empty when
// the result is not a successful plan_execute).
func PlanExecutedPath(result string) string {
	i := strings.Index(result, planExecutedMarker)
	if i < 0 {
		return ""
	}
	rest := result[i+len(planExecutedMarker):]
	if j := strings.Index(rest, " ("); j >= 0 {
		rest = rest[:j]
	}
	return strings.TrimSpace(rest)
}

// planExecuteStatuses are the accepted outcomes.
var planExecuteStatuses = map[string]bool{"done": true, "partial": true, "abandoned": true}

func (t *PlanExecuteTool) Execute(_ context.Context, params map[string]any) (string, error) {
	status, _ := params["status"].(string)
	status = strings.ToLower(strings.TrimSpace(status))
	if !planExecuteStatuses[status] {
		return "", fmt.Errorf(`status must be one of "done", "partial", "abandoned"`)
	}
	summary, _ := params["summary"].(string)
	if strings.TrimSpace(summary) == "" {
		return "", fmt.Errorf("summary is required")
	}
	report, _ := params["report"].(string)

	planPath := t.currentPlanPath()
	if planPath == "" {
		return "", fmt.Errorf("no plan artefact is associated with this session")
	}
	planDir := filepath.Clean(filepath.Dir(planPath))
	if info, err := os.Stat(planPath); err != nil || info.IsDir() {
		return "", fmt.Errorf("plan artefact %q is not readable", planPath)
	}

	reportPath := filepath.Join(planDir, strings.TrimSuffix(filepath.Base(planPath), filepath.Ext(planPath))+".report.md")
	if clean, ok := confinedPath(planDir, reportPath); !ok {
		return "", fmt.Errorf("invalid report path %q", clean)
	}

	var b strings.Builder
	if _, err := os.Stat(reportPath); err != nil {
		b.WriteString("# Execution report — " + filepath.Base(planPath) + "\n")
	}
	b.WriteString("\n## " + time.Now().Format("2006-01-02 15:04:05") + " — " + status + "\n\n")
	b.WriteString(strings.TrimSpace(summary) + "\n")
	if body := strings.TrimSpace(report); body != "" {
		b.WriteString("\n" + body + "\n")
	}

	f, err := os.OpenFile(reportPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return "", fmt.Errorf("write execution report: %w", err)
	}
	defer f.Close()
	if _, err := f.WriteString(b.String()); err != nil {
		return "", fmt.Errorf("write execution report: %w", err)
	}
	return fmt.Sprintf("%s%s (%s). Report appended to %s.", planExecutedMarker, planPath, status, reportPath), nil
}

// confinedPath reports whether path stays inside dir, returning the cleaned path
// for the error message either way.
func confinedPath(dir, path string) (string, bool) {
	cleanDir, clean := filepath.Clean(dir), filepath.Clean(path)
	return clean, strings.HasPrefix(clean, cleanDir+string(os.PathSeparator))
}
