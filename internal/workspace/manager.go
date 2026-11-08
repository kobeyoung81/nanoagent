// Package workspace provides per-agent isolated workspaces plus a shared
// workspace, with path-resolution guards so tools cannot escape their roots.
// It also owns the on-disk mapping of agent -> home folder name (the folder is
// named after the agent's display name, frozen at creation).
package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gitee.com/kobeyoung81/nanoprotocol"
)

// homesFileName is the registry mapping agentID -> assigned home folder name
// under <dataRoot>/agents/. It is the single source of truth for resolvers
// (file browsing, terminals) so they always agree with the folder the seeder
// assigned.
const homesFileName = ".homes.json"

// maxHomeNameLen bounds a display-name-derived folder name.
const maxHomeNameLen = 64

// Manager resolves agent-isolated and shared workspace paths under a data root.
type Manager struct {
	AgentID  string
	HomeName string
	RootDir  string
	SharedID string
}

// New creates a Manager rooted at dataRoot whose agent home folder is named by
// agentID (legacy behavior; used by tests). Production should use NewNamed.
func New(agentID, dataRoot string) *Manager {
	return NewNamed(agentID, agentID, dataRoot)
}

// NewNamed creates a Manager with an explicit agent home folder name under
// <dataRoot>/agents.
func NewNamed(agentID, homeName, dataRoot string) *Manager {
	sharedID := "sharedspace"
	return &Manager{AgentID: agentID, HomeName: homeName, RootDir: dataRoot, SharedID: sharedID}
}

// NewWithShared creates a Manager with an explicit shared directory name.
func NewWithShared(agentID, dataRoot, sharedID string) *Manager {
	if sharedID == "" {
		sharedID = "sharedspace"
	}
	return &Manager{AgentID: agentID, HomeName: agentID, RootDir: dataRoot, SharedID: sharedID}
}

// AgentWorkspace returns the agent home root: dataRoot/agents/<homeName>. This
// is the shell cwd, the tool file root, and the portal browse root.
func (m *Manager) AgentWorkspace() string {
	return filepath.Join(m.RootDir, "agents", m.HomeName)
}

// SharedWorkspace returns the shared space path accessible to all agents.
func (m *Manager) SharedWorkspace() string {
	return filepath.Join(m.RootDir, m.SharedID)
}

// AgentHomeName derives the desired home folder name for an agent from its
// display name; when the display name sanitizes empty (e.g. CJK-only) or
// missing, it falls back to the sanitized agent ID.
func AgentHomeName(agentID, displayName string) string {
	if name := nanoprotocol.SanitizeName(displayName, maxHomeNameLen); name != "" {
		return name
	}
	if name := nanoprotocol.SanitizeName(agentID, maxHomeNameLen); name != "" {
		return name
	}
	return "agent-" + shortID(agentID)
}

// shortID returns a stable short identifier for an agent id (8 hex chars).
func shortID(agentID string) string {
	sum := sha256.Sum256([]byte(agentID))
	return hex.EncodeToString(sum[:])[:8]
}

// ShortID is the exported stable short identifier for an agent id.
func ShortID(agentID string) string {
	return shortID(agentID)
}

// AssignedHome returns the registry-assigned home folder name for an agent
// (no pure derive fallback) — used by the seeder to honor frozen names.
func AssignedHome(index *HomesIndex, agentID string) (string, bool) {
	index.mu.Lock()
	defer index.mu.Unlock()
	index.refreshLocked()
	name, ok := index.homes[agentID]
	return name, ok && name != ""
}

// HomesIndex is the persisted agentID -> home folder name registry.
type HomesIndex struct {
	mu       sync.Mutex
	dataRoot string
	filePath string
	loaded   string // mtime at last load
	homes    map[string]string
}

// NewHomesIndex returns an index backed by <dataRoot>/agents/.homes.json.
func NewHomesIndex(dataRoot string) *HomesIndex {
	return &HomesIndex{
		dataRoot: dataRoot,
		filePath: filepath.Join(dataRoot, "agents", homesFileName),
		homes:    map[string]string{},
	}
}

// Lookup returns the assigned home folder name for an agent, refreshing the
// registry from disk as needed. Unregistered agents fall back to a pure
// derive from the display name (the seeder records the real assignment).
func (h *HomesIndex) Lookup(agentID, displayName string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.refreshLocked()
	if name, ok := h.homes[agentID]; ok && name != "" {
		return name
	}
	return AgentHomeName(agentID, displayName)
}

