package agent

import (
	"testing"
	"time"

	"gitee.com/kobeyoung81/nanoprotocol"
)

func TestSessionAgentsHonorsExplicitList(t *testing.T) {
	c := Config{
		Agents: map[string]ResolvedAgent{
			"build": {
				ID:             "build",
				Output:         nanoprotocol.OutputStream,
				SessionBinding: nanoprotocol.SessionBindingShared,
			},
			"plan": {
				ID:             "plan",
				Output:         nanoprotocol.OutputStream,
				SessionBinding: nanoprotocol.SessionBindingShared,
			},
			"role_general": {
				ID:             "role_general",
				Role:           "general",
				Output:         nanoprotocol.OutputCaller,
				SessionBinding: nanoprotocol.SessionBindingIsolated,
			},
		},
		SessionAgentIDs: []string{"plan", "build"},
	}
	got := c.SessionAgents()
	if len(got) != 2 || got[0].ID != "plan" || got[1].ID != "build" {
		t.Fatalf("SessionAgents = %+v", got)
	}
	if !c.isSessionAgent("plan") {
		t.Fatalf("plan should be a session agent")
	}
	if c.isSessionAgent("role_general") || c.isSessionAgent("") {
		t.Fatalf("unexpected session agent membership")
	}
}

func TestSessionAgentsDerivedWhenNoList(t *testing.T) {
	c := Config{Agents: map[string]ResolvedAgent{
		"build": {
			ID:             "build",
			Output:         nanoprotocol.OutputStream,
			SessionBinding: nanoprotocol.SessionBindingShared,
		},
		"role_general": {
			ID:             "role_general",
			Output:         nanoprotocol.OutputCaller,
			SessionBinding: nanoprotocol.SessionBindingIsolated,
		},
	}}
	got := c.SessionAgents()
	if len(got) != 1 || got[0].ID != "build" {
		t.Fatalf("derived SessionAgents = %+v", got)
	}
}

func TestSubagentProfilesSelectsCallerOutput(t *testing.T) {
	c := Config{Agents: map[string]ResolvedAgent{
		"role_general": {ID: "role_general", Role: "general", Output: nanoprotocol.OutputCaller,
			SessionBinding: nanoprotocol.SessionBindingIsolated, MaxRounds: 15, Prompt: "g"},
		"role_explore": {ID: "role_explore", Role: "explore", Output: nanoprotocol.OutputCaller,
			SessionBinding: nanoprotocol.SessionBindingIsolated, MaxRounds: 12},
		"build":      {ID: "build", Output: nanoprotocol.OutputStream},
		"role_title": {ID: "role_title", Role: "title", Strategy: nanoprotocol.StrategyOneshot, Output: nanoprotocol.OutputSessionMeta},
	}}
	p := c.SubagentProfiles()
	if len(p) != 2 {
		t.Fatalf("profiles = %+v", p)
	}
	if g, ok := p["general"]; !ok || g.MaxIterations != 15 || g.Prompt != "g" {
		t.Fatalf("general profile = %+v (ok=%v)", g, ok)
	}
	if _, ok := p["explore"]; !ok {
		t.Fatalf("explore profile missing")
	}
}

// TestSubagentProfilesMapsSettings verifies per-type settings flow into the
// subagent profile.
func TestSubagentProfilesMapsSettings(t *testing.T) {
	c := Config{Agents: map[string]ResolvedAgent{
		"role_scout": {
			ID: "role_scout", Role: "scout", Output: nanoprotocol.OutputCaller,
			SessionBinding: nanoprotocol.SessionBindingIsolated, MaxRounds: 12,
			Settings: map[string]string{
				"max_tool_result_chars": "8000",
				"shell_timeout":         "45s",
				"restrict_to_workspace": "false",
			},
		},
		"role_general": {
			ID: "role_general", Role: "general", Output: nanoprotocol.OutputCaller,
			SessionBinding: nanoprotocol.SessionBindingIsolated, MaxRounds: 15,
		},
	}}
	p := c.SubagentProfiles()
	scout := p["scout"]
	if scout.MaxToolResultChars != 8000 {
		t.Errorf("MaxToolResultChars = %d, want 8000", scout.MaxToolResultChars)
	}
	if scout.ShellTimeout != 45*time.Second {
		t.Errorf("ShellTimeout = %v, want 45s", scout.ShellTimeout)
	}
	if scout.RestrictToWorkspace == nil || *scout.RestrictToWorkspace {
		t.Errorf("RestrictToWorkspace = %v, want explicit false", scout.RestrictToWorkspace)
	}
	if g := p["general"]; g.RestrictToWorkspace != nil {
		t.Errorf("general RestrictToWorkspace = %v, want nil (inherit)", g.RestrictToWorkspace)
	}
}
