// Package skillstore materializes catalog skill packages into an agent's
// skills directory. It is owned by the hypervisor (the per-user control plane),
// keeping the agent process a pure consumer of the filesystem.
package skillstore

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gitee.com/kobeyoung81/nanoprotocol"
)

const (
	manifestName = ".capabilities-manifest.json"
	legacyDir    = ".legacy"
	tmpDir       = ".tmp"
)

// Materialize writes the enabled catalog skills into skillsDir and removes
// previously-managed skills that are no longer enabled.
//
// Catalog skill names are authoritative: an existing unmanaged directory that
// collides with an enabled skill is moved to skillsDir/.legacy/ before being
// replaced, so a legacy template copy can never shadow a catalog update while
// its content is still preserved. Unrelated user-authored directories (names
// not present in the catalog) are left untouched.
func Materialize(skillsDir string, enabled []nanoprotocol.SkillDef) error {
	if skillsDir == "" {
		return nil
	}
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		return fmt.Errorf("create skills dir: %w", err)
	}
	managed, err := loadManifest(skillsDir)
	if err != nil {
		return err
	}

	wanted := map[string]bool{}
	for _, sk := range enabled {
		dir := sanitizeSkillDir(sk.Name)
		if dir == "" {
			slog.Warn("skillstore: skill has unusable name, skipping", "id", sk.ID, "name", sk.Name)
			continue
		}
		target := filepath.Join(skillsDir, dir)
		if _, isManaged := managed[dir]; !isManaged {
			if _, statErr := os.Stat(target); statErr == nil {
				backupLegacy(skillsDir, dir, target)
			}
		}
		if err := writeAtomic(skillsDir, dir, sk.Files); err != nil {
			slog.Warn("skillstore: failed to materialize skill", "id", sk.ID, "name", sk.Name, "error", err)
			continue
		}
		wanted[dir] = true
	}

	for dir := range managed {
		if !wanted[dir] {
			_ = os.RemoveAll(filepath.Join(skillsDir, dir))
			delete(managed, dir)
		}
	}
	_ = os.RemoveAll(filepath.Join(skillsDir, tmpDir))
	return saveManifest(skillsDir, wanted)
}

// backupLegacy moves an unmanaged, catalog-named directory aside so it cannot
// shadow the catalog copy. Only called when the name is not managed.
func backupLegacy(skillsDir, name, target string) {
	dest := filepath.Join(skillsDir, legacyDir, fmt.Sprintf("%s-%d", name, time.Now().Unix()))
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		slog.Warn("skillstore: cannot create legacy backup dir", "skill", name, "error", err)
		return
	}
	if err := os.Rename(target, dest); err != nil {
		slog.Warn("skillstore: cannot back up legacy skill dir", "skill", name, "error", err)
		return
	}
	slog.Info("skillstore: backed up legacy skill dir", "skill", name, "backup", dest)
}

// writeAtomic writes files into a temp dir, then swaps it into place so an
// agent reading the directory never sees a partially written skill.
func writeAtomic(skillsDir, name string, files map[string]string) error {
	tmp := filepath.Join(skillsDir, tmpDir, fmt.Sprintf("%s-%d", name, time.Now().UnixNano()))
	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	if err := writeFiles(tmp, files); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	target := filepath.Join(skillsDir, name)
	if err := os.RemoveAll(target); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	return nil
}

func sanitizeSkillDir(name string) string {
	name = strings.TrimSpace(name)
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" || out == "." {
		return ""
	}
	return out
}

func writeFiles(dir string, files map[string]string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	hasSKILL := false
	for path, content := range files {
		clean := filepath.Clean(path)
		if strings.HasPrefix(clean, "..") || strings.HasPrefix(clean, "/") || filepath.IsAbs(clean) {
			return fmt.Errorf("unsafe skill file path %q", path)
		}
		if clean == "SKILL.md" {
			hasSKILL = true
		}
		target := filepath.Join(dir, clean)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", filepath.Dir(target), err)
		}
		if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", clean, err)
		}
	}
	if !hasSKILL {
		return fmt.Errorf("skill has no SKILL.md")
	}
	return nil
}

func loadManifest(skillsDir string) (map[string]bool, error) {
	data, err := os.ReadFile(filepath.Join(skillsDir, manifestName))
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]bool{}, nil
		}
		return nil, err
	}
	var managed []string
	if json.Unmarshal(data, &managed) != nil {
		return map[string]bool{}, nil
	}
	out := map[string]bool{}
	for _, name := range managed {
		out[name] = true
	}
	return out, nil
}

func saveManifest(skillsDir string, managed map[string]bool) error {
	list := make([]string, 0, len(managed))
	for name := range managed {
		list = append(list, name)
	}
	data, err := json.Marshal(list)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(skillsDir, manifestName), data, 0o644)
}
