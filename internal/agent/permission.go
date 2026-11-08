package agent

import (
	"encoding/json"
	"log/slog"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanosec/internal/provider"
	"gitee.com/kobeyoung81/nanosec/internal/session"
)

// SetPermissions installs the resolved permission rules (type baseline then
// per-agent overrides, last-match-wins).
func (a *Agent) SetPermissions(rules []nanoprotocol.PermissionRule) {
	a.permMu.Lock()
	a.permissions = rules
	a.permMu.Unlock()
}

// channelAllows reports whether an "ask" decision may proceed non-interactively
// on the given channel for a definition's channel-fallback policy (default
// deny).
func channelAllows(fallbacks map[string]string, channel string) bool {
	return fallbacks[channel] == string(nanoprotocol.PermissionAllow)
}

// Permissions returns a copy of the agent's resolved permission rules.
func (a *Agent) Permissions() []nanoprotocol.PermissionRule {
	a.permMu.RLock()
	defer a.permMu.RUnlock()
	out := make([]nanoprotocol.PermissionRule, len(a.permissions))
	copy(out, a.permissions)
	return out
}

// sessionGrantsMetaKey is the session-metadata key holding per-mode permission
// grants recorded when the user answers "always allow". The value is a JSON
// object mapping a mode (agent type id) to the tool-name patterns granted for
// the rest of that session. It lives in session metadata (so it survives a
// reload and an agent restart) and is deliberately NOT written to the agent
// configuration, so every other session asks again.
const sessionGrantsMetaKey = "_session_grants"

