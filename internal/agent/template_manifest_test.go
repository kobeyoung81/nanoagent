package agent

import (
	"path/filepath"
	"testing"
)

// TestShippedTemplateManifestMatchesTheFiles guards the template manifest against
// the one mistake that fails silently: a stale hash. A file whose shipped hash
// does not match the manifest is treated as a user edit by ReconcileTemplate, so
// the new content never reaches an existing agent home and nothing complains.
func TestShippedTemplateManifestMatchesTheFiles(t *testing.T) {
	tpl := filepath.Join("..", "..", "template")
	manifest, err := LoadTemplateManifest(tpl)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if manifest == nil {
		t.Fatal("the image ships no template manifest")
	}
	if len(manifest.Files) == 0 {
		t.Fatal("the manifest owns no files")
	}
	for path, want := range manifest.Files {
		got, err := hashFile(filepath.Join(tpl, path))
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if got != want {
			t.Errorf("%s: manifest hash %s does not match the shipped file (%s)", path, want, got)
		}
	}
	for path, olds := range manifest.Previous {
		for _, old := range olds {
			if old == manifest.Files[path] {
				t.Errorf("%s: a previous hash equals the current one", path)
			}
		}
	}
}
