package agent

import (
	"context"
	"sort"
	"strconv"
	"time"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanosec/internal/provider"
)

// ResolvedAgent is a fully-resolved agent definition the runtime can execute.
// It is built by the capability reconciler from the snapshot's unified Agents
// list: session agents (build/plan) share the conversation, subagents run
// isolated, and utilities run one-shot.
type ResolvedAgent struct {
	ID string
	// Role is the short invocation tag (e.g. "general", "title") used to key
	// helper definitions; empty for session agents.
	Role            string
	Prompt          string
	Model           string
	ReasoningEffort string
	Temperature     *float32
	TopP            float32
	// EnabledTools is the builtin-tool allowlist derived from the definition's
	// capability baseline. nil means "no baseline" (all tools). It gates
	// builtin tools only; MCP tools are gated by McpServers.
	EnabledTools map[string]bool
	// McpServers is the set of MCP server ids enabled for this definition
	// (capability baseline + per-(agent,type) override + catalog default).
	McpServers map[string]bool
	// ChannelFallbacks is the definition's per-channel policy for
	// unsatisfiable "ask" decisions (empty = deny).
	ChannelFallbacks map[string]string
	Permissions      []nanoprotocol.PermissionRule
	MaxRounds        int
	Strategy         string // react | oneshot
	Context          string // windowed | recent | incremental | conversation | task | none
	Output           string // stream | session_meta | summary | caller | none
	// SessionBinding is shared | scratch | isolated.
	SessionBinding string
	Settings       map[string]string

	// Provider is the LLM client for this definition's model connection.
	Provider provider.Provider
}

// setting returns a definition setting, or the fallback.
func (r ResolvedAgent) setting(key, fallback string) string {
	if r.Settings != nil {
		if v, ok := r.Settings[key]; ok && v != "" {
			return v
		}
	}
	return fallback
}

