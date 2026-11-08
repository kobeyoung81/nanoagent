package tool

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestSharedWorkspace_ReadWrite verifies filesystem tools can access the shared
// workspace while still rejecting paths outside both roots.
func TestSharedWorkspace_ReadWrite(t *testing.T) {
	root := t.TempDir()
	agentWS := filepath.Join(root, "agents", "agent-1")
	sharedWS := filepath.Join(root, "shared")
	for _, dir := range []string{agentWS, sharedWS} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	sharedFile := filepath.Join(sharedWS, "notes.md")
	if err := os.WriteFile(sharedFile, []byte("shared content"), 0o644); err != nil {
		t.Fatalf("write shared file: %v", err)
	}

	// Read a file in the shared workspace using a path relative to the agent
	// workspace (via ../../shared/...) — must be permitted.
	rel := filepath.Join("..", "..", "shared", "notes.md")
	reader := &ReadFileTool{Workspace: agentWS, SharedWorkspace: sharedWS}
	content, err := reader.Execute(context.Background(), map[string]any{"path": rel})
	if err != nil {
		t.Fatalf("read shared file: %v", err)
	}
	if content != "shared content" {
		t.Errorf("content = %q", content)
	}

	// Write to the shared workspace directly.
	writer := &WriteFileTool{Workspace: agentWS, SharedWorkspace: sharedWS}
	if _, err := writer.Execute(context.Background(), map[string]any{
		"path":    filepath.Join(sharedWS, "new.txt"),
		"content": "hello",
	}); err != nil {
		t.Fatalf("write shared file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sharedWS, "new.txt")); err != nil {
		t.Errorf("shared file not created: %v", err)
	}
}

// TestSharedWorkspace_RejectsOutside verifies tools without the shared root
// cannot escape their workspace.
func TestSharedWorkspace_RejectsOutside(t *testing.T) {
	root := t.TempDir()
	agentWS := filepath.Join(root, "agents", "agent-1")
	if err := os.MkdirAll(agentWS, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	reader := &ReadFileTool{Workspace: agentWS, SharedWorkspace: filepath.Join(root, "shared")}
	if _, err := reader.Execute(context.Background(), map[string]any{"path": "../../secret.txt"}); err == nil {
		t.Fatal("expected escape path to be rejected")
	}
}
