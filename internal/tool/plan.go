package tool

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gitee.com/kobeyoung81/nanosec/internal/provider"
)

// PlanWriteTool persists a plan artefact for the read-only Plan session agent.
//
// Plan mode denies every other write/side-effect tool, so without a dedicated
// tool its output lives only in the transcript — and is lost as soon as the
// session is archived/split or the mode changes. This tool gives the plan agent
// exactly one write capability, scoped to <workspace>/plans/, so the plan
// survives as a durable file the build agent is told to read.
type PlanWriteTool struct {
	Workspace       string
	SharedWorkspace string
}

// NewPlanWriteTool creates a PlanWriteTool rooted at the agent workspace.
func NewPlanWriteTool(workspace, sharedWorkspace string) *PlanWriteTool {
	return &PlanWriteTool{Workspace: workspace, SharedWorkspace: sharedWorkspace}
}

func (t *PlanWriteTool) Name() string { return "plan_write" }

func (t *PlanWriteTool) Description() string {
	return `Write the plan artefact for the current planning session.

Use this instead of the generic write/edit tools: in plan mode it is the only
way to persist output, and the build agent is instructed to read the newest plan
file before it starts work. The file is written to plans/<slug>.md inside the
agent workspace.

Provide a short kebab-case slug (e.g. "fix-session-split"), an optional one-line
title, and the full plan as markdown. Calling it again with the same slug
replaces that plan.`
}

func (t *PlanWriteTool) Parameters() provider.ParameterSchema {
	return provider.ParameterSchema{
		Type: "object",
		Properties: map[string]provider.PropertySchema{
			"slug":    {Type: "string", Description: "Short kebab-case file slug, e.g. fix-session-split"},
			"title":   {Type: "string", Description: "One-line plan title (rendered as the document heading)"},
			"content": {Type: "string", Description: "The plan, as markdown"},
		},
		Required: []string{"content"},
	}
}

// planPathMarker is the prefix of the tool result that carries the written
// path; the agent loop reads it back to remember the latest plan file so a
// later mode switch can point the build agent at it.
const planPathMarker = "Plan written to "

var planSlugUnsafe = regexp.MustCompile(`[^a-z0-9]+`)

// PlanSlug normalizes a caller-supplied slug into [a-z0-9-], bounded in length.
// Empty or fully-stripped input falls back to a timestamp so a plan always
// saves.
func PlanSlug(raw string) string {
	s := planSlugUnsafe.ReplaceAllString(strings.ToLower(strings.TrimSpace(raw)), "-")
	s = strings.Trim(s, "-")
	if len(s) > 64 {
		s = strings.Trim(s[:64], "-")
	}
	if s == "" {
		s = "plan-" + time.Now().Format("20060102-150405")
	}
	return s
}

// PlanPathFromResult extracts the written plan path from a plan_write result
// (empty when the result is not a successful plan_write).
func PlanPathFromResult(result string) string {
	i := strings.Index(result, planPathMarker)
	if i < 0 {
		return ""
	}
	rest := result[i+len(planPathMarker):]
	if j := strings.Index(rest, " ("); j >= 0 {
		rest = rest[:j]
	}
	return strings.TrimSpace(rest)
}

func (t *PlanWriteTool) Execute(_ context.Context, params map[string]any) (string, error) {
	content, _ := params["content"].(string)
	if strings.TrimSpace(content) == "" {
		return "", fmt.Errorf("content is required")
	}
	title, _ := params["title"].(string)
	slug, _ := params["slug"].(string)

	root := t.Workspace
	if root == "" {
		root = t.SharedWorkspace
	}
	if root == "" {
		return "", fmt.Errorf("no workspace configured")
	}
	dir := filepath.Join(root, "plans")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create plans dir: %w", err)
	}

	path := filepath.Join(dir, PlanSlug(slug)+".md")
	// Defence in depth: PlanSlug already restricts the name to [a-z0-9-], but
	// never write outside the plans directory even if that changes.
	if cleanDir, clean := filepath.Clean(dir), filepath.Clean(path); !strings.HasPrefix(clean, cleanDir+string(os.PathSeparator)) {
		return "", fmt.Errorf("invalid plan path %q", clean)
	}

	var b strings.Builder
	if heading := strings.TrimSpace(title); heading != "" {
		b.WriteString("# " + heading + "\n\n")
	}
	b.WriteString(content)
	if !strings.HasSuffix(content, "\n") {
		b.WriteString("\n")
	}

	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", fmt.Errorf("write plan: %w", err)
	}
	return fmt.Sprintf("%s%s (%d bytes). Read it back with the read tool if you need to revise it.", planPathMarker, path, b.Len()), nil
}
