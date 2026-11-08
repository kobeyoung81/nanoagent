package agent

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gitee.com/kobeyoung81/nanosec/internal/config"
	"gitee.com/kobeyoung81/nanosec/internal/workspace"
)

// Default placeholder content seeded into a fresh agent home when the image
// template is unavailable. These files are user/agent editable and are never
// overwritten once created.
const (
	// placeholderAGENT must stay identical to template/AGENT.md — the image
	// template seeds agent homes and this is the fallback when it is missing.
	// A test in this package compares the two.
	placeholderAGENT = "# AGENT\n\nThis file is this agent's identity: who it is, what domain it works in, and any standing instructions the user has given it. Edit it to change that.\n\nEverything else about how you work \u2014 how your context is assembled, where your files live, tool etiquette, memory policy and safety \u2014 is platform behaviour and is stated in your mode's instructions, not here. Durable user preferences go in `bootstrap/USER.md`; facts worth keeping go in `memory/`.\n\n## Identity\n\n*(Describe this agent: its role, its domain, its tone, the standing instructions it should follow. Keep it short.)*\n"
	placeholderSOUL  = `# Collaboration Style

Write down the collaboration norms and working style you should follow here.
`
	placeholderUSER = `# User Context

Record durable user preferences, recurring constraints, and communication norms
here when they become stable enough to keep across sessions.
`
	placeholderTOOLS = `# Tool Notes

Platform defaults for the tools you have — calls in one message run one after
another unless they are wrapped in parallel_execution, and read hands back one
window at a time — ship with the image's template and replace this file on the
first reconcile.

Record what you learn about your own tools here.
`
	placeholderMEMORY = `# MEMORY

Keep durable facts, stable user preferences, and long-lived project context here.
`
)

// agentIDMarker is a small file written into each agent home identifying its
// owner; it backs collision detection for display-name-derived folder names.
const agentIDMarker = ".agent-id"

// EnsureAgentHome initializes a per-agent home under the persistent data root:
//
//	<dataRoot>/agents/<homeName>/agent      identity, bootstrap, memory, skills, sessions, cron, media
//	<dataRoot>/agents/<homeName>/workspace   working files
//	<dataRoot>/agents/<homeName>/sharedspace -> <dataRoot>/sharedspace  (symlink)
//
// <homeName> is derived from the agent's display name and FROZEN at creation
// (later display-name renames do not move the folder); the mapping is kept in
// <dataRoot>/agents/.homes.json. Legacy folders named by agent id are adopted
// (renamed) on first sight so pre-existing volumes keep their files.
// The image template is copied in for files that don't already exist, so each
// agent starts fully initialized while user edits and agent-managed files are
// preserved. It returns the effective (assigned) home folder name.
func EnsureAgentHome(dataRoot, agentID, displayName string) (string, error) {
	return EnsureAgentHomeFrom(dataRoot, agentID, displayName, config.DefaultTemplateDir)
}

