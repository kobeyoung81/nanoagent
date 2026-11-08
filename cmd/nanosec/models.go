package main

import (
	"encoding/json"
	"log/slog"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanosec/internal/agent"
	"gitee.com/kobeyoung81/nanosec/internal/capability"
	"gitee.com/kobeyoung81/nanosec/internal/config"
	"gitee.com/kobeyoung81/nanosec/internal/provider"
)

// effortNone is the platform's explicit "do not reason". A reasoning model
// keeps thinking when reasoning_effort is left out (and when it carries a low
// level), so "off" has to be sent as this value to take effect.
const effortNone = "none"

// thinkingSettingKey is the AgentType setting holding the platform default
// reasoning level for a type (and, merged, for one mode). Empty means "follow
// the model catalog entry".
const thinkingSettingKey = "thinking"

// normalizeThinkingLevel maps a stored level onto what the provider layer
// understands: off and none both mean "do not reason" and become none;
// low/medium/high pass through. Anything else — the empty string included — is
// "no level", so the caller falls back to the next layer instead of forwarding
// a typo upstream.
func normalizeThinkingLevel(v string) string {
	switch v {
	case "off", effortNone:
		return effortNone
	case "low", "medium", "high":
		return v
	}
	return ""
}

// modelDefaultEffort is what a model's own catalog entry asks for. A model the
// catalog has switched off cannot think, so its entry is an explicit none
// rather than an omitted parameter. md must not be nil.
func modelDefaultEffort(md *nanoprotocol.ModelDef) string {
	if !md.ThinkingEnabled {
		return effortNone
	}
	if md.ThinkingEffort == "" {
		return "medium"
	}
	return md.ThinkingEffort
}

// overrideThinkingLevel is the user's explicit per-(agent, mode) choice, if any.
func overrideThinkingLevel(def nanoprotocol.AgentTypeDef) string {
	if def.Override != nil {
		return def.Override.Thinking
	}
	return ""
}

// resolveThinkingEffort picks the reasoning level for one definition, most
// specific first:
//
//  1. the user's explicit choice (agent_type_overrides.thinking);
//  2. the model's capability — a model the catalog has switched off cannot
//     think, and a platform default does not turn it back on;
//  3. the platform default (agent_types.settings["thinking"]);
//  4. the model entry's own effort.
//
// A value neither layer recognizes is reported and ignored, so it can never
// reach the provider as a stray reasoning_effort. A nil model (the agent's
// model is not in the visible catalog) leaves nothing but layers 1 and 3.
func resolveThinkingEffort(md *nanoprotocol.ModelDef, platformDefault, userChoice, definitionID string) string {
	if userChoice != "" {
		if v := normalizeThinkingLevel(userChoice); v != "" {
			return v
		}
		slog.Warn("ignoring unknown thinking level", "definition", definitionID, "value", userChoice)
	}
	if md != nil && !md.ThinkingEnabled {
		return effortNone
	}
	if platformDefault != "" {
		if v := normalizeThinkingLevel(platformDefault); v != "" {
			return v
		}
		slog.Warn("ignoring unknown platform thinking default", "definition", definitionID, "value", platformDefault)
	}
	if md == nil {
		return ""
	}
	return modelDefaultEffort(md)
}

// resolveSubagentEffort picks the reasoning level for a subagent definition.
// Subagents have no user layer — the user config surface covers the agent and
// its modes, not its helpers — so only the platform decides:
//
//  1. the subagent type's default (agent_types.settings["thinking"]);
//  2. the platform-wide subagent default (agent.llmSubagentReasoningEffort);
//  3. nothing: the request carries no reasoning_effort at all, which is the
//     behaviour every deployment had before either layer existed.
//
// A model the catalog has switched off cannot think, so neither default applies
// to it. An unrecognized value is reported and skipped, never forwarded.
func resolveSubagentEffort(md *nanoprotocol.ModelDef, typeDefault, platformDefault, definitionID string) string {
	if md != nil && !md.ThinkingEnabled {
		return ""
	}
	if typeDefault != "" {
		if v := normalizeThinkingLevel(typeDefault); v != "" {
			return v
		}
		slog.Warn("ignoring unknown subagent thinking default", "definition", definitionID, "value", typeDefault)
	}
	if platformDefault != "" {
		if v := normalizeThinkingLevel(platformDefault); v != "" {
			return v
		}
		slog.Warn("ignoring unknown platform subagent thinking default", "definition", definitionID, "value", platformDefault)
	}
	return ""
}

