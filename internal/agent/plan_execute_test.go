package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanosec/internal/provider"
	"gitee.com/kobeyoung81/nanosec/internal/session"
	"gitee.com/kobeyoung81/nanosec/internal/tool"
)

// TestPlanExecute_RetiresModeNote covers the loop half of the plan -> build
// contract: closing the plan removes the mode note and the plan path (injected
// on every turn otherwise, still pointing at finished work) and records which
// artefact was closed.
func TestPlanExecute_RetiresModeNote(t *testing.T) {
	tmpDir := t.TempDir()
	ws := t.TempDir()
	plansDir := filepath.Join(ws, "plans")
	if err := os.MkdirAll(plansDir, 0o755); err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(plansDir, "x.md")
	if err := os.WriteFile(planPath, []byte("# x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sessStore, err := session.NewStore(tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	reg := tool.NewRegistry()
	reg.Register(tool.NewPlanExecuteTool(ws, ""))

	step := 0
	mockProvider := provider.NewMock()
	mockProvider.ResponseFunc = func(context.Context, provider.ChatRequest) (*provider.ChatResponse, error) {
		step++
		if step == 1 {
			return &provider.ChatResponse{ToolCalls: []provider.ToolCall{{
				ID:        "c1",
				Name:      "plan_execute",
				Arguments: `{"status":"done","summary":"implemented and verified"}`,
			}}}, nil
		}
		return &provider.ChatResponse{Content: "closed out"}, nil
	}
	ag, err := New(Config{UserID: "u", MaxIterations: 5, SessionDir: tmpDir}, &MockBusClient{}, mockProvider, reg)
	if err != nil {
		t.Fatal(err)
	}
	ag.sessions = sessStore

	sess := sessStore.GetOrCreate("web:chat1")
	sess.SetMeta(lastPlanPathMetaKey, planPath)
	sess.SetMeta(sessionModeNoteMetaKey, "Mode: build. A plan artefact is at "+planPath)

	if err := ag.processMessage(context.Background(), &nanoprotocol.InboundMessage{
		UserID: "u", SessionID: "web:chat1", Channel: "web", ChatID: "chat1", Content: "do the plan",
	}); err != nil {
		t.Fatalf("processMessage: %v", err)
	}

	sess = sessStore.GetOrCreate("web:chat1")
	if got := sess.GetMeta(planExecutedMetaKey); got != planPath {
		t.Errorf("plan_executed = %q, want %q", got, planPath)
	}
	if _, ok := sess.GetAllMeta()[sessionModeNoteMetaKey]; ok {
		t.Error("the mode note must be removed once the plan is closed")
	}
	if _, ok := sess.GetAllMeta()[lastPlanPathMetaKey]; ok {
		t.Error("the plan path must be removed once the plan is closed")
	}
	report := strings.TrimSuffix(planPath, ".md") + ".report.md"
	if _, err := os.Stat(report); err != nil {
		t.Errorf("expected an execution report next to the plan: %v", err)
	}
}
