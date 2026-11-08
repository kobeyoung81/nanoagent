package main

import (
	"testing"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanosec/internal/config"
)

// TestThinkingOffIsSentAsAnExplicitNone pins the platform's "off" on the wire:
// a reasoning model keeps thinking when reasoning_effort is missing, so turning
// thinking off has to send none rather than nothing.
func TestThinkingOffIsSentAsAnExplicitNone(t *testing.T) {
	// The agent-level selection (the composer's per-mode override, resolved by
	// the bus and handed to applyModelCatalog).
	merged := &config.AgentConfig{}
	applyModelCatalog(nil, 0, "", "off", "", merged)
	if merged.Agent.ThinkingEffort != effortNone {
		t.Fatalf("applyModelCatalog(off) = %q, want %q", merged.Agent.ThinkingEffort, effortNone)
	}

	// The model catalog's own switch.
	var core config.AgentCoreConfig
	applyModelParams(&core, &nanoprotocol.ModelDef{ThinkingEnabled: false})
	if core.ThinkingEffort != effortNone {
		t.Fatalf("thinking disabled = %q, want %q", core.ThinkingEffort, effortNone)
	}
	applyModelParams(&core, &nanoprotocol.ModelDef{ThinkingEnabled: true})
	if core.ThinkingEffort != "medium" {
		t.Fatalf("thinking enabled without a level = %q, want medium", core.ThinkingEffort)
	}
	applyModelParams(&core, &nanoprotocol.ModelDef{ThinkingEnabled: true, ThinkingEffort: "high"})
	if core.ThinkingEffort != "high" {
		t.Fatalf("thinking enabled = %q, want high", core.ThinkingEffort)
	}
}

// TestPerModeOverrideOffBeatsTheModelDefault: the override is what the composer
// writes, and "off" must reach the provider as an explicit none.
func TestPerModeOverrideOffBeatsTheModelDefault(t *testing.T) {
	snap := &nanoprotocol.AgentSnapshotResponse{
		Agent: nanoprotocol.AgentInfo{DefaultAgent: "build"},
		Capabilities: nanoprotocol.CapabilitiesResponse{
			Models: []nanoprotocol.ModelDef{{ID: 1, Model: "basic", Name: "Basic", ThinkingEnabled: true, ThinkingEffort: "medium"}},
		},
		Agents: []nanoprotocol.AgentTypeDef{{
			ID:       "build",
			Models:   []nanoprotocol.TypeModelBinding{{ModelID: 1, IsDefault: true}},
			Override: &nanoprotocol.AgentTypeOverride{Thinking: "off"},
		}},
	}
	reg, _, _, _ := resolveAgentRegistry(snap, nil, "")
	spec, ok := reg["build"]
	if !ok {
		t.Fatal("the build mode must resolve")
	}
	if spec.ReasoningEffort != effortNone {
		t.Fatalf("ReasoningEffort = %q, want %q", spec.ReasoningEffort, effortNone)
	}
}

// TestPlatformThinkingDefaultApplies: agent_types.settings["thinking"] is the
// platform default. With no explicit choice from the user it must be what the
// provider is asked for, not the model entry's own level.
func TestPlatformThinkingDefaultApplies(t *testing.T) {
	snap := &nanoprotocol.AgentSnapshotResponse{
		Agent: nanoprotocol.AgentInfo{DefaultAgent: "build"},
		Capabilities: nanoprotocol.CapabilitiesResponse{
			Models: []nanoprotocol.ModelDef{{ID: 1, Model: "basic", Name: "Basic", ThinkingEnabled: true, ThinkingEffort: "medium"}},
		},
		Agents: []nanoprotocol.AgentTypeDef{{
			ID:       "build",
			Models:   []nanoprotocol.TypeModelBinding{{ModelID: 1, IsDefault: true}},
			Settings: map[string]string{thinkingSettingKey: "high"},
		}},
	}
	reg, _, _, _ := resolveAgentRegistry(snap, nil, "")
	if got := reg["build"].ReasoningEffort; got != "high" {
		t.Fatalf("ReasoningEffort = %q, want high (the platform default)", got)
	}
}

