package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
)

// Template ownership of the agent context files.
//
// The image template ships a manifest (template/.template.json) recording the
// sha256 of every file it owns plus the hashes of its previous versions, and each
// agent home keeps agent/.home-state.json recording what the manifest last
// applied. Together they make a template update reach existing homes without ever
// clobbering a user edit: a file is replaced only when its content still matches
// something the platform wrote (this template, or an older one).
//
// Runtime data is never reconciled. sessions/ and media/ are not in the template,
// and memory/MEMORY.md is seeded once and then belongs to the agent — a stale
// template copy of it must never overwrite what the agent has learned.
const (
	templateManifestName = ".template.json"
	homeStateName        = ".home-state.json"
)

// TemplateManifest is the template's record of the files it owns.
type TemplateManifest struct {
	Version int `json:"version"`
	// Files maps a template-relative path to the sha256 of its current content.
	Files map[string]string `json:"files"`
	// Previous maps a template-relative path to the hashes of earlier versions,
	// so a home created before the manifest existed can still be recognised as
	// unmodified.
	Previous map[string][]string `json:"previous,omitempty"`
}

// HomeState is what an agent home recorded about the last reconciliation.
type HomeState struct {
	TemplateVersion int `json:"template_version"`
	// Applied maps a template-relative path to the hash written at the time.
	Applied map[string]string `json:"applied"`
}

// ReconcileAction is what happened to one template-owned file.
type ReconcileAction string

const (
	// ReconcileCopied means the file was absent and has been created.
	ReconcileCopied ReconcileAction = "copied"
	// ReconcileUpdated means the file still held platform content and has been
	// refreshed to the current template.
	ReconcileUpdated ReconcileAction = "updated"
	// ReconcileRecorded means the file already matched the current template, and
	// its baseline was captured so future updates can replace it.
	ReconcileRecorded ReconcileAction = "recorded"
	// ReconcileKeptEdited means the file was edited and has been left alone.
	ReconcileKeptEdited ReconcileAction = "kept-edited"
)

// ReconcileResult reports one file's outcome.
type ReconcileResult struct {
	Path   string
	Action ReconcileAction
}

// LoadTemplateManifest reads the template manifest, returning nil when the
// template ships none (an older image), which callers treat as "seed missing
// files only".
func LoadTemplateManifest(templateDir string) (*TemplateManifest, error) {
	data, err := os.ReadFile(filepath.Join(templateDir, templateManifestName))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read template manifest: %w", err)
	}
	var m TemplateManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse template manifest: %w", err)
	}
	return &m, nil
}

// LoadHomeState reads an agent home's reconcile state (an empty one when it has
// never been reconciled).
func LoadHomeState(agentDir string) *HomeState {
	state := &HomeState{Applied: map[string]string{}}
	data, err := os.ReadFile(filepath.Join(agentDir, homeStateName))
	if err != nil {
		return state
	}
	var loaded HomeState
	if json.Unmarshal(data, &loaded) != nil {
		return state
	}
	if loaded.Applied == nil {
		loaded.Applied = map[string]string{}
	}
	return &loaded
}

func saveHomeState(agentDir string, state *HomeState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(agentDir, homeStateName), append(data, '\n'), 0o644)
}

// ReconcileTemplate applies the manifest to one agent home and returns what
// happened to each owned file. With dryRun nothing is written; with force even
// an edited file is replaced.
func ReconcileTemplate(manifest *TemplateManifest, templateDir, agentDir string, force, dryRun bool) ([]ReconcileResult, error) {
	if manifest == nil {
		return nil, nil
	}
	state := LoadHomeState(agentDir)
	results := make([]ReconcileResult, 0, len(manifest.Files))

	paths := make([]string, 0, len(manifest.Files))
	for path := range manifest.Files {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	for _, path := range paths {
		want := manifest.Files[path]
		src := filepath.Join(templateDir, path)
		dst := filepath.Join(agentDir, path)

		got, err := hashFile(dst)
		switch {
		case os.IsNotExist(err):
			if err := installFile(src, dst, dryRun); err != nil {
				return results, err
			}
			results = append(results, ReconcileResult{Path: path, Action: ReconcileCopied})
		case err != nil:
			return results, err
		case got == want:
			results = append(results, ReconcileResult{Path: path, Action: ReconcileRecorded})
		case force || got == state.Applied[path] || hasHash(manifest.Previous[path], got):
			// Platform content the user never touched: refresh it.
			if err := installFile(src, dst, dryRun); err != nil {
				return results, err
			}
			results = append(results, ReconcileResult{Path: path, Action: ReconcileUpdated})
		default:
			results = append(results, ReconcileResult{Path: path, Action: ReconcileKeptEdited})
			continue
		}
		if !dryRun {
			state.Applied[path] = want
		}
	}

	if !dryRun {
		state.TemplateVersion = manifest.Version
		if err := saveHomeState(agentDir, state); err != nil {
			return results, err
		}
	}
	return results, nil
}

// ReconcileHomeTemplate reconciles an agent home against the image template:
// template-owned files follow the manifest rules, every other template file is
// seeded only when missing (it is runtime data owned by the agent). Without a
// manifest it preserves the historical "copy what is missing" behaviour.
func ReconcileHomeTemplate(templateDir, agentDir string) error {
	if templateDir == "" {
		return nil
	}
	manifest, err := LoadTemplateManifest(templateDir)
	if err != nil {
		return err
	}
	if manifest == nil {
		return copyMissingTree(templateDir, agentDir)
	}
	if err := seedUnmanagedTree(templateDir, agentDir, manifest.Files); err != nil {
		return err
	}
	results, err := ReconcileTemplate(manifest, templateDir, agentDir, false, false)
	if err != nil {
		return err
	}
	for _, r := range results {
		switch r.Action {
		case ReconcileUpdated:
			slog.Info("agent home: template file updated", "home", agentDir, "file", r.Path)
		case ReconcileKeptEdited:
			// Drift is reported, never fixed: the file is the user's.
			slog.Info("agent home: template file edited, keeping local version",
				"home", agentDir, "file", r.Path, "template_version", manifest.Version)
		}
	}
	return nil
}

// seedUnmanagedTree copies template files the manifest does not own (runtime
// data such as memory/MEMORY.md), skipping anything that already exists.
func seedUnmanagedTree(src, dst string, managed map[string]string) error {
	return seedUnmanaged(src, dst, "", managed)
}

func seedUnmanaged(src, dst, prefix string, managed map[string]string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return nil // template missing — nothing to seed
	}
	for _, e := range entries {
		rel := filepath.Join(prefix, e.Name())
		if e.IsDir() {
			if err := seedUnmanaged(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()), rel, managed); err != nil {
				return err
			}
			continue
		}
		if _, owned := managed[rel]; owned {
			continue // the manifest reconciles this one
		}
		target := filepath.Join(dst, e.Name())
		if _, err := os.Lstat(target); err == nil {
			continue // exists — keep the current version
		}
		if err := installFile(filepath.Join(src, e.Name()), target, false); err != nil {
			return err
		}
	}
	return nil
}

// installFile copies src over dst (no-op in dry-run), creating parent dirs.
func installFile(src, dst string, dryRun bool) error {
	if dryRun {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return copyFile(src, dst)
}

// hashFile returns the sha256 of a file, or the os.IsNotExist error.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func hasHash(hashes []string, want string) bool {
	for _, h := range hashes {
		if h == want {
			return true
		}
	}
	return false
}
