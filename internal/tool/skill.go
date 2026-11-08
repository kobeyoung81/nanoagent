package tool

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"gitee.com/kobeyoung81/nanosec/internal/provider"
	"gitee.com/kobeyoung81/nanosec/internal/skill"
)

// SkillProvider is the subset of skill.Registry the SkillTool needs.
type SkillProvider interface {
	List() []skill.Skill
	BuildSummary() string
	LoadDetail(name string) (skill.Detail, bool)
}

// SkillTool loads a skill's SKILL.md into the conversation on demand, part of
// the opencode-style progressive skill disclosure. Only the summary is present
// in the tool description; the full body is returned when the model calls it.
type SkillTool struct {
	registry SkillProvider
}

// NewSkillTool creates a SkillTool backed by the given registry.
func NewSkillTool(registry SkillProvider) *SkillTool {
	return &SkillTool{registry: registry}
}

func (t *SkillTool) Name() string { return "load_skill" }

func (t *SkillTool) Description() string {
	base := "Load a specialized skill when the task at hand matches one of the skills listed below. " +
		"Use this tool to inject the skill's instructions and resources into the current conversation. " +
		"The output may contain detailed workflow guidance as well as references to scripts, files, etc. " +
		"in the same directory as the skill."
	summary := t.registry.BuildSummary()
	if summary == "" {
		return base
	}
	return base + "\n\n" + summary
}

func (t *SkillTool) Parameters() provider.ParameterSchema {
	return provider.ParameterSchema{
		Type: "object",
		Properties: map[string]provider.PropertySchema{
			"name": {Type: "string", Description: "The name of the skill to load"},
		},
		Required: []string{"name"},
	}
}

func (t *SkillTool) Execute(_ context.Context, params map[string]any) (string, error) {
	name, _ := params["name"].(string)
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("missing name")
	}

	detail, ok := t.registry.LoadDetail(name)
	if !ok {
		return "", fmt.Errorf("skill %q not found. Available skills: %s", name, t.availableNames())
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("<skill_content name=%q>\n", detail.Name))
	sb.WriteString("# Skill: " + detail.Name + "\n\n")
	sb.WriteString(strings.TrimSpace(detail.Body))
	sb.WriteString("\n\nBase directory for this skill: " + detail.BaseDir + "\n")
	sb.WriteString("Relative paths in this skill (e.g., scripts/, reference/) are relative to this base directory.\n")
	if len(detail.Files) > 0 {
		sb.WriteString("Note: file list is sampled.\n\n")
		sb.WriteString("<skill_files>\n")
		for _, f := range detail.Files {
			sb.WriteString("<file>" + f + "</file>\n")
		}
		sb.WriteString("</skill_files>\n")
	}
	sb.WriteString("</skill_content>")
	return sb.String(), nil
}

func (t *SkillTool) availableNames() string {
	skills := t.registry.List()
	names := make([]string, 0, len(skills))
	for _, s := range skills {
		names = append(names, s.Name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}