// TestModelCapabilityBeatsThePlatformDefault: a model whose catalog entry has
// thinking switched off cannot think, so a platform default must not turn it
// back on (only the user's explicit choice below can).
func TestModelCapabilityBeatsThePlatformDefault(t *testing.T) {
	snap := &nanoprotocol.AgentSnapshotResponse{
		Agent: nanoprotocol.AgentInfo{DefaultAgent: "build"},
		Capabilities: nanoprotocol.CapabilitiesResponse{
			Models: []nanoprotocol.ModelDef{{ID: 1, Model: "plain", Name: "Plain", ThinkingEnabled: false}},
		},
		Agents: []nanoprotocol.AgentTypeDef{{
			ID:       "build",
			Models:   []nanoprotocol.TypeModelBinding{{ModelID: 1, IsDefault: true}},
			Settings: map[string]string{thinkingSettingKey: "high"},
		}},
	}
	reg, _, _, _ := resolveAgentRegistry(snap, nil, "")
	if got := reg["build"].ReasoningEffort; got != effortNone {
		t.Fatalf("ReasoningEffort = %q, want %q (the model cannot think)", got, effortNone)
	}
}

// TestUserChoiceBeatsTheModelCapability: an explicit per-(agent, mode) choice is
// the user's decision and still wins, exactly as it did before the platform
// default existed.
func TestUserChoiceBeatsTheModelCapability(t *testing.T) {
	snap := &nanoprotocol.AgentSnapshotResponse{
		Agent: nanoprotocol.AgentInfo{DefaultAgent: "build"},
		Capabilities: nanoprotocol.CapabilitiesResponse{
			Models: []nanoprotocol.ModelDef{{ID: 1, Model: "plain", Name: "Plain", ThinkingEnabled: false}},
		},
		Agents: []nanoprotocol.AgentTypeDef{{
			ID:       "build",
			Models:   []nanoprotocol.TypeModelBinding{{ModelID: 1, IsDefault: true}},
			Settings: map[string]string{thinkingSettingKey: "low"},
			Override: &nanoprotocol.AgentTypeOverride{Thinking: "high"},
		}},
	}
	reg, _, _, _ := resolveAgentRegistry(snap, nil, "")
	if got := reg["build"].ReasoningEffort; got != "high" {
		t.Fatalf("ReasoningEffort = %q, want high (the user's choice)", got)
	}
}

// TestUnknownThinkingLevelIsIgnored: a value neither layer recognizes must fall
// back to the next layer instead of being forwarded to the provider as a stray
// reasoning_effort.
func TestUnknownThinkingLevelIsIgnored(t *testing.T) {
	snap := &nanoprotocol.AgentSnapshotResponse{
		Agent: nanoprotocol.AgentInfo{DefaultAgent: "build"},
		Capabilities: nanoprotocol.CapabilitiesResponse{
			Models: []nanoprotocol.ModelDef{{ID: 1, Model: "basic", Name: "Basic", ThinkingEnabled: true, ThinkingEffort: "medium"}},
		},
		Agents: []nanoprotocol.AgentTypeDef{{
			ID:       "build",
			Models:   []nanoprotocol.TypeModelBinding{{ModelID: 1, IsDefault: true}},
			Settings: map[string]string{thinkingSettingKey: "low"},
			Override: &nanoprotocol.AgentTypeOverride{Thinking: "banana"},
		}},
	}
	reg, _, _, _ := resolveAgentRegistry(snap, nil, "")
	if got := reg["build"].ReasoningEffort; got != "low" {
		t.Fatalf("ReasoningEffort = %q, want low (the platform default after the typo was dropped)", got)
	}
}

// TestEmptyThinkingLayersFallBackToTheModel: with no platform default and no
// user choice, the model entry's own level is used — the behaviour from before
// the platform layer existed must not drift.
func TestEmptyThinkingLayersFallBackToTheModel(t *testing.T) {
	if got := resolveThinkingEffort(&nanoprotocol.ModelDef{ThinkingEnabled: true}, "", "", "build"); got != "medium" {
		t.Fatalf("enabled model without a level = %q, want medium", got)
	}
	if got := resolveThinkingEffort(&nanoprotocol.ModelDef{ThinkingEnabled: true, ThinkingEffort: "high"}, "", "", "build"); got != "high" {
		t.Fatalf("enabled model with a level = %q, want high", got)
	}
	// A model the visible catalog does not know leaves only the two stored
	// layers, and an empty result means "use the agent-wide fallback".
	if got := resolveThinkingEffort(nil, "", "", "build"); got != "" {
		t.Fatalf("unknown model with no layers = %q, want empty", got)
	}
	if got := resolveThinkingEffort(nil, "low", "", "build"); got != "low" {
		t.Fatalf("unknown model with a platform default = %q, want low", got)
	}
}

