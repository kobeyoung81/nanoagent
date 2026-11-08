package tool

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runGrep(t *testing.T, dir string, params map[string]any) string {
	t.Helper()
	p := map[string]any{"pattern": "NEEDLE"}
	for k, v := range params {
		p[k] = v
	}
	out, err := NewGrepTool(dir, "").Execute(context.Background(), p)
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	return out
}

// TestGrep_TruncatesLongLines pins the minified-bundle case: one match on a
// megabytes-long line must not become the whole result.
func TestGrep_TruncatesLongLines(t *testing.T) {
	dir := t.TempDir()
	long := "prefix NEEDLE " + strings.Repeat("x", 20000)
	if err := os.WriteFile(filepath.Join(dir, "bundle.js"), []byte(long+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := runGrep(t, dir, nil)
	if !strings.Contains(out, "NEEDLE") {
		t.Fatalf("expected the match, got: %q", tailOf(out, 200))
	}
	if len(out) > 1000 {
		t.Errorf("a single long line should be truncated, got %d bytes", len(out))
	}
	if !strings.Contains(out, "…") {
		t.Error("expected the truncation marker")
	}
}

// TestGrep_ReportsSkippedBinaryFiles pins that a search never looks complete
// while quietly ignoring binary files.
func TestGrep_ReportsSkippedBinaryFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "text.txt"), []byte("a NEEDLE here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	binary := append([]byte("NEEDLE"), []byte{0, 1, 2, 3, 0}...)
	if err := os.WriteFile(filepath.Join(dir, "blob.bin"), binary, 0o644); err != nil {
		t.Fatal(err)
	}

	out := runGrep(t, dir, nil)
	if !strings.Contains(out, "text.txt:1: a NEEDLE here") {
		t.Fatalf("expected the text match, got: %q", out)
	}
	if !strings.Contains(out, "[skipped 1 binary file(s)]") {
		t.Errorf("expected the skip notice, got: %q", out)
	}
}

func TestGrep_NoMatchesStillReportsSkips(t *testing.T) {
	dir := t.TempDir()
	binary := append([]byte("NEEDLE"), []byte{0, 1, 2, 0}...)
	if err := os.WriteFile(filepath.Join(dir, "blob.bin"), binary, 0o644); err != nil {
		t.Fatal(err)
	}

	out := runGrep(t, dir, nil)
	if !strings.HasPrefix(out, "No matches found.") {
		t.Fatalf("expected the empty result, got: %q", out)
	}
	if !strings.Contains(out, "[skipped 1 binary file(s)]") {
		t.Errorf("expected the skip notice, got: %q", out)
	}
}

// TestGrep_MaxResultsStillApplies guards the existing cap while the new notices
// are in place.
func TestGrep_MaxResultsStillApplies(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	for i := 0; i < 50; i++ {
		fmt.Fprintf(&b, "NEEDLE %d\n", i)
	}
	if err := os.WriteFile(filepath.Join(dir, "many.txt"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	out := runGrep(t, dir, map[string]any{"max_results": float64(5)})
	if got := strings.Count(out, "NEEDLE"); got != 5 {
		t.Errorf("expected 5 matches, got %d", got)
	}
	if !strings.Contains(out, "[truncated at 5 matches]") {
		t.Errorf("expected the truncation notice, got: %q", out)
	}
}