// applyModelCatalog resolves the agent's catalog model from a capability
// snapshot and folds its provider connection, context/output limits, thinking
// setting and sampling params into the effective config. modelID/modelOverride
// describe the per-agent selection; thinking is the already-resolved level for
// the default mode (see resolveThinkingEffort) and overrides the model's own.
// No-op when caps is nil or the model is not visible.
func applyModelCatalog(caps *nanoprotocol.CapabilitiesResponse, modelID int64, modelOverride, thinking, defaultName string, merged *config.AgentConfig) {
	if caps != nil {
		if md := selectCatalogModel(caps, modelID, modelOverride, defaultName); md != nil {
			if md.ProviderBaseURL != "" {
				merged.Provider.BaseURL = md.ProviderBaseURL
			}
			if md.ProviderAPIKey != "" {
				merged.Provider.APIKey = md.ProviderAPIKey
			}
			merged.Provider.DefaultModel = md.Model
			if md.ContextWindow > 0 {
				merged.Agent.ContextWindow = md.ContextWindow
			}
			if md.MaxOutputTokens > 0 {
				merged.Agent.MaxOutputTokens = md.MaxOutputTokens
			}
			applyModelParams(&merged.Agent, md)
		}
	}
	switch thinking {
	case "off", effortNone:
		merged.Agent.ThinkingEffort = effortNone
	case "low", "medium", "high":
		merged.Agent.ThinkingEffort = thinking
	case "":
		// No level applies to this agent: the model entry's own effort stays as
		// applyModelParams left it.
	default:
		slog.Warn("unknown thinking level; keeping the model default", "value", thinking)
	}
}

// selectCatalogModel resolves the model for an agent. When the agent's
// AgentType declares an allowed model set, the type's locked default wins,
// otherwise the agent's own selection if allowed, otherwise the type default.
// Falls back to name/override matching when no type set is available.
func selectCatalogModel(caps *nanoprotocol.CapabilitiesResponse, modelID int64, modelOverride, defaultName string) *nanoprotocol.ModelDef {
	if at := caps.AgentType; at != nil && len(at.Models) > 0 {
		byID := map[int64]*nanoprotocol.ModelDef{}
		for i := range caps.Models {
			byID[caps.Models[i].ID] = &caps.Models[i]
		}
		var defaultID int64
		var defaultLocked bool
		inSet := false
		for _, b := range at.Models {
			if b.ModelID == modelID {
				inSet = true
			}
			if b.IsDefault {
				defaultID = b.ModelID
				defaultLocked = b.Locked
			}
		}
		if defaultLocked && defaultID != 0 {
			if m := byID[defaultID]; m != nil {
				return m
			}
		}
		if modelID != 0 && inSet {
			if m := byID[modelID]; m != nil {
				return m
			}
		}
		if defaultID != 0 {
			if m := byID[defaultID]; m != nil {
				return m
			}
		}
	}
	return resolveSelectedModel(caps.Models, modelID, modelOverride, defaultName)
}

// resolveSelectedModel picks the catalog model for an agent: an explicit
// model_id wins, then a name match (agent override or the resolved default)
// against the catalog's API model id or display name. Returns nil when the
// agent's model is not in the visible catalog (free-text fallback).
func resolveSelectedModel(models []nanoprotocol.ModelDef, modelID int64, modelOverride, defaultName string) *nanoprotocol.ModelDef {
	if modelID != 0 {
		for i := range models {
			if models[i].ID == modelID {
				return &models[i]
			}
		}
	}
	want := modelOverride
	if want == "" {
		want = defaultName
	}
	if want == "" {
		return nil
	}
	for i := range models {
		if models[i].Model == want || models[i].Name == want {
			return &models[i]
		}
	}
	return nil
}