// TestPlatformDefaultReachesTheAgentWideEffort drives the second call site: the
// agent-wide fallback seeded from the default mode.
func TestPlatformDefaultReachesTheAgentWideEffort(t *testing.T) {
	snap := &nanoprotocol.AgentSnapshotResponse{
		Agent: nanoprotocol.AgentInfo{DefaultAgent: "build"},
		Capabilities: nanoprotocol.CapabilitiesResponse{
			Models: []nanoprotocol.ModelDef{{ID: 1, Model: "basic", Name: "Basic", ThinkingEnabled: true, ThinkingEffort: "medium"}},
		},
		Agents: []nanoprotocol.AgentTypeDef{{
			ID:       "build",
			Models:   []nanoprotocol.TypeModelBinding{{ModelID: 1, IsDefault: true}},
			Settings: map[string]string{thinkingSettingKey: "high"},
		}},
	}
	modelID, thinking := defaultModeModel(snap)
	if thinking != "high" {
		t.Fatalf("defaultModeModel thinking = %q, want high", thinking)
	}
	merged := &config.AgentConfig{}
	applyModelCatalog(&snap.Capabilities, modelID, "", thinking, "basic", merged)
	if merged.Agent.ThinkingEffort != "high" {
		t.Fatalf("agent-wide ThinkingEffort = %q, want high", merged.Agent.ThinkingEffort)
	}
}

// TestDefPermissionsOverrideWinsLast pins the runtime order for a definition
// whose override the bus has already folded into Permissions: the override must
// be re-applied after the per-capability rules so a capability 'ask' cannot
// defeat a persisted 'always allow'.
func TestDefPermissionsOverrideWinsLast(t *testing.T) {
	def := nanoprotocol.AgentTypeDef{
		Permissions: []nanoprotocol.PermissionRule{
			{Pattern: "*", Action: nanoprotocol.PermissionAllow},
			{Pattern: "bash", Action: nanoprotocol.PermissionAsk},
			// Folded in by the bus's mergeOverride.
			{Pattern: "bash", Action: nanoprotocol.PermissionAllow},
		},
		Capabilities: []nanoprotocol.TypeCapabilityBinding{
			{Permissions: []nanoprotocol.PermissionRule{{Pattern: "bash", Action: nanoprotocol.PermissionAsk}}},
		},
		Override: &nanoprotocol.AgentTypeOverride{
			Permissions: []nanoprotocol.PermissionRule{{Pattern: "bash", Action: nanoprotocol.PermissionAllow}},
		},
	}
	if got := nanoprotocol.EffectivePermission("bash", defPermissions(def)); got != nanoprotocol.PermissionAllow {
		t.Fatalf("override should win: got %q", got)
	}

	// Without an override the capability ask stands.
	def.Override = nil
	def.Permissions = def.Permissions[:2]
	if got := nanoprotocol.EffectivePermission("bash", defPermissions(def)); got != nanoprotocol.PermissionAsk {
		t.Fatalf("capability ask expected without override: got %q", got)
	}
}

// subagentSnap is a snapshot with one session mode and one subagent type, which
// is what makes "the subagent is resolved differently" observable.
func subagentSnap(sessionSettings, subagentSettings map[string]string, override *nanoprotocol.AgentTypeOverride) *nanoprotocol.AgentSnapshotResponse {
	return &nanoprotocol.AgentSnapshotResponse{
		Agent: nanoprotocol.AgentInfo{DefaultAgent: "build"},
		Capabilities: nanoprotocol.CapabilitiesResponse{
			Models: []nanoprotocol.ModelDef{{ID: 1, Model: "basic", Name: "Basic", ThinkingEnabled: true, ThinkingEffort: "medium"}},
		},
		Agents: []nanoprotocol.AgentTypeDef{
			{
				ID:       "build",
				Models:   []nanoprotocol.TypeModelBinding{{ModelID: 1, IsDefault: true}},
				Settings: sessionSettings,
			},
			{
				ID:       "role_explore",
				Role:     "explore",
				Output:   nanoprotocol.OutputCaller,
				Models:   []nanoprotocol.TypeModelBinding{{ModelID: 1, IsDefault: true}},
				Settings: subagentSettings,
				Override: override,
			},
		},
	}
}

