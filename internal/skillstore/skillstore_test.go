package skillstore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitee.com/kobeyoung81/nanoprotocol"
)

func TestMaterialize_WriteRemovePreserve(t *testing.T) {
	dir := t.TempDir()

	// Unrelated user-authored dir should survive.
	if err := os.MkdirAll(filepath.Join(dir, "user-skill"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "user-skill", "SKILL.md"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}

	enabled := []nanoprotocol.SkillDef{
		{ID: "skill:one", Name: "one", Files: map[string]string{"SKILL.md": "# one", "refs/guide.md": "g"}},
	}
	if err := Materialize(dir, enabled); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "one", "SKILL.md")); err != nil {
		t.Fatalf("skill one not materialized: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "one", "refs", "guide.md")); err != nil {
		t.Fatalf("skill one sub-file missing: %v", err)
	}
	// Empty list removes the previously-managed skill.
	if err := Materialize(dir, nil); err != nil {
		t.Fatalf("materialize-empty: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "one")); !os.IsNotExist(err) {
		t.Fatalf("managed skill should have been removed (stat err=%v)", err)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "user-skill", "SKILL.md")); string(data) != "mine" {
		t.Fatal("unrelated user skill was modified")
	}
}

func TestMaterialize_TakesOverLegacyCollision(t *testing.T) {
	dir := t.TempDir()

	// A legacy template copy (unmanaged) with a catalog name.
	if err := os.MkdirAll(filepath.Join(dir, "weather"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "weather", "SKILL.md"), []byte("legacy"), 0o644); err != nil {
		t.Fatal(err)
	}

	enabled := []nanoprotocol.SkillDef{
		{ID: "skill:bundled-weather", Name: "weather", Files: map[string]string{"SKILL.md": "catalog"}},
	}
	if err := Materialize(dir, enabled); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "weather", "SKILL.md")); string(data) != "catalog" {
		t.Fatalf("catalog content should shadow the legacy copy, got %q", data)
	}

	// The legacy content is preserved under .legacy/.
	var found bool
	_ = filepath.WalkDir(filepath.Join(dir, legacyDir), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if b, rerr := os.ReadFile(path); rerr == nil && strings.TrimSpace(string(b)) == "legacy" {
			found = true
		}
		return nil
	})
	if !found {
		t.Fatal("legacy copy was not backed up")
	}
}

func TestMaterialize_RejectsUnsafePaths(t *testing.T) {
	dir := t.TempDir()
	enabled := []nanoprotocol.SkillDef{
		{ID: "skill:evil", Name: "evil", Files: map[string]string{"../../escape.md": "x"}},
	}
	_ = Materialize(dir, enabled)
	if _, err := os.Stat(filepath.Dir(dir) + "/escape.md"); !os.IsNotExist(err) {
		t.Fatal("skill escaped the skills dir")
	}
}

func TestMaterialize_LeavesNoTempDir(t *testing.T) {
	dir := t.TempDir()
	if err := Materialize(dir, []nanoprotocol.SkillDef{
		{ID: "skill:one", Name: "one", Files: map[string]string{"SKILL.md": "# one"}},
	}); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, tmpDir)); !os.IsNotExist(err) {
		t.Fatal("temp dir should be cleaned up")
	}
}
