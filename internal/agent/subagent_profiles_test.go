package agent

import (
	"testing"

	"gitee.com/kobeyoung81/nanoprotocol"
)

// TestSubagentProfiles_DropDelegationTools pins the depth-1 rule at profile
// level: a subagent's allowlist must not mention the delegation tools, and the
// resolved registry's map must not be mutated by the copy.
func TestSubagentProfiles_DropDelegationTools(t *testing.T) {
	source := map[string]bool{"read": true, "grep": true, "task": true, "readonly_task": true}
	profiles := Config{Agents: map[string]ResolvedAgent{
		"role_explore": {
			ID:           "role_explore",
			Role:         "explore",
			Output:       nanoprotocol.OutputCaller,
			EnabledTools: source,
		},
	}}.SubagentProfiles()

	profile, ok := profiles["explore"]
	if !ok {
		t.Fatalf("explore profile missing: %v", profiles)
	}
	for _, name := range DelegationToolNames {
		if profile.Tools[name] {
			t.Errorf("subagent profile must not offer %q", name)
		}
	}
	if !profile.Tools["read"] || !profile.Tools["grep"] {
		t.Errorf("other tools must survive: %v", profile.Tools)
	}
	if !source["task"] || !source["readonly_task"] {
		t.Errorf("the resolved registry's map must not be mutated: %v", source)
	}
}

// TestSubagentProfiles_KeepsNilAllowlist pins the documented limit: a profile
// without an allowlist means "everything registered", which a copy cannot
// narrow — the registry is what keeps the delegation tools away there.
func TestSubagentProfiles_KeepsNilAllowlist(t *testing.T) {
	profiles := Config{Agents: map[string]ResolvedAgent{
		"role_general": {ID: "role_general", Role: "general", Output: nanoprotocol.OutputCaller},
	}}.SubagentProfiles()

	if profiles["general"].Tools != nil {
		t.Errorf("a nil allowlist must stay nil, got %v", profiles["general"].Tools)
	}
}

// TestSubagentProfiles_CarryTheResolvedThinkingLevel: the profile is the only
// place the runner reads a subagent's level from, so the registry's resolved
// value has to reach it.
func TestSubagentProfiles_CarryTheResolvedThinkingLevel(t *testing.T) {
	profiles := Config{Agents: map[string]ResolvedAgent{
		"role_explore": {
			ID:              "role_explore",
			Role:            "explore",
			Output:          nanoprotocol.OutputCaller,
			ReasoningEffort: "low",
		},
	}}.SubagentProfiles()

	if got := profiles["explore"].ReasoningEffort; got != "low" {
		t.Fatalf("profile effort = %q, want low", got)
	}
}

// TestSubagentEffortPrefersTheProfile: the profile's level wins; the manager's
// platform default is only the fallback, and an empty result means "send no
// reasoning_effort at all".
func TestSubagentEffortPrefersTheProfile(t *testing.T) {
	cfg := SubagentConfig{ReasoningEffort: "none"}
	if got := subagentEffort(SubagentProfile{ReasoningEffort: "high"}, cfg); got != "high" {
		t.Fatalf("profile level must win: got %q", got)
	}
	if got := subagentEffort(SubagentProfile{}, cfg); got != "none" {
		t.Fatalf("manager default expected: got %q", got)
	}
	if got := subagentEffort(SubagentProfile{}, SubagentConfig{}); got != "" {
		t.Fatalf("nothing configured must stay empty: got %q", got)
	}
}
