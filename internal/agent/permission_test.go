package agent

import (
	"testing"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanosec/internal/provider"
	"gitee.com/kobeyoung81/nanosec/internal/session"
)

func TestWildcardMatch(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"bash", "bash", true},
		{"bash", "edit", false},
		{"mcp__server__*", "mcp__server__search", true},
		{"mcp__*", "mcp__other__tool", true},
		{"mcp__*", "bash", false},
		{"*_media", "download_media", true},
		{"*_media", "upload_media", true},
		{"*_media", "write", false},
		{"*", "anything", true},
	}
	for _, tc := range cases {
		if got := nanoprotocol.WildcardMatch(tc.pattern, tc.name); got != tc.want {
			t.Errorf("WildcardMatch(%q, %q) = %v, want %v", tc.pattern, tc.name, got, tc.want)
		}
	}
}

func TestPermissionFor_LastMatchWins(t *testing.T) {
	a := &Agent{}
	a.SetPermissions([]nanoprotocol.PermissionRule{
		{Pattern: "*", Action: nanoprotocol.PermissionAllow},
		{Pattern: "bash", Action: nanoprotocol.PermissionAsk},
		{Pattern: "mcp__*", Action: nanoprotocol.PermissionDeny},
	})
	if got := a.PermissionFor("read"); got != nanoprotocol.PermissionAllow {
		t.Errorf("read = %v, want allow", got)
	}
	if got := a.PermissionFor("bash"); got != nanoprotocol.PermissionAsk {
		t.Errorf("bash = %v, want ask", got)
	}
	if got := a.PermissionFor("mcp__x__tool"); got != nanoprotocol.PermissionDeny {
		t.Errorf("mcp tool = %v, want deny", got)
	}
}

func TestPermissionFor_EmptyDefaultsAllow(t *testing.T) {
	a := &Agent{}
	if got := a.PermissionFor("bash"); got != nanoprotocol.PermissionAllow {
		t.Errorf("default = %v, want allow", got)
	}
}

func TestIsInteractiveChannel(t *testing.T) {
	for _, ch := range []string{"web", "cli"} {
		if !isInteractiveChannel(ch) {
			t.Errorf("%q should be interactive", ch)
		}
	}
	for _, ch := range []string{"wecom", "feishu", "wechatkf", ""} {
		if isInteractiveChannel(ch) {
			t.Errorf("%q should not be interactive", ch)
		}
	}
}

func TestChannelAllows(t *testing.T) {
	fallbacks := map[string]string{"wecom": "allow"}
	if !channelAllows(fallbacks, "wecom") {
		t.Error("wecom fallback allow should permit")
	}
	if channelAllows(fallbacks, "feishu") {
		t.Error("unset channel should default deny")
	}
	if channelAllows(nil, "wecom") {
		t.Error("nil fallbacks should default deny")
	}
}

func TestFilterByDefinition(t *testing.T) {
	defs := []provider.ToolDef{
		{Name: "read"},            // builtin, allowed
		{Name: "bash"},            // builtin, not allowlisted
		{Name: "mcp__gh__search"}, // MCP, enabled server
		{Name: "mcp__db__drop"},   // MCP, disabled server
		{Name: "mystery"},         // unknown provenance
	}
	builtins := map[string]bool{"read": true, "bash": true}
	mcpServerByTool := map[string]string{
		"mcp__gh__search": "gh",
		"mcp__db__drop":   "db",
	}
	spec := ResolvedAgent{
		EnabledTools: map[string]bool{"read": true},
		McpServers:   map[string]bool{"gh": true},
	}

	got := filterByDefinition(defs, builtins, mcpServerByTool, spec)
	names := map[string]bool{}
	for _, d := range got {
		names[d.Name] = true
	}
	if !names["read"] {
		t.Error("allowlisted builtin should be kept")
	}
	if names["bash"] {
		t.Error("non-allowlisted builtin should be dropped")
	}
	if !names["mcp__gh__search"] {
		t.Error("MCP tool on an enabled server should be kept")
	}
	if names["mcp__db__drop"] {
		t.Error("MCP tool on a disabled server should be dropped")
	}
	if !names["mystery"] {
		t.Error("unknown-provenance tool should be kept")
	}
}

func TestFilterByDefinition_LegacyFallback(t *testing.T) {
	// No provenance recorded: every tool is filtered by the allowlist.
	defs := []provider.ToolDef{{Name: "read"}, {Name: "bash"}}
	spec := ResolvedAgent{EnabledTools: map[string]bool{"read": true}}
	got := filterByDefinition(defs, nil, nil, spec)
	if len(got) != 1 || got[0].Name != "read" {
		t.Fatalf("legacy fallback filter = %+v, want [read]", got)
	}
}

func TestPermissionSummary(t *testing.T) {
	if got := permissionSummary("bash", `{"command":"rm -rf ./tmp"}`); got != "rm -rf ./tmp" {
		t.Fatalf("bash summary = %q", got)
	}
	if got := permissionSummary("read", `{"path":"/etc/hosts"}`); got == "" {
		t.Fatal("non-bash summary should not be empty")
	}
	if got := permissionSummary("bash", ""); got != "" {
		t.Fatalf("empty args summary = %q, want empty", got)
	}
}

func TestExtractApprovalDecision(t *testing.T) {
	msg := &nanoprotocol.InboundMessage{Metadata: map[string]any{
		nanoprotocol.MetadataApprovalID: "a1",
		nanoprotocol.MetadataDecision:   string(nanoprotocol.PermissionDecisionAlwaysAllow),
	}}
	got := extractApprovalDecision(msg)
	if len(got) != 1 || got[0] != string(nanoprotocol.PermissionDecisionAlwaysAllow) {
		t.Fatalf("extractApprovalDecision = %v", got)
	}
}

func TestSessionToolGrants(t *testing.T) {
	a := &Agent{}
	sess := session.New("s1")
	other := session.New("s2")

	if sessionToolGranted(sess, "build", "bash") {
		t.Fatal("ungranted tool must not be granted")
	}

	a.grantSessionTool(sess, "build", "bash")
	if !sessionToolGranted(sess, "build", "bash") {
		t.Fatal("granted tool must be granted")
	}
	if sessionToolGranted(other, "build", "bash") {
		t.Fatal("grant must be session-scoped")
	}
	if sessionToolGranted(sess, "plan", "bash") {
		t.Fatal("grant must be mode-scoped")
	}
	if sessionToolGranted(sess, "build", "write") {
		t.Fatal("grant must not leak to a different tool")
	}

	// Wildcard grants match a family of tools.
	a.grantSessionTool(sess, "build", "mcp__*")
	if !sessionToolGranted(sess, "build", "mcp__github__search") {
		t.Fatal("wildcard grant should match an mcp tool")
	}

	// Grants live in session metadata, so they survive a reload.
	reloaded := session.New("s1")
	reloaded.SetMeta(sessionGrantsMetaKey, sess.GetMeta(sessionGrantsMetaKey))
	if !sessionToolGranted(reloaded, "build", "bash") || !sessionToolGranted(reloaded, "build", "mcp__github__search") {
		t.Fatal("grants should survive a reload via session metadata")
	}

	a.clearSessionGrants(sess)
	if sessionToolGranted(sess, "build", "bash") || sessionToolGranted(sess, "build", "mcp__github__search") {
		t.Fatal("clearSessionGrants must drop all grants for the session")
	}
}