// defDefaultModel returns the definition's default allowed model resolved to
// its catalog entry, or nil.
func defDefaultModel(caps *nanoprotocol.CapabilitiesResponse, def nanoprotocol.AgentTypeDef) *nanoprotocol.ModelDef {
	if caps == nil || len(def.Models) == 0 {
		return nil
	}
	var id int64
	for _, b := range def.Models {
		if b.IsDefault {
			id = b.ModelID
		}
	}
	if id == 0 {
		id = def.Models[0].ModelID
	}
	for i := range caps.Models {
		if caps.Models[i].ID == id {
			return &caps.Models[i]
		}
	}
	return nil
}

// defEffectiveModel resolves a definition's effective catalog model: the user's
// per-(agent, mode) override when it is in the allowed set, unless the type's
// default binding is locked (locked default wins). Falls back to the type
// default.
func defEffectiveModel(caps *nanoprotocol.CapabilitiesResponse, def nanoprotocol.AgentTypeDef) *nanoprotocol.ModelDef {
	if caps == nil || len(def.Models) == 0 {
		return nil
	}
	var defID int64
	var defLocked bool
	inSet := false
	sel := int64(0)
	if def.Override != nil {
		sel = def.Override.ModelID
	}
	for _, b := range def.Models {
		if b.ModelID == sel {
			inSet = true
		}
		if b.IsDefault {
			defID = b.ModelID
			defLocked = b.Locked
		}
	}
	if defID == 0 {
		defID = def.Models[0].ModelID
	}
	pick := defID
	if !defLocked && sel != 0 && inSet {
		pick = sel
	}
	for i := range caps.Models {
		if caps.Models[i].ID == pick {
			return &caps.Models[i]
		}
	}
	return nil
}

// defaultModeModel returns the default mode's catalog model id and its resolved
// thinking level, used to seed the agent-wide provider connection/context
// window.
func defaultModeModel(snap *nanoprotocol.AgentSnapshotResponse) (int64, string) {
	if snap == nil {
		return 0, ""
	}
	defID := snap.Agent.DefaultAgent
	if defID == "" && len(snap.Agent.SessionAgents) > 0 {
		defID = snap.Agent.SessionAgents[0]
	}
	if defID == "" {
		defID = snap.Agent.TypeID
	}
	if defID == "" {
		defID = "build"
	}
	for _, def := range snap.Agents {
		if def.ID != defID {
			continue
		}
		md := defEffectiveModel(&snap.Capabilities, def)
		thinking := resolveThinkingEffort(md, def.Settings[thinkingSettingKey], overrideThinkingLevel(def), def.ID)
		if md != nil {
			return md.ID, thinking
		}
		return 0, thinking
	}
	return 0, ""
}

// defPermissions merges a definition's baseline permissions with its
// per-capability rules and its user override, in last-match-wins order:
// baseline, then capability rules, then the override. The bus folds the
// override into def.Permissions (baseline ++ override); strip it back out so
// the override is applied last and cannot be defeated by a capability rule.
func defPermissions(def nanoprotocol.AgentTypeDef) []nanoprotocol.PermissionRule {
	base := def.Permissions
	var override []nanoprotocol.PermissionRule
	if def.Override != nil {
		override = def.Override.Permissions
	}
	if n := len(override); n > 0 && len(base) >= n {
		base = base[:len(base)-n]
	}
	rules := append([]nanoprotocol.PermissionRule{}, base...)
	for _, c := range def.Capabilities {
		rules = append(rules, c.Permissions...)
	}
	rules = append(rules, override...)
	return rules
}