// EnsureAgentHomeFrom is EnsureAgentHome with an explicit template directory
// (used by tests).
func EnsureAgentHomeFrom(dataRoot, agentID, displayName, templateDir string) (string, error) {
	homeName, err := assignHomeName(dataRoot, agentID, displayName)
	if err != nil {
		return "", err
	}

	home := filepath.Join(dataRoot, "agents", homeName)
	agentDir := filepath.Join(home, "agent")

	// Migrate a legacy shared/ directory to the sharedspace name before
	// creating an empty sharedspace.
	legacyShared := filepath.Join(dataRoot, "shared")
	if _, err := os.Stat(legacyShared); err == nil {
		if _, err := os.Stat(filepath.Join(dataRoot, "sharedspace")); os.IsNotExist(err) {
			if err := os.Rename(legacyShared, filepath.Join(dataRoot, "sharedspace")); err != nil {
				return "", fmt.Errorf("migrate shared dir: %w", err)
			}
		}
	}

	for _, dir := range []string{
		filepath.Join(agentDir, "bootstrap"),
		filepath.Join(agentDir, "memory"),
		filepath.Join(agentDir, "skills"),
		filepath.Join(agentDir, "sessions"),
		filepath.Join(agentDir, "media"),
		filepath.Join(home, "workspace"),
		filepath.Join(dataRoot, "sharedspace"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("create agent home dir %s: %w", dir, err)
		}
	}

	// Seed from the image template: template-owned files follow the manifest
	// (replaced only while unedited), the rest is created when missing.
	if templateDir != "" {
		if err := ReconcileHomeTemplate(templateDir, agentDir); err != nil {
			return "", fmt.Errorf("seed agent home from template: %w", err)
		}
	}

	// Placeholders only for context files the template did not provide.
	placeholders := []struct {
		path    string
		content string
	}{
		{filepath.Join(agentDir, "AGENT.md"), placeholderAGENT},
		{filepath.Join(agentDir, "bootstrap", "SOUL.md"), placeholderSOUL},
		{filepath.Join(agentDir, "bootstrap", "USER.md"), placeholderUSER},
		{filepath.Join(agentDir, "bootstrap", "TOOLS.md"), placeholderTOOLS},
		{filepath.Join(agentDir, "memory", "MEMORY.md"), placeholderMEMORY},
	}
	for _, ph := range placeholders {
		if _, err := os.Stat(ph.path); os.IsNotExist(err) {
			if err := os.WriteFile(ph.path, []byte(ph.content), 0o644); err != nil {
				return "", fmt.Errorf("seed %s: %w", ph.path, err)
			}
		}
	}

	// Expose the shared space inside the home as a symlink.
	link := filepath.Join(home, "sharedspace")
	if _, err := os.Lstat(link); os.IsNotExist(err) {
		if err := os.Symlink(filepath.Join(dataRoot, "sharedspace"), link); err != nil {
			return "", fmt.Errorf("create sharedspace symlink: %w", err)
		}
	}

	// Record ownership so resolver lookups and collision checks are stable.
	marker := filepath.Join(agentDir, agentIDMarker)
	if _, err := os.Stat(marker); os.IsNotExist(err) {
		if err := os.WriteFile(marker, []byte(agentID), 0o644); err != nil {
			return "", fmt.Errorf("write agent id marker: %w", err)
		}
	}

	return homeName, nil
}

// assignHomeName returns the frozen home folder name for an agent, adopting a
// legacy agent-id-named folder or a pure display-name folder, and registering
// the mapping. It is the only place that creates/renames an agent home folder.
func assignHomeName(dataRoot, agentID, displayName string) (string, error) {
	agentsDir := filepath.Join(dataRoot, "agents")
	index := workspace.NewHomesIndex(dataRoot)

	// Already assigned (frozen): reuse; never follow display-name renames.
	if name, ok := workspace.AssignedHome(index, agentID); ok {
		return name, nil
	}

	desired := workspace.AgentHomeName(agentID, displayName)
	legacyHome := filepath.Join(agentsDir, agentID)
	freshHome := filepath.Join(agentsDir, desired)

	// Legacy adoption: the folder was named by the raw agent id.
	if agentID != desired {
		if _, err := os.Stat(legacyHome); err == nil {
			// Never clobber an existing (other) display-name folder.
			if _, err := os.Stat(freshHome); os.IsNotExist(err) {
				if err := os.Rename(legacyHome, freshHome); err != nil {
					return "", fmt.Errorf("adopt legacy home %s -> %s: %w", legacyHome, freshHome, err)
				}
			}
		}
	}

	// Collision: another agent already claims the desired name -> suffix.
	if owner, taken := index.OwnedBy(desired); taken && owner != agentID {
		desired = desired + "-" + workspace.ShortID(agentID)
	} else if _, err := os.Stat(freshHome); err == nil {
		if owner, _ := homeOwner(freshHome); owner != "" && owner != agentID {
			desired = desired + "-" + workspace.ShortID(agentID)
			freshHome = filepath.Join(agentsDir, desired)
		}
	}

	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		return "", fmt.Errorf("create agents dir: %w", err)
	}
	if err := index.Register(agentID, desired); err != nil {
		return "", err
	}
	return desired, nil
}

// homeOwner reads the marker file of an existing agent home, if any.
func homeOwner(homeDir string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(homeDir, "agent", agentIDMarker))
	if err != nil {
		return "", false
	}
	owner := string(data)
	return owner, owner != ""
}

// copyMissingTree copies src into dst, skipping any file or directory that
// already exists at dst (so user edits and agent-managed files are preserved).
func copyMissingTree(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return nil // template missing — nothing to seed
	}
	for _, e := range entries {
		if err := copyMissing(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

func copyMissing(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	if !info.IsDir() {
		if _, err := os.Lstat(dst); err == nil {
			return nil // exists — keep the current version
		}
		return copyFile(src, dst)
	}
	if err := os.MkdirAll(dst, info.Mode().Perm()); err != nil {
		return err
	}
	children, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, c := range children {
		if err := copyMissing(filepath.Join(src, c.Name()), filepath.Join(dst, c.Name())); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}