// TestSubagentThinkingUsesTheTypeDefault: a subagent type's platform default is
// what its calls run at — it outranks the platform-wide default, and the model
// entry's own level is not used at all (the subagent layer has no "follow the
// model" step).
func TestSubagentThinkingUsesTheTypeDefault(t *testing.T) {
	// Both layers set, with different values: only the order tells them apart.
	snap := subagentSnap(nil, map[string]string{thinkingSettingKey: "low"}, nil)
	reg, _, _, _ := resolveAgentRegistry(snap, nil, "high")
	if got := reg["role_explore"].ReasoningEffort; got != "low" {
		t.Fatalf("subagent effort = %q, want low (the type default outranks the platform default)", got)
	}
}

// TestSubagentThinkingFallsBackToThePlatformDefault: with no type default, the
// platform-wide subagent default applies.
func TestSubagentThinkingFallsBackToThePlatformDefault(t *testing.T) {
	snap := subagentSnap(nil, nil, nil)
	reg, _, _, _ := resolveAgentRegistry(snap, nil, "none")
	if got := reg["role_explore"].ReasoningEffort; got != effortNone {
		t.Fatalf("subagent effort = %q, want %q (the platform default)", got, effortNone)
	}
}

// TestSubagentThinkingSendsNothingWhenUnset: neither layer set means the request
// carries no reasoning_effort at all — the behaviour before either layer
// existed, and the reason no cost baseline moves.
func TestSubagentThinkingSendsNothingWhenUnset(t *testing.T) {
	snap := subagentSnap(nil, nil, nil)
	reg, _, _, _ := resolveAgentRegistry(snap, nil, "")
	if got := reg["role_explore"].ReasoningEffort; got != "" {
		t.Fatalf("subagent effort = %q, want empty (send nothing)", got)
	}
	// The session mode is unaffected: it still follows the model entry.
	if got := reg["build"].ReasoningEffort; got != "medium" {
		t.Fatalf("session effort = %q, want medium", got)
	}
}

// TestSubagentThinkingIgnoresTheUserOverride: users configure the agent and its
// modes, not its helpers, so a stored (agent, subagent type) level must not
// reach the subagent call.
func TestSubagentThinkingIgnoresTheUserOverride(t *testing.T) {
	snap := subagentSnap(nil, nil, &nanoprotocol.AgentTypeOverride{Thinking: "high"})
	reg, _, _, _ := resolveAgentRegistry(snap, nil, "")
	if got := reg["role_explore"].ReasoningEffort; got != "" {
		t.Fatalf("subagent effort = %q, want empty (user overrides do not apply to subagents)", got)
	}
}

// TestSubagentThinkingSkipsAThinkingDisabledModel: neither default turns thinking
// back on for a model the catalog switched off.
func TestSubagentThinkingSkipsAThinkingDisabledModel(t *testing.T) {
	snap := &nanoprotocol.AgentSnapshotResponse{
		Agent: nanoprotocol.AgentInfo{DefaultAgent: "build"},
		Capabilities: nanoprotocol.CapabilitiesResponse{
			Models: []nanoprotocol.ModelDef{{ID: 1, Model: "plain", Name: "Plain", ThinkingEnabled: false}},
		},
		Agents: []nanoprotocol.AgentTypeDef{{
			ID:       "role_explore",
			Role:     "explore",
			Output:   nanoprotocol.OutputCaller,
			Models:   []nanoprotocol.TypeModelBinding{{ModelID: 1, IsDefault: true}},
			Settings: map[string]string{thinkingSettingKey: "high"},
		}},
	}
	reg, _, _, _ := resolveAgentRegistry(snap, nil, "low")
	if got := reg["role_explore"].ReasoningEffort; got != "" {
		t.Fatalf("subagent effort = %q, want empty (the model cannot think)", got)
	}
}

// TestSubagentThinkingDropsAnUnknownValue: a typo in either platform layer is
// reported and skipped, never forwarded upstream.
func TestSubagentThinkingDropsAnUnknownValue(t *testing.T) {
	snap := subagentSnap(nil, map[string]string{thinkingSettingKey: "banana"}, nil)
	reg, _, _, _ := resolveAgentRegistry(snap, nil, "banana")
	if got := reg["role_explore"].ReasoningEffort; got != "" {
		t.Fatalf("subagent effort = %q, want empty after two typos", got)
	}
}