// OwnedBy reports whether a home folder name is already assigned to a
// different agent (registry is the authority).
func (h *HomesIndex) OwnedBy(name string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.refreshLocked()
	for id, n := range h.homes {
		if n == name && id != "" {
			return id, true
		}
	}
	return "", false
}

// Register persists the assignment agentID -> homeName (merge + atomic write).
func (h *HomesIndex) Register(agentID, homeName string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.refreshLocked()
	h.homes[agentID] = homeName
	return h.persistLocked()
}

func (h *HomesIndex) refreshLocked() {
	info, err := os.Stat(h.filePath)
	if err != nil {
		return
	}
	key := info.ModTime().String()
	if key == h.loaded {
		return
	}
	if data, err := os.ReadFile(h.filePath); err == nil {
		var parsed struct {
			Homes map[string]string `json:"homes"`
		}
		if json.Unmarshal(data, &parsed) == nil && parsed.Homes != nil {
			h.homes = parsed.Homes
			h.loaded = key
		}
	}
}

func (h *HomesIndex) persistLocked() error {
	if err := os.MkdirAll(filepath.Dir(h.filePath), 0o755); err != nil {
		return fmt.Errorf("create agents dir: %w", err)
	}
	data, err := json.Marshal(struct {
		Homes map[string]string `json:"homes"`
	}{Homes: h.homes})
	if err != nil {
		return err
	}
	tmp := h.filePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, h.filePath)
}

// EnsureDirs creates the agent and shared workspace directories.
func (m *Manager) EnsureDirs() error {
	for _, dir := range []string{m.AgentWorkspace(), m.SharedWorkspace()} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create workspace %s: %w", dir, err)
		}
	}
	return nil
}

// Resolve returns the absolute path for a path the agent requested, verifying
// it stays within the agent home or the shared space. Relative paths resolve
// against the agent home; an empty path resolves to the agent home root.
// Symlinks are resolved to their canonical location before the containment
// check, so a symlink cannot escape the allowed roots (the deliberate
// sharedspace symlink resolves to the shared space target and is allowed).
func (m *Manager) Resolve(path string) (string, error) {
	if path == "" {
		return m.AgentWorkspace(), nil
	}

	// Resolve relative paths against the agent home root.
	if !filepath.IsAbs(path) {
		path = filepath.Join(m.AgentWorkspace(), path)
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("invalid path %q: %w", path, err)
	}

	canon, err := canonical(absPath)
	if err != nil {
		return "", fmt.Errorf("resolve path %q: %w", path, err)
	}

	for _, root := range []string{m.AgentWorkspace(), m.SharedWorkspace()} {
		canonRoot, err := canonical(root)
		if err != nil {
			continue
		}
		if within(canon, canonRoot) {
			return absPath, nil
		}
	}

	return "", fmt.Errorf("access denied: path %q is outside the agent and shared spaces", path)
}

// IsProtected reports whether the resolved path is a structural root that must
// not be deleted or renamed: the agent home root, the agent grouping directory
// (identity/context/runtime), the shared space root, and the agent-managed
// runtime roots (sessions, cron, media). Symlinks (e.g. the home's sharedspace
// link) are resolved to their canonical location first.
func (m *Manager) IsProtected(resolved string) bool {
	canon := func(p string) string {
		c, err := canonical(p)
		if err != nil {
			return p
		}
		return c
	}
	target := canon(resolved)
	agentDir := filepath.Join(m.AgentWorkspace(), "agent")
	for _, root := range []string{
		m.AgentWorkspace(),
		agentDir,
		filepath.Join(agentDir, "sessions"),
		filepath.Join(agentDir, "cron.json"),
		filepath.Join(agentDir, "media"),
		m.SharedWorkspace(),
	} {
		if target == canon(root) {
			return true
		}
	}
	return false
}

// canonical resolves symlinks for path (which may not exist yet) by resolving
// the nearest existing ancestor and re-appending the remaining suffix.
func canonical(path string) (string, error) {
	current := path
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			rel, rerr := filepath.Rel(current, path)
			if rerr != nil {
				return "", rerr
			}
			return filepath.Join(resolved, rel), nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		current = parent
	}
}

// within reports whether child is the same as or inside parent.
func within(child, parent string) bool {
	if child == parent {
		return true
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
