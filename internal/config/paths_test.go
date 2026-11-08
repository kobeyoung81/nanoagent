package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultPathsConfig(t *testing.T) {
	paths := DefaultPathsConfig()

	if paths.DataRoot != DefaultDataRoot {
		t.Errorf("DataRoot = %q, want %q", paths.DataRoot, DefaultDataRoot)
	}
	if paths.WorkspaceDir != DefaultTemplateDir {
		t.Errorf("WorkspaceDir = %q, want %q", paths.WorkspaceDir, DefaultTemplateDir)
	}
	if paths.IdentityFile != DefaultTemplateDir+"/AGENT.md" {
		t.Errorf("IdentityFile = %q", paths.IdentityFile)
	}
}

func TestPathsResolve_Defaults(t *testing.T) {
	resolved, err := DefaultPathsConfig().Resolve()
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	if resolved.DataRoot == "" || resolved.SessionDir == "" || resolved.WorkspaceDir == "" {
		t.Fatal("expected resolved defaults to be populated")
	}
	if got, want := resolved.CronFile, filepath.Join(resolved.DataRoot, "cron.json"); got != want {
		t.Errorf("CronFile = %q, want %q", got, want)
	}
	if got, want := resolved.MemoryDir, filepath.Join(resolved.WorkspaceDir, "memory"); got != want {
		t.Errorf("MemoryDir = %q, want %q", got, want)
	}
}

func TestPathsResolve_Overrides(t *testing.T) {
	cfg := PathsConfig{
		DataRoot:     "/data/nanosec",
		SessionDir:   "/state/sessions",
		CronFile:     "/state/cron/jobs.json",
		MediaBaseDir: "/var/lib/nanosec/media",
		WorkspaceDir: "/workspace/custom",
		IdentityFile: "/workspace/custom/AGENT.md",
		BootstrapDir: "/workspace/custom/bootstrap",
		MemoryDir:    "/workspace/custom/memory",
		SkillsDir:    "/workspace/custom/skills",
	}

	resolved, err := cfg.Resolve()
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	if got, want := resolved.SessionDir, "/state/sessions"; got != want {
		t.Errorf("SessionDir = %q, want %q", got, want)
	}
	if got, want := resolved.SkillsDir, "/workspace/custom/skills"; got != want {
		t.Errorf("SkillsDir = %q, want %q", got, want)
	}
}

func TestResolveAgentPaths_DerivesHomeLayout(t *testing.T) {
	root := t.TempDir()
	resolved, err := ResolveAgentPaths("agent-1", PathsConfig{DataRoot: root})
	if err != nil {
		t.Fatalf("ResolveAgentPaths() error = %v", err)
	}

	home := filepath.Join(root, "agents", "agent-1")
	want := map[string]string{
		"IdentityFile": filepath.Join(home, "agent", "AGENT.md"),
		"BootstrapDir": filepath.Join(home, "agent", "bootstrap"),
		"MemoryDir":    filepath.Join(home, "agent", "memory"),
		"SkillsDir":    filepath.Join(home, "agent", "skills"),
		"SessionDir":   filepath.Join(home, "agent", "sessions"),
		"CronFile":     filepath.Join(home, "agent", "cron.json"),
		"MediaBaseDir": filepath.Join(home, "agent", "media"),
		"WorkspaceDir": filepath.Join(home, "workspace"),
	}
	got := map[string]string{
		"IdentityFile": resolved.IdentityFile,
		"BootstrapDir": resolved.BootstrapDir,
		"MemoryDir":    resolved.MemoryDir,
		"SkillsDir":    resolved.SkillsDir,
		"SessionDir":   resolved.SessionDir,
		"CronFile":     resolved.CronFile,
		"MediaBaseDir": resolved.MediaBaseDir,
		"WorkspaceDir": resolved.WorkspaceDir,
	}
	for field, expected := range want {
		if got[field] != expected {
			t.Errorf("%s = %q, want %q", field, got[field], expected)
		}
	}
}

func TestResolveAgentPaths_LegacyDefaultsIgnored(t *testing.T) {
	root := t.TempDir()
	// The old global seeds pointed at the image's template (shared for every
	// agent); those must be treated as "derive" for per-agent homes.
	legacy := DefaultPathsConfig()
	resolved, err := ResolveAgentPaths("agent-1", PathsConfig{
		DataRoot:     root,
		WorkspaceDir: legacy.WorkspaceDir,
		IdentityFile: legacy.IdentityFile,
		BootstrapDir: legacy.BootstrapDir,
		MemoryDir:    legacy.MemoryDir,
		SkillsDir:    legacy.SkillsDir,
		SessionDir:   legacy.SessionDir,
		CronFile:     legacy.CronFile,
		MediaBaseDir: legacy.MediaBaseDir,
	})
	if err != nil {
		t.Fatalf("ResolveAgentPaths() error = %v", err)
	}

	home := filepath.Join(root, "agents", "agent-1")
	if got, want := resolved.IdentityFile, filepath.Join(home, "agent", "AGENT.md"); got != want {
		t.Errorf("IdentityFile = %q, want %q (legacy default should be ignored)", got, want)
	}
	if got, want := resolved.SessionDir, filepath.Join(home, "agent", "sessions"); got != want {
		t.Errorf("SessionDir = %q, want %q (legacy default should be ignored)", got, want)
	}
	if got, want := resolved.WorkspaceDir, filepath.Join(home, "workspace"); got != want {
		t.Errorf("WorkspaceDir = %q, want %q (legacy default should be ignored)", got, want)
	}
}

func TestResolveAgentPaths_Overrides(t *testing.T) {
	root := t.TempDir()
	resolved, err := ResolveAgentPaths("agent-1", PathsConfig{
		DataRoot:     root,
		WorkspaceDir: filepath.Join(root, "custom-ws"),
		MemoryDir:    filepath.Join(root, "custom-mem"),
	})
	if err != nil {
		t.Fatalf("ResolveAgentPaths() error = %v", err)
	}

	if got, want := resolved.WorkspaceDir, filepath.Join(root, "custom-ws"); got != want {
		t.Errorf("WorkspaceDir = %q, want %q", got, want)
	}
	if got, want := resolved.MemoryDir, filepath.Join(root, "custom-mem"); got != want {
		t.Errorf("MemoryDir = %q, want %q", got, want)
	}
	// Non-overridden paths still derive from the agent home.
	home := filepath.Join(root, "agents", "agent-1")
	if got, want := resolved.IdentityFile, filepath.Join(home, "agent", "AGENT.md"); got != want {
		t.Errorf("IdentityFile = %q, want %q", got, want)
	}
}

func TestResolvePath_BareTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"~", "~/"} {
		got, err := resolvePath(p)
		if err != nil {
			t.Fatalf("resolvePath(%q) error = %v", p, err)
		}
		if got != home {
			t.Errorf("resolvePath(%q) = %q, want %q", p, got, home)
		}
	}
}
