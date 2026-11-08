package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PathsConfig holds filesystem locations used by the agent runtime.
type PathsConfig struct {
	DataRoot     string `json:"dataRoot"     mapstructure:"dataRoot"`
	SessionDir   string `json:"sessionDir"   mapstructure:"sessionDir"`
	CronFile     string `json:"cronFile"     mapstructure:"cronFile"`
	MediaBaseDir string `json:"mediaBaseDir" mapstructure:"mediaBaseDir"`
	WorkspaceDir string `json:"workspaceDir" mapstructure:"workspaceDir"`
	IdentityFile string `json:"identityFile" mapstructure:"identityFile"`
	BootstrapDir string `json:"bootstrapDir" mapstructure:"bootstrapDir"`
	MemoryDir    string `json:"memoryDir"    mapstructure:"memoryDir"`
	SkillsDir    string `json:"skillsDir"    mapstructure:"skillsDir"`
}

// ResolvedPaths holds absolute filesystem locations.
type ResolvedPaths struct {
	DataRoot     string
	SessionDir   string
	CronFile     string
	MediaBaseDir string
	WorkspaceDir string
	IdentityFile string
	BootstrapDir string
	MemoryDir    string
	SkillsDir    string
}

// DefaultPathsConfig returns the default path configuration. The workspace-root
// defaults point at the image template (/nanosec) and act as the "legacy"
// values that per-agent derivation treats as unset.
func DefaultPathsConfig() PathsConfig {
	return PathsConfig{
		DataRoot:     DefaultDataRoot,
		SessionDir:   DefaultDataRoot + "/sessions",
		CronFile:     DefaultDataRoot + "/cron.json",
		MediaBaseDir: DefaultDataRoot + "/media",
		WorkspaceDir: DefaultTemplateDir,
		IdentityFile: DefaultTemplateDir + "/AGENT.md",
		BootstrapDir: DefaultTemplateDir + "/bootstrap",
		MemoryDir:    DefaultTemplateDir + "/memory",
		SkillsDir:    DefaultTemplateDir + "/skills",
	}
}

// Resolve normalizes all configured paths to absolute paths and fills in any
// omitted derived locations from the data/workspace roots.
func (p PathsConfig) Resolve() (ResolvedPaths, error) {
	dataRoot, err := resolvePath(valueOrDefault(p.DataRoot, DefaultDataRoot))
	if err != nil {
		return ResolvedPaths{}, fmt.Errorf("resolve data root: %w", err)
	}

	workspaceDir, err := resolvePath(valueOrDefault(p.WorkspaceDir, DefaultTemplateDir))
	if err != nil {
		return ResolvedPaths{}, fmt.Errorf("resolve workspace dir: %w", err)
	}

	sessionDir, err := resolvePath(valueOrDefault(p.SessionDir, filepath.Join(dataRoot, "sessions")))
	if err != nil {
		return ResolvedPaths{}, fmt.Errorf("resolve session dir: %w", err)
	}

	cronFile, err := resolvePath(valueOrDefault(p.CronFile, filepath.Join(dataRoot, "cron.json")))
	if err != nil {
		return ResolvedPaths{}, fmt.Errorf("resolve cron file: %w", err)
	}

	mediaBaseDir, err := resolvePath(valueOrDefault(p.MediaBaseDir, filepath.Join(dataRoot, "media")))
	if err != nil {
		return ResolvedPaths{}, fmt.Errorf("resolve media base dir: %w", err)
	}

	identityFile, err := resolvePath(valueOrDefault(p.IdentityFile, filepath.Join(workspaceDir, "AGENT.md")))
	if err != nil {
		return ResolvedPaths{}, fmt.Errorf("resolve identity file: %w", err)
	}

	bootstrapDir, err := resolvePath(valueOrDefault(p.BootstrapDir, filepath.Join(workspaceDir, "bootstrap")))
	if err != nil {
		return ResolvedPaths{}, fmt.Errorf("resolve bootstrap dir: %w", err)
	}

	memoryDir, err := resolvePath(valueOrDefault(p.MemoryDir, filepath.Join(workspaceDir, "memory")))
	if err != nil {
		return ResolvedPaths{}, fmt.Errorf("resolve memory dir: %w", err)
	}

	skillsDir, err := resolvePath(valueOrDefault(p.SkillsDir, filepath.Join(workspaceDir, "skills")))
	if err != nil {
		return ResolvedPaths{}, fmt.Errorf("resolve skills dir: %w", err)
	}

	return ResolvedPaths{
		DataRoot:     dataRoot,
		SessionDir:   sessionDir,
		CronFile:     cronFile,
		MediaBaseDir: mediaBaseDir,
		WorkspaceDir: workspaceDir,
		IdentityFile: identityFile,
		BootstrapDir: bootstrapDir,
		MemoryDir:    memoryDir,
		SkillsDir:    skillsDir,
	}, nil
}