// sessionGrants parses the per-mode grant map from a session's metadata.
func sessionGrants(sess *session.Session) map[string][]string {
	if sess == nil {
		return nil
	}
	raw := sess.GetMeta(sessionGrantsMetaKey)
	if raw == "" {
		return nil
	}
	out := map[string][]string{}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

// grantSessionTool records a pattern as allowed for one session+mode so repeated
// calls in the same conversation skip the ask prompt. The turn's spec rules are
// a snapshot taken at turn start; this grant bridges that window for the rest of
// the session without touching persistent config.
func (a *Agent) grantSessionTool(sess *session.Session, typeID, pattern string) {
	if sess == nil || pattern == "" {
		return
	}
	grants := sessionGrants(sess)
	if grants == nil {
		grants = map[string][]string{}
	}
	found := false
	for _, p := range grants[typeID] {
		if p == pattern {
			found = true
			break
		}
	}
	if !found {
		grants[typeID] = append(grants[typeID], pattern)
	}
	if b, err := json.Marshal(grants); err == nil {
		sess.SetMeta(sessionGrantsMetaKey, string(b))
	}
	if a.sessions != nil {
		if err := a.sessions.Save(sess); err != nil {
			slog.Warn("persist session permission grant failed", "session", sess.ID, "error", err)
		}
	}
}

// sessionToolGranted reports whether a tool matches a grant recorded for the
// session under the given mode.
func sessionToolGranted(sess *session.Session, typeID, toolName string) bool {
	for _, pattern := range sessionGrants(sess)[typeID] {
		if nanoprotocol.WildcardMatch(pattern, toolName) {
			return true
		}
	}
	return false
}

// clearSessionGrants drops all grants for a session (e.g. a /new reset).
func (a *Agent) clearSessionGrants(sess *session.Session) {
	if sess == nil {
		return
	}
	sess.SetMeta(sessionGrantsMetaKey, "")
}

// PermissionFor returns the action for a tool. An empty rule set (or no
// matching rule) defaults to allow so existing agents are unaffected until an
// explicit policy is configured.
func (a *Agent) PermissionFor(tool string) nanoprotocol.PermissionAction {
	a.permMu.RLock()
	defer a.permMu.RUnlock()
	return permissionFor(tool, a.permissions)
}

// permissionFor evaluates a rule set (last match wins; default allow).
func permissionFor(tool string, rules []nanoprotocol.PermissionRule) nanoprotocol.PermissionAction {
	return nanoprotocol.EffectivePermission(tool, rules)
}

// filterByPermission drops tool definitions whose permission is deny so the
// model never sees them.
func (a *Agent) filterByPermission(defs []provider.ToolDef) []provider.ToolDef {
	a.permMu.RLock()
	rules := a.permissions
	a.permMu.RUnlock()
	return filterByRules(defs, rules)
}

// filterByRules drops tool definitions denied by the given rules.
func filterByRules(defs []provider.ToolDef, rules []nanoprotocol.PermissionRule) []provider.ToolDef {
	hasDeny := false
	for _, r := range rules {
		if r.Action == nanoprotocol.PermissionDeny {
			hasDeny = true
			break
		}
	}
	if !hasDeny {
		return defs
	}
	out := make([]provider.ToolDef, 0, len(defs))
	for _, d := range defs {
		if permissionFor(d.Name, rules) != nanoprotocol.PermissionDeny {
			out = append(out, d)
		}
	}
	return out
}

// filterByDefinition applies the active definition's tool gating: builtin tools
// via its builtin allowlist (nil = all), MCP tools via its enabled-server set.
// Tools of unknown provenance are kept. When builtins is nil (no provenance
// recorded) the legacy behavior applies — every tool is filtered by the
// allowlist.
func filterByDefinition(defs []provider.ToolDef, builtins map[string]bool, mcpServerByTool map[string]string, spec ResolvedAgent) []provider.ToolDef {
	out := make([]provider.ToolDef, 0, len(defs))
	for _, d := range defs {
		if isMcp, srv := mcpTool(mcpServerByTool, d.Name); isMcp {
			if spec.McpServers[srv] {
				out = append(out, d)
			}
			continue
		}
		if builtins == nil {
			if spec.EnabledTools == nil || spec.EnabledTools[d.Name] {
				out = append(out, d)
			}
			continue
		}
		if !builtins[d.Name] {
			out = append(out, d) // unknown provenance: keep
			continue
		}
		if spec.EnabledTools == nil || spec.EnabledTools[d.Name] {
			out = append(out, d)
		}
	}
	return out
}

// unionByDefinition keeps every tool at least one of the specs may use. A
// session agent's modes (build/plan) share one conversation, so they must share
// one tool-definition block: the upstream caches the request prefix by content,
// and a per-mode list made every mode switch a full cache miss (measured: a
// tool-set change dropped cached tokens from 98.5% to 0). What a mode may
// actually *call* is still decided per turn by its permission rules — see
// filterByRules and the executor's defense-in-depth check.
func unionByDefinition(defs []provider.ToolDef, builtins map[string]bool, mcpServerByTool map[string]string, specs []ResolvedAgent) []provider.ToolDef {
	if len(specs) == 0 {
		return defs
	}
	keep := make(map[string]bool, len(defs))
	for _, spec := range specs {
		for _, d := range filterByDefinition(defs, builtins, mcpServerByTool, spec) {
			keep[d.Name] = true
		}
	}
	out := make([]provider.ToolDef, 0, len(defs))
	for _, d := range defs {
		if keep[d.Name] {
			out = append(out, d)
		}
	}
	return out
}

// unionByRules is unionByDefinition's permission twin: a definition survives
// when any of the specs may use it, so the block does not change when the
// active mode does.
func unionByRules(defs []provider.ToolDef, specs []ResolvedAgent) []provider.ToolDef {
	if len(specs) == 0 {
		return defs
	}
	keep := make(map[string]bool, len(defs))
	for _, spec := range specs {
		for _, d := range filterByRules(defs, spec.Permissions) {
			keep[d.Name] = true
		}
	}
	hasAny := false
	for _, spec := range specs {
		for _, r := range spec.Permissions {
			if r.Action == nanoprotocol.PermissionDeny {
				hasAny = true
			}
		}
	}
	if !hasAny {
		return defs
	}
	out := make([]provider.ToolDef, 0, len(defs))
	for _, d := range defs {
		if keep[d.Name] {
			out = append(out, d)
		}
	}
	return out
}

// mcpTool reports whether a tool name is an MCP adapter and, if so, its server.
func mcpTool(mcpServerByTool map[string]string, name string) (bool, string) {
	if mcpServerByTool == nil {
		return false, ""
	}
	srv, ok := mcpServerByTool[name]
	return ok, srv
}

// modeExecutableTools splits the agent's tools for one definition into what it
// may call and what it will refuse. It applies the same two judgements the turn
// applies — the definition's tool gating and its permission rules — so a prompt
// that lists them cannot drift from what the executor accepts.
func (a *Agent) modeExecutableTools(spec ResolvedAgent) (allowed, denied []string) {
	if a.tools == nil {
		return nil, nil
	}
	builtinNames, mcpServerByTool := a.toolProvenance()
	for _, d := range filterByDefinition(a.tools.Definitions(), builtinNames, mcpServerByTool, spec) {
		if permissionFor(d.Name, spec.Permissions) == nanoprotocol.PermissionDeny {
			denied = append(denied, d.Name)
			continue
		}
		allowed = append(allowed, d.Name)
	}
	return allowed, denied
}
