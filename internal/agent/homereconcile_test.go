package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// newTemplateDir writes a template with the given files and returns a manifest
// built from them (version 1 unless the caller rewrites it).
func newTemplateDir(t *testing.T, files map[string]string) (string, *TemplateManifest) {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range files {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	manifest, err := LoadTemplateManifest(writeManifest(t, dir, 1, mapKeys(files)))
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if manifest == nil {
		t.Fatal("expected a manifest")
	}
	return dir, manifest
}

// writeManifest writes template/.template.json from the current file contents.
func writeManifest(t *testing.T, dir string, version int, paths []string) string {
	t.Helper()
	files := map[string]string{}
	for _, rel := range paths {
		hash, err := hashFile(filepath.Join(dir, rel))
		if err != nil {
			t.Fatalf("hash %s: %v", rel, err)
		}
		files[rel] = hash
	}
	if err := os.WriteFile(filepath.Join(dir, templateManifestName),
		[]byte(`{"version":`+itoa(version)+`,"files":{`+jsonPairs(files)+`}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func itoa(n int) string { return string(rune('0' + n)) }

func jsonPairs(m map[string]string) string {
	out := ""
	for k, v := range m {
		if out != "" {
			out += ","
		}
		out += `"` + k + `":"` + v + `"`
	}
	return out
}

func mapKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// TestReconcileTemplate_CopiesMissingAndRecordsBaseline covers a fresh home: the
// file is created and its hash recorded, so a later template can replace it while
// the agent never edited it.
func TestReconcileTemplate_CopiesMissingAndRecordsBaseline(t *testing.T) {
	tpl, manifest := newTemplateDir(t, map[string]string{"AGENT.md": "v1 identity\n"})
	home := t.TempDir()

	results, err := ReconcileTemplate(manifest, tpl, home, false, false)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(results) != 1 || results[0].Action != ReconcileCopied {
		t.Fatalf("results = %+v, want one copied", results)
	}
	if got := readFile(t, filepath.Join(home, "AGENT.md")); got != "v1 identity\n" {
		t.Errorf("content = %q", got)
	}
	state := LoadHomeState(home)
	if state.TemplateVersion != 1 || state.Applied["AGENT.md"] != manifest.Files["AGENT.md"] {
		t.Errorf("state = %+v", state)
	}
}

// TestReconcileTemplate_UpdatesPlatformContent is the point of the mechanism: a
// template bump reaches a home whose file the agent never touched.
func TestReconcileTemplate_UpdatesPlatformContent(t *testing.T) {
	tpl, manifest := newTemplateDir(t, map[string]string{"AGENT.md": "v1 identity\n"})
	home := t.TempDir()
	if _, err := ReconcileTemplate(manifest, tpl, home, false, false); err != nil {
		t.Fatal(err)
	}

	// Ship template v2.
	if err := os.WriteFile(filepath.Join(tpl, "AGENT.md"), []byte("v2 identity\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeManifest(t, tpl, 2, []string{"AGENT.md"})
	v2, err := LoadTemplateManifest(tpl)
	if err != nil {
		t.Fatal(err)
	}

	results, err := ReconcileTemplate(v2, tpl, home, false, false)
	if err != nil {
		t.Fatalf("reconcile v2: %v", err)
	}
	if results[0].Action != ReconcileUpdated {
		t.Fatalf("results = %+v, want updated", results)
	}
	if got := readFile(t, filepath.Join(home, "AGENT.md")); got != "v2 identity\n" {
		t.Errorf("content = %q, want the v2 template", got)
	}
	if state := LoadHomeState(home); state.TemplateVersion != 2 {
		t.Errorf("state version = %d, want 2", state.TemplateVersion)
	}
}

// TestReconcileTemplate_KeepsEditedFile verifies a user edit is never clobbered,
// even by a template bump — it is reported instead.
func TestReconcileTemplate_KeepsEditedFile(t *testing.T) {
	tpl, manifest := newTemplateDir(t, map[string]string{"AGENT.md": "v1 identity\n"})
	home := t.TempDir()
	if _, err := ReconcileTemplate(manifest, tpl, home, false, false); err != nil {
		t.Fatal(err)
	}
	edited := "v1 identity\n\n## Mine\n- remember the thing\n"
	if err := os.WriteFile(filepath.Join(home, "AGENT.md"), []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(tpl, "AGENT.md"), []byte("v2 identity\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeManifest(t, tpl, 2, []string{"AGENT.md"})
	v2, _ := LoadTemplateManifest(tpl)

	results, err := ReconcileTemplate(v2, tpl, home, false, false)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if results[0].Action != ReconcileKeptEdited {
		t.Fatalf("results = %+v, want kept-edited", results)
	}
	if got := readFile(t, filepath.Join(home, "AGENT.md")); got != edited {
		t.Errorf("the user's file was modified: %q", got)
	}

	// --force overrides, for an operator who decided the edit should go.
	forced, err := ReconcileTemplate(v2, tpl, home, true, false)
	if err != nil {
		t.Fatalf("forced reconcile: %v", err)
	}
	if forced[0].Action != ReconcileUpdated {
		t.Fatalf("forced results = %+v, want updated", forced)
	}
	if got := readFile(t, filepath.Join(home, "AGENT.md")); got != "v2 identity\n" {
		t.Errorf("content = %q, want the forced template", got)
	}
}

// TestReconcileTemplate_RecognisesPreviousVersion covers homes created before the
// manifest existed: their content matches an older template, so it is replaced.
func TestReconcileTemplate_RecognisesPreviousVersion(t *testing.T) {
	tpl, _ := newTemplateDir(t, map[string]string{"AGENT.md": "v1 identity\n"})
	v1Hash, err := hashFile(filepath.Join(tpl, "AGENT.md"))
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "AGENT.md"), []byte("v1 identity\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// v2 template that remembers v1 as a previous version.
	if err := os.WriteFile(filepath.Join(tpl, "AGENT.md"), []byte("v2 identity\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	v2Hash, _ := hashFile(filepath.Join(tpl, "AGENT.md"))
	manifestRaw := `{"version":2,"files":{"AGENT.md":"` + v2Hash + `"},"previous":{"AGENT.md":["` + v1Hash + `"]}}`
	if err := os.WriteFile(filepath.Join(tpl, templateManifestName), []byte(manifestRaw), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest, _ := LoadTemplateManifest(tpl)

	results, err := ReconcileTemplate(manifest, tpl, home, false, false)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if results[0].Action != ReconcileUpdated {
		t.Fatalf("results = %+v, want updated via the previous-version hash", results)
	}
	if got := readFile(t, filepath.Join(home, "AGENT.md")); got != "v2 identity\n" {
		t.Errorf("content = %q", got)
	}
}

// TestReconcileTemplate_DryRunWritesNothing verifies the operator preview.
func TestReconcileTemplate_DryRunWritesNothing(t *testing.T) {
	tpl, manifest := newTemplateDir(t, map[string]string{"AGENT.md": "v1 identity\n"})
	home := t.TempDir()

	results, err := ReconcileTemplate(manifest, tpl, home, false, true)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if results[0].Action != ReconcileCopied {
		t.Errorf("dry run should still report the action, got %+v", results)
	}
	if _, err := os.Stat(filepath.Join(home, "AGENT.md")); !os.IsNotExist(err) {
		t.Error("dry run created the file")
	}
	if _, err := os.Stat(filepath.Join(home, homeStateName)); !os.IsNotExist(err) {
		t.Error("dry run wrote the state file")
	}
}

// TestReconcileHomeTemplate_SeedsMemoryOnceOnly verifies runtime data stays out
// of reconciliation: memory/MEMORY.md is created when missing, then never
// touched again because it is not in the manifest.
func TestReconcileHomeTemplate_SeedsMemoryOnceOnly(t *testing.T) {
	tpl, _ := newTemplateDir(t, map[string]string{
		"AGENT.md":         "v1 identity\n",
		"memory/MEMORY.md": "# MEMORY\n\nplaceholder\n",
	})
	home := t.TempDir()

	if err := ReconcileHomeTemplate(tpl, home); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	memory := filepath.Join(home, "memory", "MEMORY.md")
	if got := readFile(t, memory); got != "# MEMORY\n\nplaceholder\n" {
		t.Errorf("memory not seeded: %q", got)
	}

	learned := "# MEMORY\n\n- the user prefers Chinese\n"
	if err := os.WriteFile(memory, []byte(learned), 0o644); err != nil {
		t.Fatal(err)
	}
	// A new template version must not reach the memory file.
	if err := os.WriteFile(filepath.Join(tpl, "memory", "MEMORY.md"), []byte("# MEMORY\n\nnew placeholder\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileHomeTemplate(tpl, home); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if got := readFile(t, memory); got != learned {
		t.Errorf("memory was overwritten by the template: %q", got)
	}
}

// TestReconcileHomeTemplate_NoManifestFallsBackToCopyMissing keeps older images
// working: with no manifest, only missing files are created.
func TestReconcileHomeTemplate_NoManifestFallsBackToCopyMissing(t *testing.T) {
	tpl := t.TempDir()
	if err := os.WriteFile(filepath.Join(tpl, "AGENT.md"), []byte("v1 identity\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "AGENT.md"), []byte("user file\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := ReconcileHomeTemplate(tpl, home); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := readFile(t, filepath.Join(home, "AGENT.md")); got != "user file\n" {
		t.Errorf("existing file replaced without a manifest: %q", got)
	}
}