// resolveAgentRegistry builds the unified resolved definition map from the
// snapshot's Agents list. Each definition (mode) resolves its own effective
// model/thinking/settings/permissions/builtin-tools from the type baseline
// merged with the user's per-(agent, mode) override (carried on def.Override
// and already folded into def by the bus).
func resolveAgentRegistry(snap *nanoprotocol.AgentSnapshotResponse, shared provider.Provider, subagentDefault string) (map[string]agent.ResolvedAgent, string, map[string]string, map[string]string) {
	reg := map[string]agent.ResolvedAgent{}
	rolePrompts := map[string]string{}
	roleModels := map[string]string{}
	if snap == nil {
		return reg, "", rolePrompts, roleModels
	}
	buildType := snap.Agent.TypeID
	if buildType == "" {
		buildType = "build"
	}
	for _, def := range snap.Agents {
		spec := agent.ResolvedAgent{
			ID:               def.ID,
			Role:             def.Role,
			Prompt:           def.Prompt,
			EnabledTools:     capability.EnabledBuiltinTools(&def, snap.Capabilities.BuiltinTools),
			McpServers:       capability.EnabledMcpServers(&def, snap.Capabilities.McpServers),
			ChannelFallbacks: def.ChannelFallbacks,
			Permissions:      defPermissions(def),
			MaxRounds:        def.MaxRounds,
			Strategy:         orDefault(def.Strategy, nanoprotocol.StrategyReact),
			Context:          orDefault(def.Context, nanoprotocol.ContextWindowed),
			Output:           orDefault(def.Output, nanoprotocol.OutputStream),
			SessionBinding:   orDefault(def.SessionBinding, nanoprotocol.SessionBindingShared),
			Settings:         def.Settings,
			Provider:         shared,
		}
		md := defEffectiveModel(&snap.Capabilities, def)
		if md != nil {
			spec.Model = md.Model
			var core config.AgentCoreConfig
			applyModelParams(&core, md)
			spec.Temperature = core.Temperature
			spec.TopP = core.TopP
		}
		if spec.Output == nanoprotocol.OutputCaller {
			// Subagents have no user layer: their level comes from the type's
			// platform default and the platform-wide default, nothing else.
			spec.ReasoningEffort = resolveSubagentEffort(md, def.Settings[thinkingSettingKey], subagentDefault, def.ID)
		} else {
			// Everything else resolves across four layers, most specific first:
			// the user's choice, the model's capability, the platform default,
			// then the model entry itself.
			spec.ReasoningEffort = resolveThinkingEffort(md, def.Settings[thinkingSettingKey], overrideThinkingLevel(def), def.ID)
		}
		reg[def.ID] = spec

		if def.Role != "" {
			if def.Prompt != "" {
				rolePrompts[def.Role] = def.Prompt
			}
			if spec.Model != "" {
				roleModels[def.Role] = spec.Model
			}
		}
	}
	defaultID := snap.Agent.DefaultAgent
	if _, ok := reg[defaultID]; !ok {
		defaultID = ""
		for _, id := range snap.Agent.SessionAgents {
			if _, ok := reg[id]; ok {
				defaultID = id
				break
			}
		}
	}
	if defaultID == "" {
		defaultID = buildType
	}
	return reg, defaultID, rolePrompts, roleModels
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// params (JSON: temperature, top_p) into the agent core config.
func applyModelParams(cfg *config.AgentCoreConfig, md *nanoprotocol.ModelDef) {
	// The model's own thinking setting first; resolveThinkingEffort may override
	// it with the platform default or the user's explicit choice.
	cfg.ThinkingEffort = modelDefaultEffort(md)
	if md.Params == "" {
		return
	}
	var p struct {
		Temperature *float64 `json:"temperature"`
		TopP        *float64 `json:"top_p"`
	}
	if json.Unmarshal([]byte(md.Params), &p) == nil {
		if p.Temperature != nil {
			cfg.Temperature = float32(*p.Temperature)
		}
		if p.TopP != nil {
			cfg.TopP = float32(*p.TopP)
		}
	}
}
