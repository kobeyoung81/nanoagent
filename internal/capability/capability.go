// Package capability applies the catalog resolution rule to an agent's
// capability snapshot. It is the single implementation consumed by both the
// agent process (tools + MCP) and the hypervisor (skill materialization).
package capability

import "gitee.com/kobeyoung81/nanoprotocol"

// Resolved is the outcome of applying the resolution rule
// (locked → per-agent attachment → type default) to an agent's snapshot.
type Resolved struct {
	// BuiltinEnabled maps builtin tool id → whether it is enabled for this agent.
	BuiltinEnabled map[string]bool
	// McpServers are the enabled MCP servers (full defs, credentials intact).
	McpServers []nanoprotocol.McpServerDef
	// Skills are the enabled skills (with files) to materialize.
	Skills []nanoprotocol.SkillDef
}

// Resolve applies the resolution rule to a catalog snapshot.
func Resolve(resp *nanoprotocol.CapabilitiesResponse) Resolved {
	out := Resolved{BuiltinEnabled: map[string]bool{}}
	if resp == nil {
		return out
	}

	attachment := map[string]bool{}
	has := map[string]bool{}
	for _, a := range resp.Attachments {
		key := string(a.Kind) + "/" + a.Ref
		attachment[key] = a.Enabled
		has[key] = true
	}
	// Platform baseline lives on the AgentType, not the catalog item.
	typeBase := map[string]nanoprotocol.TypeCapabilityBinding{}
	if resp.AgentType != nil {
		for _, c := range resp.AgentType.Capabilities {
			typeBase[string(c.Kind)+"/"+c.Ref] = c
		}
	}
	eff := func(kind, ref string, def bool, locked string) bool {
		return effectiveCapability(kind, ref, def, locked, typeBase, attachment, has)
	}

	for _, t := range resp.BuiltinTools {
		if eff(string(nanoprotocol.CapabilityKindBuiltinTool), t.ID, t.DefaultEnabled, t.Locked) {
			out.BuiltinEnabled[t.ID] = true
		}
	}
	for _, m := range resp.McpServers {
		if eff(string(nanoprotocol.CapabilityKindMcp), m.ID, m.DefaultEnabled, m.Locked) {
			out.McpServers = append(out.McpServers, m)
		}
	}
	for _, sk := range resp.Skills {
		if eff(string(nanoprotocol.CapabilityKindSkill), sk.ID, sk.DefaultEnabled, sk.Locked) {
			out.Skills = append(out.Skills, sk)
		}
	}
	return out
}

// effectiveCapability applies the single resolution rule to one capability:
// admin global lock (absolute) > type lock > per-agent/mode override >
// type default > global (definition) default. typeBase/attachment/has may be
// nil. The global lock and global default come from the catalog item (the
// definition-plane row).
func effectiveCapability(kind, ref string, globalDef bool, globalLock string, typeBase map[string]nanoprotocol.TypeCapabilityBinding, attachment, has map[string]bool) bool {
	key := kind + "/" + ref
	// The definition-plane (global) lock is admin policy: it forces availability
	// for every type and agent, so it is checked first and is not overridable.
	switch globalLock {
	case "on":
		return true
	case "off":
		return false
	}
	def := globalDef
	if b, ok := typeBase[key]; ok {
		def = b.DefaultEnabled
		switch b.Locked {
		case "on":
			return true
		case "off":
			return false
		}
	}
	if has[key] {
		return attachment[key]
	}
	return def
}

// EnabledBuiltinTools resolves the builtin tools enabled for one agent
// definition (session agent, subagent or utility). The definition's
// per-(agent,type) override is already merged into its Capabilities by the bus,
// so no separate attachment layer applies here. Rule: global lock (absolute) >
// type lock > type default > global default. Returns the enabled tool-id set;
// nil when the catalog is unavailable (caller treats nil as "no gating").
func EnabledBuiltinTools(def *nanoprotocol.AgentTypeDef, catalog []nanoprotocol.BuiltinToolDef) map[string]bool {
	if def == nil || len(catalog) == 0 {
		return nil
	}
	typeBase := map[string]nanoprotocol.TypeCapabilityBinding{}
	for _, c := range def.Capabilities {
		typeBase[string(c.Kind)+"/"+c.Ref] = c
	}
	out := map[string]bool{}
	for _, t := range catalog {
		if effectiveCapability(string(nanoprotocol.CapabilityKindBuiltinTool), t.ID, t.DefaultEnabled, t.Locked, typeBase, nil, nil) {
			out[t.ID] = true
		}
	}
	return out
}

// EnabledMcpServers resolves the MCP servers enabled for one agent definition
// (session agent, subagent or utility). The definition's per-(agent,type)
// override is already merged into its Capabilities by the bus, so no separate
// attachment layer applies here. Rule: admin lock > type baseline > catalog
// default. Returns the set of enabled server ids (possibly empty).
func EnabledMcpServers(def *nanoprotocol.AgentTypeDef, catalog []nanoprotocol.McpServerDef) map[string]bool {
	if def == nil {
		return nil
	}
	typeBase := map[string]nanoprotocol.TypeCapabilityBinding{}
	for _, c := range def.Capabilities {
		typeBase[string(c.Kind)+"/"+c.Ref] = c
	}
	out := map[string]bool{}
	for _, m := range catalog {
		if effectiveCapability(string(nanoprotocol.CapabilityKindMcp), m.ID, m.DefaultEnabled, m.Locked, typeBase, nil, nil) {
			out[m.ID] = true
		}
	}
	return out
}
