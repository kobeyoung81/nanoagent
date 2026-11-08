package agent

import (
	"strings"
	"testing"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanosec/internal/tool"
)

// TestModeNoteToolsFollowTheLiveRules pins the point of the {tools} placeholder:
// the list in the mode-switch prompt is rendered from the same judgement the
// executor applies per call, so flipping the rules flips the list. A hardcoded
// list would fail the second half of this test.
func TestModeNoteToolsFollowTheLiveRules(t *testing.T) {
	a, sess := newSessionFilesAgent(t)
	reg := tool.NewRegistry()
	reg.Register(&MockTool{NameVal: "read"})
	reg.Register(&MockTool{NameVal: "bash"})
	a.tools = reg

	spec := ResolvedAgent{
		ID:       "plan",
		Settings: map[string]string{"mode_note": "mode={mode} tools={tools}"},
		Permissions: []nanoprotocol.PermissionRule{
			{Pattern: "*", Action: nanoprotocol.PermissionDeny},
			{Pattern: "read", Action: nanoprotocol.PermissionAllow},
		},
	}

	first := a.modeNoteBody(sess, spec)
	if !strings.Contains(first, "mode=plan") {
		t.Fatalf("the {mode} hole should still be filled: %q", first)
	}
	if !strings.HasPrefix(first, "【模式状态】mode=plan tools=read") {
		t.Errorf("expected read to be the allowed tool, got %q", first)
	}
	if !strings.Contains(first, "refused in this mode: bash") {
		t.Errorf("expected bash to be listed as refused, got %q", first)
	}

	// Flip which tool the rules allow: the note has to follow.
	spec.Permissions = []nanoprotocol.PermissionRule{
		{Pattern: "*", Action: nanoprotocol.PermissionDeny},
		{Pattern: "bash", Action: nanoprotocol.PermissionAllow},
	}
	second := a.modeNoteBody(sess, spec)
	if !strings.Contains(second, "tools=bash") {
		t.Errorf("expected bash to be allowed now, got %q", second)
	}
	if !strings.Contains(second, "refused in this mode: read") {
		t.Errorf("expected read to be refused now, got %q", second)
	}
	if first == second {
		t.Error("the rendered tool list must change with the rules")
	}
}

// TestModeNoteToolsTolerateAMissingRegistry pins that a bare agent (no registry)
// and a template without the placeholder both stay safe.
func TestModeNoteToolsTolerateAMissingRegistry(t *testing.T) {
	a, sess := newSessionFilesAgent(t)
	spec := ResolvedAgent{
		ID:          "build",
		Settings:    map[string]string{"mode_note": "mode={mode} tools={tools}"},
		Permissions: []nanoprotocol.PermissionRule{{Pattern: "*", Action: nanoprotocol.PermissionAllow}},
	}
	if got := a.modeNoteBody(sess, spec); !strings.Contains(got, "tools=(none)") {
		t.Errorf("a missing registry should render (none), got %q", got)
	}

	spec.Settings = map[string]string{"mode_note": "mode={mode}"}
	if got := a.modeNoteBody(sess, spec); strings.Contains(got, "{tools}") || got != "【模式状态】mode=build" {
		t.Errorf("a template without the placeholder must pass through, got %q", got)
	}
}