// settingInt returns an integer definition setting, or the fallback.
func (r ResolvedAgent) settingInt(key string, fallback int) int {
	if v := r.setting(key, ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

// settingDuration returns a duration definition setting, or the fallback.
func (r ResolvedAgent) settingDuration(key string, fallback time.Duration) time.Duration {
	if v := r.setting(key, ""); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

// settingBoolPresent returns a boolean definition setting and whether it was
// explicitly set (a tri-state so callers can distinguish "inherit" from false).
func (r ResolvedAgent) settingBoolPresent(key string) (*bool, bool) {
	if r.Settings == nil {
		return nil, false
	}
	v, ok := r.Settings[key]
	if !ok || v == "" {
		return nil, false
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return nil, false
	}
	return &b, true
}

// isSessionAgent reports whether a definition id is a switchable session agent.
// It prefers the explicit per-agent link list and falls back to the derived
// shape (output=stream, session_binding=shared).
func (c Config) isSessionAgent(id string) bool {
	if id == "" {
		return false
	}
	if len(c.SessionAgentIDs) > 0 {
		for _, s := range c.SessionAgentIDs {
			if s == id {
				return true
			}
		}
		return false
	}
	spec, ok := c.Agents[id]
	return ok && spec.Output == nanoprotocol.OutputStream && spec.SessionBinding == nanoprotocol.SessionBindingShared
}

// SessionAgents returns the session definitions in the registry. When the
// explicit switchable list is present it is honored in order; otherwise the
// definitions are derived from their execution attributes.
func (c Config) SessionAgents() []ResolvedAgent {
	out := make([]ResolvedAgent, 0, 2)
	if len(c.SessionAgentIDs) > 0 {
		for _, id := range c.SessionAgentIDs {
			if spec, ok := c.Agents[id]; ok {
				out = append(out, spec)
			}
		}
		return out
	}
	for _, a := range c.Agents {
		if a.Output == nanoprotocol.OutputStream && a.SessionBinding == nanoprotocol.SessionBindingShared {
			out = append(out, a)
		}
	}
	return out
}

// sessionUnionSpecs returns the definitions whose tool gating is unioned into
// the LLM tool block: every session mode of the agent when the active spec is a
// session agent, otherwise just the active spec. The union is what keeps the
// block byte-identical across a mode switch (see unionByDefinition).
func (c Config) sessionUnionSpecs(spec ResolvedAgent) []ResolvedAgent {
	if c.isSessionAgent(spec.ID) {
		if specs := c.SessionAgents(); len(specs) > 0 {
			return specs
		}
	}
	return []ResolvedAgent{spec}
}

// resolveSession picks the session agent for an inbound message: an explicit
// metadata "agent"/"agent_mode" value wins when it names a known session
// definition; otherwise the configured default, else the first session agent.
func (a *Agent) resolveSession(msg *nanoprotocol.InboundMessage) ResolvedAgent {
	if msg != nil && msg.Metadata != nil {
		for _, key := range []string{"agent", "agent_mode"} {
			if v, ok := msg.Metadata[key]; ok {
				if s, ok := v.(string); ok && s != "" {
					if a.config.isSessionAgent(s) {
						if spec, ok := a.config.Agents[s]; ok {
							return spec
						}
					}
					// Legacy/UI aliases: "plan" selects the platform plan agent,
					// "build" selects the primary (non-plan) session agent.
					if s == "plan" && a.config.isSessionAgent("plan") {
						return a.config.Agents["plan"]
					}
					if s == "build" {
						for _, spec := range a.config.SessionAgents() {
							if spec.ID != "plan" {
								return spec
							}
						}
					}
				}
			}
		}
	}
	if a.config.isSessionAgent(a.config.DefaultAgent) {
		if spec, ok := a.config.Agents[a.config.DefaultAgent]; ok {
			return spec
		}
	}
	for _, spec := range a.config.SessionAgents() {
		return spec
	}
	// No registry (bare tests / pre-AgentType agents): synthesize from config.
	return ResolvedAgent{
		ID:             "build",
		Model:          a.config.Model,
		Permissions:    a.Permissions(),
		Strategy:       nanoprotocol.StrategyReact,
		Context:        nanoprotocol.ContextWindowed,
		Output:         nanoprotocol.OutputStream,
		SessionBinding: nanoprotocol.SessionBindingShared,
	}
}

// agentByID returns a resolved definition by id.
func (a *Agent) agentByID(id string) (ResolvedAgent, bool) {
	spec, ok := a.config.Agents[id]
	return spec, ok
}

// SubagentProfiles derives the subagent "newborn agent" profiles from the
// unified registry: definitions whose output sink is the caller (output=caller),
// keyed by their invocation role.
func (c Config) SubagentProfiles() map[string]SubagentProfile {
	out := map[string]SubagentProfile{}
	for _, spec := range c.Agents {
		if spec.Output != nanoprotocol.OutputCaller {
			continue
		}
		key := spec.Role
		if key == "" {
			key = spec.ID
		}
		restrict, _ := spec.settingBoolPresent("restrict_to_workspace")
		out[key] = SubagentProfile{
			Tools:         withoutDelegation(spec.EnabledTools),
			McpServers:    spec.McpServers,
			Prompt:        spec.Prompt,
			MaxIterations: spec.MaxRounds,
			Model:         spec.Model,
			// Already resolved for this subagent type by the caller (platform
			// layers only — subagents have no user-level setting).
			ReasoningEffort:     spec.ReasoningEffort,
			Permissions:         spec.Permissions,
			MaxToolResultChars:  spec.settingInt("max_tool_result_chars", 0),
			ShellTimeout:        spec.settingDuration("shell_timeout", 0),
			RestrictToWorkspace: restrict,
		}
	}
	return out
}

// DelegationToolNames are the tools that start another subagent. A subagent never
// receives them: delegation depth stays 1, so a runaway fan-out cannot hide
// behind nested subagents. The registry enforces this when a subagent registry
// is built (the names are unregistered); dropping them from the profile as well
// keeps the allowlist a profile carries from even mentioning them.
var DelegationToolNames = []string{"task", "readonly_task"}

// withoutDelegation copies an allowlist with the delegation tools switched off.
// A nil allowlist means "everything registered" and cannot express a removal, so
// it stays nil: the registry is what actually keeps the tools away in that case.
func withoutDelegation(enabled map[string]bool) map[string]bool {
	if enabled == nil {
		return nil
	}
	out := make(map[string]bool, len(enabled))
	for name, on := range enabled {
		out[name] = on
	}
	for _, name := range DelegationToolNames {
		delete(out, name)
	}
	return out
}

// SubagentProfilesFrom derives subagent profiles from a resolved registry.
func SubagentProfilesFrom(agents map[string]ResolvedAgent) map[string]SubagentProfile {
	return Config{Agents: agents}.SubagentProfiles()
}

// readOnlyProfileToolNames are the tool names that make a subagent capable of
// changing the workspace. A profile that enables any of them is not read-only.
var readOnlyProfileToolNames = []string{"write", "edit", "bash"}

// profileIsReadOnly reports whether a subagent type cannot modify anything: its
// builtin allowlist excludes every write/edit/full-shell tool, and it has no MCP
// servers enabled (an MCP tool may have side effects on the far side). A profile
// without an allowlist (Tools == nil) gets every tool, so it is deliberately not
// read-only.
func profileIsReadOnly(p SubagentProfile) bool {
	if p.Tools == nil || len(p.McpServers) > 0 {
		return false
	}
	for _, name := range readOnlyProfileToolNames {
		if p.Tools[name] {
			return false
		}
	}
	return true
}

// ReadOnlyProfileIDs returns the subagent types that cannot modify anything.
// Result is sorted for stable prompts and tests.
func ReadOnlyProfileIDs(profiles map[string]SubagentProfile) []string {
	out := make([]string, 0, len(profiles))
	for id, p := range profiles {
		if profileIsReadOnly(p) {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func effectiveModel(spec ResolvedAgent, fallback string) string {
	if spec.Model != "" {
		return spec.Model
	}
	return fallback
}

// effectiveReasoning is the definition's own level when it resolved one, else
// the agent-wide fallback seeded from the default mode.
func effectiveReasoning(spec ResolvedAgent, fallback string) string {
	if spec.ReasoningEffort != "" {
		return spec.ReasoningEffort
	}
	return fallback
}

// effectiveTemp resolves the sampling temperature: the definition's own value
// when it set one, else the agent-wide fallback. Both are optional, and both use
// nil for "not set" so an explicit 0 (deterministic decoding) survives.
func effectiveTemp(spec ResolvedAgent, fallback *float32) *float32 {
	if spec.Temperature != nil {
		return spec.Temperature
	}
	return fallback
}

// utilityTemperature is the temperature a one-shot utility call uses. A
// definition may pin one in its settings ("judge_temperature"), which is how the
// goal judge locks itself to deterministic output; otherwise the model entry's
// own params apply.
func utilityTemperature(spec ResolvedAgent) *float32 {
	raw := spec.setting("judge_temperature", "")
	if raw == "" {
		return spec.Temperature
	}
	v, err := strconv.ParseFloat(raw, 32)
	if err != nil {
		return spec.Temperature
	}
	t := float32(v)
	return &t
}

func effectiveTopP(spec ResolvedAgent, fallback float32) float32 {
	if spec.TopP != 0 {
		return spec.TopP
	}
	return fallback
}

// runUtility executes a one-shot utility definition (title, summarize, memory
// flush) with the definition's prompt and model. It returns "" and false when
// the definition is not registered, so callers can fall back to their built-in
// defaults. onDelta, when non-nil, receives streamed content deltas.
func (a *Agent) runUtility(ctx context.Context, id, userContent, sessionID string, onDelta func(string)) (string, bool, error) {
	spec, ok := a.config.Agents[id]
	if !ok {
		return "", false, nil
	}
	prov := spec.Provider
	if prov == nil {
		prov = a.Provider()
	}
	model := spec.Model
	if model == "" {
		model = a.config.Model
	}
	resp, err := chatStreamCollectWith(ctx, prov, provider.ChatRequest{
		Model:           model,
		ReasoningEffort: spec.ReasoningEffort,
		Temperature:     utilityTemperature(spec),
		TopP:            spec.TopP,
		SessionID:       sessionID + ":" + id,
		Messages: []provider.Message{
			{Role: "system", Content: spec.Prompt},
			{Role: "user", Content: userContent},
		},
	}, a.config.LLMRequestTimeout, onDelta)
	if err != nil {
		return "", true, err
	}
	return resp.Content, true, nil
}
