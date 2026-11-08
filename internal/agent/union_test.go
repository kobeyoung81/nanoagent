package agent

import (
	"testing"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanosec/internal/provider"
)

func unionTestDefs() []provider.ToolDef {
	return []provider.ToolDef{
		{Name: "bash"}, {Name: "edit"}, {Name: "glob"},
		{Name: "read"}, {Name: "webfetch"}, {Name: "write"},
	}
}

func unionTestConfig() Config {
	readOnly := []nanoprotocol.PermissionRule{
		{Pattern: "*", Action: nanoprotocol.PermissionDeny},
		{Pattern: "read", Action: nanoprotocol.PermissionAllow},
		{Pattern: "glob", Action: nanoprotocol.PermissionAllow},
	}
	build := ResolvedAgent{ID: "build", Output: nanoprotocol.OutputStream,
		SessionBinding: nanoprotocol.SessionBindingShared}
	plan := ResolvedAgent{ID: "plan", Output: nanoprotocol.OutputStream,
		SessionBinding: nanoprotocol.SessionBindingShared, Permissions: readOnly}
	return Config{
		SessionAgentIDs: []string{"plan", "build"},
		Agents:          map[string]ResolvedAgent{"plan": plan, "build": build},
	}
}

// TestSessionUnionSpecsIsModeIndependent pins the property the prompt cache
// depends on: the definition block must be built from the same spec set whichever
// mode is active, otherwise a mode switch invalidates the whole request prefix.
func TestSessionUnionSpecsIsModeIndependent(t *testing.T) {
	cfg := unionTestConfig()

	fromPlan := cfg.sessionUnionSpecs(cfg.Agents["plan"])
	fromBuild := cfg.sessionUnionSpecs(cfg.Agents["build"])
	if len(fromPlan) != 2 || len(fromBuild) != 2 {
		t.Fatalf("session modes must union both modes: plan=%d build=%d specs", len(fromPlan), len(fromBuild))
	}
	if fromPlan[0].ID == fromBuild[0].ID && fromPlan[1].ID == fromBuild[1].ID {
		// same slice content in the same order: the union cannot depend on the mode
	} else if fromPlan[0].ID != fromBuild[0].ID || fromPlan[1].ID != fromBuild[1].ID {
		t.Errorf("union specs differ by active mode: %v vs %v", fromPlan, fromBuild)
	}

	// A non-session definition (subagent/utility) only unions itself.
	other := ResolvedAgent{ID: "role_explore"}
	if got := cfg.sessionUnionSpecs(other); len(got) != 1 || got[0].ID != "role_explore" {
		t.Errorf("non-session spec unioned %d specs, want just itself", len(got))
	}
}

// TestUnionKeepsEveryModesTools checks the shape of the shared block: plan's
// read-only rule no longer removes write/edit from the *definitions* (the block is
// shared with build), while execution still refuses them.
func TestUnionKeepsEveryModesTools(t *testing.T) {
	cfg := unionTestConfig()
	specs := cfg.sessionUnionSpecs(cfg.Agents["plan"])

	names := map[string]bool{}
	for _, d := range unionByRules(unionTestDefs(), specs) {
		names[d.Name] = true
	}
	for _, want := range []string{"bash", "edit", "glob", "read", "webfetch", "write"} {
		if !names[want] {
			t.Errorf("union is missing %q — the shared block must carry both modes' tools", want)
		}
	}

	// The plan mode itself still denies the write tools at execution time.
	if permissionFor("write", cfg.Agents["plan"].Permissions) != nanoprotocol.PermissionDeny {
		t.Error("plan must still deny write")
	}
}
