package tool

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gitee.com/kobeyoung81/nanoprotocol"
)

func TestValidateSkillPackage(t *testing.T) {
	cases := []struct {
		name    string
		files   map[string]string
		wantErr bool
	}{
		{"valid", map[string]string{"SKILL.md": "# hi", "scripts/run.sh": "echo"}, false},
		{"missing skill.md", map[string]string{"notes.txt": "x"}, true},
		{"path traversal", map[string]string{"SKILL.md": "#", "../evil": "x"}, true},
		{"absolute path", map[string]string{"SKILL.md": "#", "/etc/passwd": "x"}, true},
		{"empty", map[string]string{}, true},
	}
	for _, tc := range cases {
		err := validateSkillPackage(tc.files)
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: err=%v wantErr=%v", tc.name, err, tc.wantErr)
		}
	}
}

func TestValidSkillName(t *testing.T) {
	for _, ok := range []string{"weather", "my-skill", "skill_1"} {
		if !validSkillName(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "a b", "a/b", "../x", "skill!"} {
		if validSkillName(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

// fakeSaver records what the tool asked the bus to store.
type fakeSaver struct {
	saved   *nanoprotocol.SkillSaveRequest
	deleted bool
	err     error
}

func (f *fakeSaver) SaveSkill(req nanoprotocol.SkillSaveRequest) (*nanoprotocol.SkillDef, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.saved = &req
	return &nanoprotocol.SkillDef{ID: "skill-1", Name: req.Name, Files: req.Files}, nil
}

func (f *fakeSaver) DeleteSkill(string, string, string) error {
	f.deleted = true
	return f.err
}

// testTool builds a tool whose workspace and skills directory live in one temp
// dir, so tests can write packages and installed copies side by side.
func testTool(t *testing.T) (*ManageSkillTool, *fakeSaver, string, string) {
	t.Helper()
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	skills := filepath.Join(root, "skills")
	for _, dir := range []string{workspace, skills} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	saver := &fakeSaver{}
	return NewManageSkillTool(saver, "user-1", "agent-1", workspace, "", skills), saver, workspace, skills
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// install writes a skill into the agent's skills directory, the way the
// hypervisor would have materialized it.
func install(t *testing.T, skills, name string, files map[string]string) {
	t.Helper()
	for path, content := range files {
		writeFile(t, filepath.Join(skills, name, filepath.FromSlash(path)), content)
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func mustKeys(t *testing.T, got map[string]string, want ...string) {
	t.Helper()
	sort.Strings(want)
	if strings.Join(keys(got), ",") != strings.Join(want, ",") {
		t.Fatalf("files = %v, want %v", keys(got), want)
	}
}

func TestManageSkillFilesDir(t *testing.T) {
	tool, saver, workspace, _ := testTool(t)
	writeFile(t, filepath.Join(workspace, "pkg", "SKILL.md"), "# skill")
	writeFile(t, filepath.Join(workspace, "pkg", "notes.md"), "notes")
	writeFile(t, filepath.Join(workspace, "pkg", "extra", "detail.md"), "detail")

	if _, err := tool.Execute(t.Context(), map[string]any{
		"action":    "create",
		"name":      "from-dir",
		"files_dir": "pkg",
	}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	mustKeys(t, saver.saved.Files, "SKILL.md", "extra/detail.md", "notes.md")
	if saver.saved.Files["extra/detail.md"] != "detail" {
		t.Fatalf("nested file content = %q", saver.saved.Files["extra/detail.md"])
	}
}

func TestManageSkillFilesOverrideDir(t *testing.T) {
	tool, saver, workspace, _ := testTool(t)
	writeFile(t, filepath.Join(workspace, "pkg", "SKILL.md"), "from dir")
	writeFile(t, filepath.Join(workspace, "pkg", "notes.md"), "from dir")

	if _, err := tool.Execute(t.Context(), map[string]any{
		"action":    "create",
		"name":      "mixed",
		"files_dir": "pkg",
		"files":     map[string]any{"SKILL.md": "from files"},
	}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if saver.saved.Files["SKILL.md"] != "from files" {
		t.Fatalf("files should win over the directory: %q", saver.saved.Files["SKILL.md"])
	}
	if saver.saved.Files["notes.md"] != "from dir" {
		t.Fatalf("directory file lost: %q", saver.saved.Files["notes.md"])
	}
}

func TestManageSkillRejectsDirOutsideWorkspace(t *testing.T) {
	tool, _, workspace, _ := testTool(t)
	writeFile(t, filepath.Join(filepath.Dir(workspace), "outside", "SKILL.md"), "# x")

	_, err := tool.Execute(t.Context(), map[string]any{
		"action":    "create",
		"name":      "escape",
		"files_dir": "../outside",
	})
	if err == nil || !strings.Contains(err.Error(), "outside the allowed workspaces") {
		t.Fatalf("err = %v, want workspace confinement", err)
	}
}

func TestManageSkillRejectsMissingOrFileDir(t *testing.T) {
	tool, _, workspace, _ := testTool(t)
	writeFile(t, filepath.Join(workspace, "plain.md"), "x")

	if _, err := tool.Execute(t.Context(), map[string]any{
		"action": "create", "name": "missing", "files_dir": "nope",
	}); err == nil {
		t.Fatal("missing files_dir should fail")
	}
	if _, err := tool.Execute(t.Context(), map[string]any{
		"action": "create", "name": "not-a-dir", "files_dir": "plain.md",
	}); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("err = %v, want not a directory", err)
	}
}

func TestManageSkillSkipsDotEntries(t *testing.T) {
	tool, saver, workspace, _ := testTool(t)
	writeFile(t, filepath.Join(workspace, "pkg", "SKILL.md"), "# skill")
	writeFile(t, filepath.Join(workspace, "pkg", ".DS_Store"), "junk")
	writeFile(t, filepath.Join(workspace, "pkg", ".git", "config"), "junk")
	writeFile(t, filepath.Join(workspace, "pkg", ".hidden", "note.md"), "junk")

	if _, err := tool.Execute(t.Context(), map[string]any{
		"action": "create", "name": "clean", "files_dir": "pkg",
	}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	mustKeys(t, saver.saved.Files, "SKILL.md")
}

func TestManageSkillDirQuotas(t *testing.T) {
	tool, _, workspace, _ := testTool(t)
	dir := filepath.Join(workspace, "many")
	writeFile(t, filepath.Join(dir, "SKILL.md"), "# skill")
	for i := 0; i < manageSkillMaxFiles; i++ {
		writeFile(t, filepath.Join(dir, fmt.Sprintf("f%02d.md", i)), "x")
	}
	if _, err := tool.Execute(t.Context(), map[string]any{
		"action": "create", "name": "many", "files_dir": "many",
	}); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("err = %v, want file count limit", err)
	}

	big := filepath.Join(workspace, "big")
	writeFile(t, filepath.Join(big, "SKILL.md"), "# skill")
	writeFile(t, filepath.Join(big, "huge.md"), strings.Repeat("x", manageSkillMaxBytes+1))
	if _, err := tool.Execute(t.Context(), map[string]any{
		"action": "create", "name": "big", "files_dir": "big",
	}); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("err = %v, want byte limit", err)
	}
}

func TestManageSkillRequiresSkillMarkdownWithoutMerge(t *testing.T) {
	tool, _, _, _ := testTool(t)
	_, err := tool.Execute(t.Context(), map[string]any{
		"action": "create",
		"name":   "no-skill",
		"files":  map[string]any{"notes.md": "x"},
	})
	if err == nil || !strings.Contains(err.Error(), "SKILL.md") {
		t.Fatalf("err = %v, want SKILL.md requirement", err)
	}
}

func TestManageSkillRejectsSilentRemoval(t *testing.T) {
	tool, _, _, skills := testTool(t)
	install(t, skills, "keep", map[string]string{"SKILL.md": "# old", "a.md": "a"})

	_, err := tool.Execute(t.Context(), map[string]any{
		"action": "update",
		"name":   "keep",
		"files":  map[string]any{"SKILL.md": "# new"},
	})
	if err == nil || !strings.Contains(err.Error(), "would remove") || !strings.Contains(err.Error(), "a.md") {
		t.Fatalf("err = %v, want a removal rejection naming a.md", err)
	}
}

func TestManageSkillAllowRemove(t *testing.T) {
	tool, saver, _, skills := testTool(t)
	install(t, skills, "keep", map[string]string{"SKILL.md": "# old", "a.md": "a"})

	if _, err := tool.Execute(t.Context(), map[string]any{
		"action":       "update",
		"name":         "keep",
		"files":        map[string]any{"SKILL.md": "# new"},
		"allow_remove": true,
	}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	mustKeys(t, saver.saved.Files, "SKILL.md")
}

func TestManageSkillUpdatesWhenNotInstalled(t *testing.T) {
	tool, saver, _, _ := testTool(t)
	if _, err := tool.Execute(t.Context(), map[string]any{
		"action": "update",
		"name":   "elsewhere",
		"files":  map[string]any{"SKILL.md": "# new"},
	}); err != nil {
		t.Fatalf("an absent local copy must not block the update: %v", err)
	}
	mustKeys(t, saver.saved.Files, "SKILL.md")
}

func TestManageSkillMergeKeepsInstalledFiles(t *testing.T) {
	tool, saver, _, skills := testTool(t)
	install(t, skills, "merge-me", map[string]string{"SKILL.md": "# baseline", "a.md": "old a"})

	if _, err := tool.Execute(t.Context(), map[string]any{
		"action": "update",
		"name":   "merge-me",
		"files":  map[string]any{"a.md": "new a"},
		"merge":  true,
	}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	mustKeys(t, saver.saved.Files, "SKILL.md", "a.md")
	if saver.saved.Files["SKILL.md"] != "# baseline" {
		t.Fatalf("baseline SKILL.md lost: %q", saver.saved.Files["SKILL.md"])
	}
	if saver.saved.Files["a.md"] != "new a" {
		t.Fatalf("submitted file did not win: %q", saver.saved.Files["a.md"])
	}
}

func TestManageSkillMergeWithFilesDir(t *testing.T) {
	tool, saver, workspace, skills := testTool(t)
	install(t, skills, "merged", map[string]string{"SKILL.md": "# baseline", "a.md": "old", "b.md": "b"})
	writeFile(t, filepath.Join(workspace, "edit", "a.md"), "new")

	if _, err := tool.Execute(t.Context(), map[string]any{
		"action":    "update",
		"name":      "merged",
		"files_dir": "edit",
		"merge":     true,
	}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	mustKeys(t, saver.saved.Files, "SKILL.md", "a.md", "b.md")
	if saver.saved.Files["a.md"] != "new" {
		t.Fatalf("a.md = %q", saver.saved.Files["a.md"])
	}
}

func TestManageSkillMergeNeedsBaseline(t *testing.T) {
	tool, _, workspace, _ := testTool(t)
	writeFile(t, filepath.Join(workspace, "edit", "a.md"), "new")

	_, err := tool.Execute(t.Context(), map[string]any{
		"action":    "update",
		"name":      "absent",
		"files_dir": "edit",
		"merge":     true,
	})
	if err == nil || !strings.Contains(err.Error(), "merge needs") {
		t.Fatalf("err = %v, want a merge baseline error", err)
	}
}

func TestManageSkillMergeWithAllowRemoveReplaces(t *testing.T) {
	tool, saver, _, skills := testTool(t)
	install(t, skills, "replace", map[string]string{"SKILL.md": "# old", "a.md": "a"})

	if _, err := tool.Execute(t.Context(), map[string]any{
		"action":       "update",
		"name":         "replace",
		"files":        map[string]any{"SKILL.md": "# new"},
		"merge":        true,
		"allow_remove": true,
	}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	mustKeys(t, saver.saved.Files, "SKILL.md")
}

func TestManageSkillMergeValidatesTheFinalPackage(t *testing.T) {
	tool, _, _, skills := testTool(t)
	install(t, skills, "huge", map[string]string{"SKILL.md": strings.Repeat("x", 600<<10)})

	_, err := tool.Execute(t.Context(), map[string]any{
		"action": "update",
		"name":   "huge",
		"files":  map[string]any{"notes.md": strings.Repeat("y", 500<<10)},
		"merge":  true,
	})
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("err = %v, want the merged package size rejected", err)
	}
}
