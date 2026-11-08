package agent

import (
	"reflect"
	"testing"
)

func TestReadOnlyProfileIDs(t *testing.T) {
	profiles := map[string]SubagentProfile{
		// Read-only: no write/edit/full shell, no MCP.
		"explore": {Tools: map[string]bool{"read": true, "list": true, "grep": true, "glob": true, "webfetch": true}},
		// The whitelisted shell is not a write capability.
		"scout": {Tools: map[string]bool{"read": true, "grep": true, "readonly_bash": true, "webfetch": true}},
		// Write-capable through the file tools.
		"general": {Tools: map[string]bool{"read": true, "write": true, "edit": true, "bash": true}},
		// Write-capable only through the full shell.
		"runner": {Tools: map[string]bool{"read": true, "bash": true}},
		// A profile without an allowlist gets every tool.
		"unlisted": {Tools: nil, McpServers: map[string]bool{}},
		// MCP tools may have side effects on the far side.
		"mcp": {Tools: map[string]bool{"read": true}, McpServers: map[string]bool{"github": true}},
	}

	got := ReadOnlyProfileIDs(profiles)
	want := []string{"explore", "scout"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ReadOnlyProfileIDs() = %v, want %v", got, want)
	}
}

func TestReadOnlyProfileIDs_Empty(t *testing.T) {
	if got := ReadOnlyProfileIDs(nil); len(got) != 0 {
		t.Errorf("ReadOnlyProfileIDs(nil) = %v, want empty", got)
	}
}