func valueOrDefault(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

// AgentRoot returns the agent home root: <dataRoot>/agents/<homeName>.
func (r ResolvedPaths) AgentRoot(homeName string) string {
	return filepath.Join(r.DataRoot, "agents", homeName)
}

// ResolveAgentPaths resolves the effective filesystem paths for a single agent.
// homeName is the agent's assigned home folder name (see workspace package).
// Per-agent defaults live under agents/<homeName>/agent (identity, bootstrap, memory,
// skills, sessions, cron, media) and agents/<homeName>/workspace (working files), with a
// shared space at <dataRoot>/sharedspace. Values in p override the derived defaults,
// except when they are empty or still carry the legacy global defaults that previously
// pointed at the image's template.
func ResolveAgentPaths(homeName string, p PathsConfig) (ResolvedPaths, error) {
	dataRoot, err := resolvePath(valueOrDefault(p.DataRoot, DefaultDataRoot))
	if err != nil {
		return ResolvedPaths{}, fmt.Errorf("resolve data root: %w", err)
	}
	home := filepath.Join(dataRoot, "agents", homeName)

	legacy := DefaultPathsConfig()
	override := func(value, fallback, legacyDefault string) (string, error) {
		if value == "" || value == legacyDefault {
			value = fallback
		}
		return resolvePath(value)
	}

	sessionDir, err := override(p.SessionDir, filepath.Join(home, "agent", "sessions"), legacy.SessionDir)
	if err != nil {
		return ResolvedPaths{}, fmt.Errorf("resolve session dir: %w", err)
	}

	cronFile, err := override(p.CronFile, filepath.Join(home, "agent", "cron.json"), legacy.CronFile)
	if err != nil {
		return ResolvedPaths{}, fmt.Errorf("resolve cron file: %w", err)
	}

	mediaBaseDir, err := override(p.MediaBaseDir, filepath.Join(home, "agent", "media"), legacy.MediaBaseDir)
	if err != nil {
		return ResolvedPaths{}, fmt.Errorf("resolve media base dir: %w", err)
	}

	workspaceDir, err := override(p.WorkspaceDir, filepath.Join(home, "workspace"), legacy.WorkspaceDir)
	if err != nil {
		return ResolvedPaths{}, fmt.Errorf("resolve workspace dir: %w", err)
	}

	identityFile, err := override(p.IdentityFile, filepath.Join(home, "agent", "AGENT.md"), legacy.IdentityFile)
	if err != nil {
		return ResolvedPaths{}, fmt.Errorf("resolve identity file: %w", err)
	}

	bootstrapDir, err := override(p.BootstrapDir, filepath.Join(home, "agent", "bootstrap"), legacy.BootstrapDir)
	if err != nil {
		return ResolvedPaths{}, fmt.Errorf("resolve bootstrap dir: %w", err)
	}

	memoryDir, err := override(p.MemoryDir, filepath.Join(home, "agent", "memory"), legacy.MemoryDir)
	if err != nil {
		return ResolvedPaths{}, fmt.Errorf("resolve memory dir: %w", err)
	}

	skillsDir, err := override(p.SkillsDir, filepath.Join(home, "agent", "skills"), legacy.SkillsDir)
	if err != nil {
		return ResolvedPaths{}, fmt.Errorf("resolve skills dir: %w", err)
	}

	return ResolvedPaths{
		DataRoot:     dataRoot,
		SessionDir:   sessionDir,
		CronFile:     cronFile,
		MediaBaseDir: mediaBaseDir,
		WorkspaceDir: workspaceDir,
		IdentityFile: identityFile,
		BootstrapDir: bootstrapDir,
		MemoryDir:    memoryDir,
		SkillsDir:    skillsDir,
	}, nil
}

func resolvePath(path string) (string, error) {
	if path == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir: %w", err)
		}
		return home, nil
	}
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir: %w", err)
		}
		path = filepath.Join(home, path[2:])
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return abs, nil
}
