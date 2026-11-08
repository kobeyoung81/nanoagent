package config

import (
	"testing"

	"gitee.com/kobeyoung81/nanoprotocol"
)

func TestResolveAgentSettings_Layering(t *testing.T) {
	items := []nanoprotocol.ConfigItem{
		{Key: "agent.default.maxIterations", Value: "10"},
		{Key: "agent.default.contextWindow", Value: "8000"},
		{Key: "agent.assistant.maxIterations", Value: "20"},
		{Key: "agent.devops.model", Value: "gpt-4o"},
		{Key: "agent.legacy", Value: "direct"},
	}

	merged := ResolveAgentSettings(items, "assistant")

	if got := merged["agent.maxIterations"]; got != "20" {
		t.Errorf("assistant.maxIterations = %q, want 20 (per-agent overrides default)", got)
	}
	if got := merged["agent.contextWindow"]; got != "8000" {
		t.Errorf("contextWindow = %q, want 8000 (inherited from default)", got)
	}
	if got := merged["agent.devops.model"]; got != "gpt-4o" {
		t.Errorf("unrelated agent key leaked: %q", got)
	}
	if got := merged["agent.legacy"]; got != "direct" {
		t.Errorf("direct key = %q, want direct", got)
	}
}

func TestResolveAgentSettings_DefaultDoesNotOverridePerAgent(t *testing.T) {
	items := []nanoprotocol.ConfigItem{
		{Key: "agent.default.maxIterations", Value: "10"},
		{Key: "agent.other.maxIterations", Value: "50"},
	}

	merged := ResolveAgentSettings(items, "assistant")
	if got := merged["agent.maxIterations"]; got != "10" {
		t.Errorf("assistant.maxIterations = %q, want 10 (only default applies)", got)
	}

	mergedOther := ResolveAgentSettings(items, "other")
	if got := mergedOther["agent.maxIterations"]; got != "50" {
		t.Errorf("other.maxIterations = %q, want 50", got)
	}
}

func TestBuildAgentConfigFor_AppliesLayering(t *testing.T) {
	base := DefaultAgentConfig()
	items := []nanoprotocol.ConfigItem{
		{Key: "agent.default.maxIterations", Value: "12"},
		{Key: "agent.assistant.maxIterations", Value: "25"},
	}

	cfg := BuildAgentConfigFor(base, items, "assistant")
	if cfg.Agent.MaxIterations != 25 {
		t.Errorf("MaxIterations = %d, want 25", cfg.Agent.MaxIterations)
	}
}

// The platform default for a scheduled task's mode comes from the config plane,
// so it follows the same layering as every other agent setting: a per-agent key
// beats the agent.default. one, which beats nothing (the code default build).
func TestBuildAgentConfigFor_ScheduledTaskMode(t *testing.T) {
	base := DefaultAgentConfig()
	if base.Agent.ScheduledTaskMode != "build" {
		t.Fatalf("default ScheduledTaskMode = %q, want build", base.Agent.ScheduledTaskMode)
	}

	cfg := BuildAgentConfigFor(base, []nanoprotocol.ConfigItem{
		{Key: "agent.scheduledTaskMode", Value: "plan"},
	}, "assistant")
	if cfg.Agent.ScheduledTaskMode != "plan" {
		t.Errorf("ScheduledTaskMode = %q, want plan", cfg.Agent.ScheduledTaskMode)
	}

	cfg = BuildAgentConfigFor(base, []nanoprotocol.ConfigItem{
		{Key: "agent.scheduledTaskMode", Value: "plan"},
		{Key: "agent.default.scheduledTaskMode", Value: "build"},
		{Key: "agent.assistant.scheduledTaskMode", Value: "plan"},
	}, "assistant")
	if cfg.Agent.ScheduledTaskMode != "plan" {
		t.Errorf("per-agent key must win: got %q, want plan", cfg.Agent.ScheduledTaskMode)
	}

	cfg = BuildAgentConfigFor(base, []nanoprotocol.ConfigItem{
		{Key: "agent.other.scheduledTaskMode", Value: "plan"},
	}, "assistant")
	if cfg.Agent.ScheduledTaskMode != "build" {
		t.Errorf("another agent's key leaked: %q", cfg.Agent.ScheduledTaskMode)
	}
}
