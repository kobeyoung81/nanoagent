package capability

import (
	"testing"

	"gitee.com/kobeyoung81/nanoprotocol"
)

func TestResolve_Matrix(t *testing.T) {
	resp := &nanoprotocol.CapabilitiesResponse{
		BuiltinTools: []nanoprotocol.BuiltinToolDef{
			{ID: "a", DefaultEnabled: true},
			{ID: "b", DefaultEnabled: true, Locked: "off"},
			{ID: "c", DefaultEnabled: false},
			{ID: "d", DefaultEnabled: false, Locked: "on"},
		},
		Attachments: []nanoprotocol.CapabilityAttachment{
			{Kind: nanoprotocol.CapabilityKindBuiltinTool, Ref: "a", Enabled: false},
			{Kind: nanoprotocol.CapabilityKindBuiltinTool, Ref: "c", Enabled: true},
		},
		McpServers: []nanoprotocol.McpServerDef{
			{ID: "m1", DefaultEnabled: false},
			{ID: "m2", DefaultEnabled: false, Locked: "on"},
		},
		Skills: []nanoprotocol.SkillDef{
			{ID: "s1", DefaultEnabled: false},
			{ID: "s2", DefaultEnabled: true, Locked: "off"},
		},
	}

	got := Resolve(resp)

	// a: default true overridden by attachment false → off
	if got.BuiltinEnabled["a"] {
		t.Error("a should be disabled by attachment override")
	}
	// b: locked off wins over default true → off
	if got.BuiltinEnabled["b"] {
		t.Error("b is locked off; should stay disabled")
	}
	// c: default false overridden by attachment true → on
	if !got.BuiltinEnabled["c"] {
		t.Error("c should be enabled by attachment override")
	}
	// d: locked on wins over default false → on
	if !got.BuiltinEnabled["d"] {
		t.Error("d is locked on; should be enabled")
	}
	if len(got.BuiltinEnabled) != 2 {
		t.Errorf("expected 2 enabled builtins, got %d", len(got.BuiltinEnabled))
	}
	if len(got.McpServers) != 1 || got.McpServers[0].ID != "m2" {
		t.Errorf("expected only locked-on m2 enabled, got %+v", got.McpServers)
	}
	if len(got.Skills) != 0 {
		t.Errorf("expected no enabled skills, got %+v", got.Skills)
	}
}

func TestEnabledMcpServers(t *testing.T) {
	catalog := []nanoprotocol.McpServerDef{
		{ID: "on-by-default", DefaultEnabled: true},
		{ID: "off-by-default", DefaultEnabled: false},
		{ID: "locked-on", DefaultEnabled: false, Locked: "on"},
		{ID: "locked-off", DefaultEnabled: true, Locked: "off"},
		{ID: "type-enabled", DefaultEnabled: false},
		{ID: "type-disabled", DefaultEnabled: true},
	}
	def := &nanoprotocol.AgentTypeDef{
		Capabilities: []nanoprotocol.TypeCapabilityBinding{
			{Kind: nanoprotocol.CapabilityKindMcp, Ref: "type-enabled", DefaultEnabled: true},
			{Kind: nanoprotocol.CapabilityKindMcp, Ref: "type-disabled", DefaultEnabled: false},
			// A type lock cannot override a global (definition-plane) lock.
			{Kind: nanoprotocol.CapabilityKindMcp, Ref: "locked-off", DefaultEnabled: true, Locked: "on"},
		},
	}
	got := EnabledMcpServers(def, catalog)
	want := map[string]bool{
		"on-by-default": true,
		"locked-on":     true,
		"type-enabled":  true,
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for id := range want {
		if !got[id] {
			t.Errorf("%q should be enabled; got %v", id, got)
		}
	}
	if got["off-by-default"] || got["type-disabled"] || got["locked-off"] {
		t.Errorf("disabled servers leaked (global lock is absolute): %v", got)
	}
	if EnabledMcpServers(nil, catalog) != nil {
		t.Error("nil def should return nil")
	}
}

func TestResolve_TypeBaselineOverridesCatalogDefault(t *testing.T) {
	resp := &nanoprotocol.CapabilitiesResponse{
		Skills: []nanoprotocol.SkillDef{{ID: "s1", DefaultEnabled: true, Locked: ""}},
		AgentType: &nanoprotocol.AgentTypeDef{
			Capabilities: []nanoprotocol.TypeCapabilityBinding{
				{Kind: nanoprotocol.CapabilityKindSkill, Ref: "s1", DefaultEnabled: false, Locked: "on"},
			},
		},
	}
	got := Resolve(resp)
	if len(got.Skills) != 1 {
		t.Fatalf("type baseline locked-on should enable s1, got %+v", got.Skills)
	}
}

// TestResolve_GlobalLockAbsolute pins that the definition-plane (global) lock
// is admin policy: it forces availability for every type/agent and cannot be
// overridden by a type lock or a per-agent attachment.
func TestResolve_GlobalLockAbsolute(t *testing.T) {
	resp := &nanoprotocol.CapabilitiesResponse{
		BuiltinTools: []nanoprotocol.BuiltinToolDef{
			{ID: "g-lock-on", DefaultEnabled: false, Locked: "on"},
			{ID: "g-lock-off", DefaultEnabled: true, Locked: "off"},
		},
		Attachments: []nanoprotocol.CapabilityAttachment{
			{Kind: nanoprotocol.CapabilityKindBuiltinTool, Ref: "g-lock-on", Enabled: false},
			{Kind: nanoprotocol.CapabilityKindBuiltinTool, Ref: "g-lock-off", Enabled: true},
		},
		AgentType: &nanoprotocol.AgentTypeDef{
			Capabilities: []nanoprotocol.TypeCapabilityBinding{
				{Kind: nanoprotocol.CapabilityKindBuiltinTool, Ref: "g-lock-on", DefaultEnabled: false, Locked: "off"},
				{Kind: nanoprotocol.CapabilityKindBuiltinTool, Ref: "g-lock-off", DefaultEnabled: true, Locked: "on"},
			},
		},
	}
	got := Resolve(resp)
	if !got.BuiltinEnabled["g-lock-on"] {
		t.Error("global lock on must force-enable despite type/attachment")
	}
	if got.BuiltinEnabled["g-lock-off"] {
		t.Error("global lock off must force-disable despite type/attachment")
	}
}

// TestEnabledBuiltinTools_GlobalDefault pins that a type without a binding
// inherits the global (definition) default, and a type binding overrides it.
func TestEnabledBuiltinTools_GlobalDefault(t *testing.T) {
	catalog := []nanoprotocol.BuiltinToolDef{
		{ID: "on", DefaultEnabled: true},
		{ID: "off", DefaultEnabled: false},
		{ID: "locked", DefaultEnabled: false, Locked: "on"},
	}
	def := &nanoprotocol.AgentTypeDef{}
	got := EnabledBuiltinTools(def, catalog)
	if !got["on"] || got["off"] || !got["locked"] {
		t.Fatalf("global default resolution wrong: %v", got)
	}
	def.Capabilities = []nanoprotocol.TypeCapabilityBinding{
		{Kind: nanoprotocol.CapabilityKindBuiltinTool, Ref: "off", DefaultEnabled: true},
	}
	if got := EnabledBuiltinTools(def, catalog); !got["off"] {
		t.Fatalf("type baseline should enable off: %v", got)
	}
	if EnabledBuiltinTools(def, nil) != nil {
		t.Error("empty catalog should return nil (no gating)")
	}
}
