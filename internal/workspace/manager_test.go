package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestManager_Paths(t *testing.T) {
	root := t.TempDir()
	m := New("agent-1", root)

	if got := m.AgentWorkspace(); got != filepath.Join(root, "agents", "agent-1") {
		t.Errorf("AgentWorkspace = %q", got)
	}
	if got := m.SharedWorkspace(); got != filepath.Join(root, "sharedspace") {
		t.Errorf("SharedWorkspace = %q", got)
	}

	if err := m.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}
	for _, dir := range []string{m.AgentWorkspace(), m.SharedWorkspace()} {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("expected dir %q to exist: %v", dir, err)
		}
	}
}

func TestManager_Resolve_WithinAgent(t *testing.T) {
	root := t.TempDir()
	m := New("agent-1", root)
	_ = m.EnsureDirs()

	got, err := m.Resolve("notes.md")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := filepath.Join(m.AgentWorkspace(), "notes.md")
	if got != want {
		t.Errorf("Resolve = %q, want %q", got, want)
	}
}

func TestManager_Resolve_WithinShared(t *testing.T) {
	root := t.TempDir()
	m := New("agent-1", root)
	_ = m.EnsureDirs()

	got, err := m.Resolve(m.SharedWorkspace())
	if err != nil {
		t.Fatalf("Resolve shared: %v", err)
	}
	if got != m.SharedWorkspace() {
		t.Errorf("Resolve shared = %q", got)
	}
}

func TestManager_Resolve_RejectsEscape(t *testing.T) {
	root := t.TempDir()
	m := New("agent-1", root)
	_ = m.EnsureDirs()

	escape := filepath.Join(root, "..", "secret")
	if _, err := m.Resolve(escape); err == nil {
		t.Fatalf("expected escape path %q to be rejected", escape)
	}
}

func TestManager_Resolve_RejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	m := New("agent-1", root)
	_ = m.EnsureDirs()

	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(m.AgentWorkspace(), "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Resolve(filepath.Join(link, "secret.txt")); err == nil {
		t.Fatal("expected symlink escape to be rejected")
	}
}

func TestManager_Resolve_AllowsSharedspaceSymlink(t *testing.T) {
	root := t.TempDir()
	m := New("agent-1", root)
	_ = m.EnsureDirs()

	link := filepath.Join(m.AgentWorkspace(), "sharedspace")
	if err := os.Symlink(m.SharedWorkspace(), link); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Resolve(filepath.Join(link, "file.txt")); err != nil {
		t.Fatalf("expected sharedspace symlink path allowed: %v", err)
	}
}

func TestManager_Resolve_EmptyPath(t *testing.T) {
	root := t.TempDir()
	m := New("agent-1", root)
	got, err := m.Resolve("")
	if err != nil {
		t.Fatalf("Resolve empty: %v", err)
	}
	if got != m.AgentWorkspace() {
		t.Errorf("Resolve empty = %q, want agent workspace", got)
	}
}
