package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureAgentHome_CreatesLayout(t *testing.T) {
	root := t.TempDir()
	homeName, err := EnsureAgentHome(root, "agent-1", "")
	if err != nil {
		t.Fatalf("EnsureAgentHome: %v", err)
	}
	if homeName != "agent-1" {
		t.Fatalf("homeName = %q, want agent-1", homeName)
	}

	home := filepath.Join(root, "agents", "agent-1")
	for _, dir := range []string{
		filepath.Join(home, "agent", "bootstrap"),
		filepath.Join(home, "agent", "memory"),
		filepath.Join(home, "agent", "skills"),
		filepath.Join(home, "agent", "sessions"),
		filepath.Join(home, "agent", "media"),
		filepath.Join(home, "workspace"),
		filepath.Join(root, "sharedspace"),
	} {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			t.Errorf("expected dir %q: %v", dir, err)
		}
	}

	for _, f := range []string{
		filepath.Join(home, "agent", "AGENT.md"),
		filepath.Join(home, "agent", "bootstrap", "SOUL.md"),
		filepath.Join(home, "agent", "bootstrap", "USER.md"),
		filepath.Join(home, "agent", "bootstrap", "TOOLS.md"),
		filepath.Join(home, "agent", "memory", "MEMORY.md"),
	} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("expected placeholder %q: %v", f, err)
		}
	}

	link := filepath.Join(home, "sharedspace")
	info, err := os.Lstat(link)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("expected sharedspace symlink at %q: %v", link, err)
	}
	if target, err := os.Readlink(link); err != nil || target != filepath.Join(root, "sharedspace") {
		t.Errorf("sharedspace symlink target = %q, want %q", target, filepath.Join(root, "sharedspace"))
	}
}

func TestEnsureAgentHome_Idempotent_DoesNotOverwrite(t *testing.T) {
	root := t.TempDir()
	if _, err := EnsureAgentHome(root, "agent-1", ""); err != nil {
		t.Fatal(err)
	}

	agentPath := filepath.Join(root, "agents", "agent-1", "agent", "AGENT.md")
	custom := "# CUSTOM IDENTITY"
	if err := os.WriteFile(agentPath, []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := EnsureAgentHome(root, "agent-1", ""); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(agentPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != custom {
		t.Errorf("EnsureAgentHome overwrote existing AGENT.md: %q", data)
	}
}

func TestEnsureAgentHome_MigratesLegacyShared(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "shared"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "shared", "keep.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := EnsureAgentHome(root, "agent-1", ""); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(root, "sharedspace", "keep.txt")); err != nil {
		t.Errorf("expected legacy shared content migrated to sharedspace: %v", err)
	}
}

func TestEnsureAgentHome_SeedsFromTemplate(t *testing.T) {
	root := t.TempDir()
	tmpl := filepath.Join(root, "template")
	writeSkill := func(name string) {
		dir := filepath.Join(tmpl, "skills", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("# "+name+" skill"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(tmpl, "bootstrap"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmpl, "memory"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpl, "AGENT.md"), []byte("FULL AGENT IDENTITY"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpl, "bootstrap", "SOUL.md"), []byte("soul"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpl, "memory", "MEMORY.md"), []byte("memory"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSkill("mdtopdf")
	writeSkill("officecli")

	if _, err := EnsureAgentHomeFrom(root, "agent-1", "", tmpl); err != nil {
		t.Fatal(err)
	}

	agentDir := filepath.Join(root, "agents", "agent-1", "agent")
	for _, f := range []string{
		filepath.Join(agentDir, "AGENT.md"),
		filepath.Join(agentDir, "bootstrap", "SOUL.md"),
		filepath.Join(agentDir, "memory", "MEMORY.md"),
		filepath.Join(agentDir, "skills", "mdtopdf", "SKILL.md"),
		filepath.Join(agentDir, "skills", "officecli", "SKILL.md"),
	} {
		if data, err := os.ReadFile(f); err != nil || len(data) == 0 {
			t.Errorf("expected template file seeded at %q: %v", f, err)
		}
	}
	if data, err := os.ReadFile(filepath.Join(agentDir, "AGENT.md")); err != nil || string(data) != "FULL AGENT IDENTITY" {
		t.Errorf("expected full AGENT.md seeded, got %q (err %v)", data, err)
	}
}

func TestEnsureAgentHome_TemplateSeedDoesNotOverwrite(t *testing.T) {
	root := t.TempDir()
	tmpl := filepath.Join(root, "template")
	if err := os.MkdirAll(tmpl, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpl, "AGENT.md"), []byte("TEMPLATE"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := EnsureAgentHomeFrom(root, "agent-1", "", tmpl); err != nil {
		t.Fatal(err)
	}

	agentDir := filepath.Join(root, "agents", "agent-1", "agent")
	agentPath := filepath.Join(agentDir, "AGENT.md")
	custom := "# CUSTOM"
	if err := os.WriteFile(agentPath, []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := EnsureAgentHomeFrom(root, "agent-1", "", tmpl); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(agentPath)
	if err != nil || string(data) != custom {
		t.Errorf("template seed overwrote user AGENT.md: %q (err %v)", data, err)
	}
}

func TestEnsureAgentHome_DisplayNameFolder(t *testing.T) {
	root := t.TempDir()
	name, err := EnsureAgentHome(root, "a1", "My Agent")
	if err != nil {
		t.Fatal(err)
	}
	if name != "my-agent" {
		t.Fatalf("homeName = %q, want my-agent", name)
	}
	if _, err := os.Stat(filepath.Join(root, "agents", "my-agent", "agent", "AGENT.md")); err != nil {
		t.Errorf("expected display-name home seeded: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "agents", "a1")); !os.IsNotExist(err) {
		t.Errorf("agent-id folder should not exist (adopted/renamed): %v", err)
	}
}

func TestEnsureAgentHome_LegacyAdoptionKeepsFiles(t *testing.T) {
	root := t.TempDir()
	legacy := filepath.Join(root, "agents", "a1", "workspace")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "note.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	name, err := EnsureAgentHome(root, "a1", "My Agent")
	if err != nil {
		t.Fatal(err)
	}
	if name != "my-agent" {
		t.Fatalf("homeName = %q, want my-agent", name)
	}
	if data, err := os.ReadFile(filepath.Join(root, "agents", "my-agent", "workspace", "note.txt")); err != nil || string(data) != "keep" {
		t.Errorf("legacy home content not preserved: %q (err %v)", data, err)
	}
}

func TestEnsureAgentHome_FrozenOnRename(t *testing.T) {
	root := t.TempDir()
	name1, err := EnsureAgentHome(root, "a1", "My Agent")
	if err != nil {
		t.Fatal(err)
	}
	name2, err := EnsureAgentHome(root, "a1", "Renamed Agent")
	if err != nil {
		t.Fatal(err)
	}
	if name1 != name2 || name1 != "my-agent" {
		t.Errorf("home name should be frozen at creation, got %q then %q", name1, name2)
	}
}

func TestEnsureAgentHome_CollisionSuffix(t *testing.T) {
	root := t.TempDir()
	name1, err := EnsureAgentHome(root, "a1", "My Agent")
	if err != nil {
		t.Fatal(err)
	}
	name2, err := EnsureAgentHome(root, "a2", "my agent")
	if err != nil {
		t.Fatal(err)
	}
	if name1 != "my-agent" {
		t.Fatalf("first home = %q, want my-agent", name1)
	}
	if name2 == "my-agent" || !strings.HasPrefix(name2, "my-agent-") {
		t.Errorf("collision should suffix, got %q", name2)
	}
}
