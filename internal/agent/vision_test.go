package agent

import (
	"os"
	"path/filepath"
	"testing"

	"gitee.com/kobeyoung81/nanosec/internal/provider"
)

func TestWithinRoot(t *testing.T) {
	root := "/data/agents/alice"
	cases := []struct {
		path string
		want bool
	}{
		{filepath.Join(root, "uploads/img.png"), true},
		{filepath.Join(root, "a/b/../c.png"), true},
		{filepath.Join(root, "..", "bob", "secret.png"), false},
		{"/etc/passwd", false},
	}
	for _, tc := range cases {
		if got := withinRoot(tc.path, root); got != tc.want {
			t.Errorf("withinRoot(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestExpandImages(t *testing.T) {
	dir := t.TempDir()
	shared := t.TempDir()

	// A small "png" (content is opaque to the adapter).
	img := filepath.Join(dir, "uploads", "pic.png")
	if err := os.MkdirAll(filepath.Dir(img), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(img, []byte("not-a-real-png-but-fine"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := &Agent{config: Config{WorkspaceDir: dir, SharedWorkspaceDir: shared}}

	msgs := []provider.Message{
		{Role: "user", Content: "look", Images: []provider.ImageRef{{Path: "uploads/pic.png", MimeType: "image/png"}}},
		{Role: "user", Content: "text only"},
	}
	out := a.expandImages(msgs)

	if len(out[0].Parts) != 2 {
		t.Fatalf("expected text+image parts, got %+v", out[0].Parts)
	}
	if out[0].Parts[1].Type != "image_url" || out[0].Parts[1].ImageURL[:15] != "data:image/png;" {
		t.Errorf("unexpected image part: %+v", out[0].Parts[1])
	}
	if out[1].Parts != nil {
		t.Errorf("text-only message should be untouched: %+v", out[1].Parts)
	}
	// The source slice must not be mutated with base64.
	if msgs[0].Parts != nil {
		t.Error("source message was mutated")
	}
}

func TestExpandImages_SkipsOutsideWorkspaceAndOversize(t *testing.T) {
	dir := t.TempDir()
	a := &Agent{config: Config{WorkspaceDir: dir, VisionMaxImageBytes: 4}}

	// Path traversal must be refused.
	out := a.expandImages([]provider.Message{
		{Role: "user", Content: "x", Images: []provider.ImageRef{{Path: "../secret.png", MimeType: "image/png"}}},
	})
	if out[0].Parts != nil {
		t.Errorf("traversal path should be skipped, got %+v", out[0].Parts)
	}

	// Oversize file must be skipped.
	img := filepath.Join(dir, "big.png")
	if err := os.WriteFile(img, []byte("way-too-big"), 0o644); err != nil {
		t.Fatal(err)
	}
	out = a.expandImages([]provider.Message{
		{Role: "user", Content: "x", Images: []provider.ImageRef{{Path: "big.png", MimeType: "image/png"}}},
	})
	if out[0].Parts != nil {
		t.Errorf("oversize image should be skipped, got %+v", out[0].Parts)
	}
}
