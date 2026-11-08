package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGrepTool_FindsMatches(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\nfunc Foo() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("nothing here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := NewGrepTool(dir, "")
	out, err := tool.Execute(context.Background(), map[string]any{"pattern": "func Foo", "include": "**/*.go"})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, "a.go:2: func Foo()") {
		t.Errorf("unexpected output: %q", out)
	}
	if strings.Contains(out, "b.txt") {
		t.Errorf("include filter should have excluded b.txt: %q", out)
	}
}

func TestGlobTool_Recursive(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "root.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "deep.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := NewGlobTool(dir, "")
	out, err := tool.Execute(context.Background(), map[string]any{"pattern": "**/*.go"})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, "root.go") || !strings.Contains(out, "sub/deep.go") {
		t.Errorf("expected both files, got: %q", out)
	}
}

func TestGlobToRegexp(t *testing.T) {
	cases := []struct {
		pattern string
		match   string
		ok      bool
	}{
		{"**/*.go", "a/b/c.go", true},
		{"**/*.go", "c.go", true},
		{"*.go", "c.go", true},
		{"*.go", "a/c.go", false},
		{"src/**", "src/a/b.ts", true},
	}
	for _, tc := range cases {
		re := compileGlob(tc.pattern)
		if got := re.MatchString(tc.match); got != tc.ok {
			t.Errorf("compileGlob(%q).MatchString(%q) = %v, want %v", tc.pattern, tc.match, got, tc.ok)
		}
	}
}

func TestTodoWriteTool_Validation(t *testing.T) {
	tool := NewTodoWriteTool()
	ok := map[string]any{"todos": []any{
		map[string]any{"content": "do a", "status": "in_progress"},
		map[string]any{"content": "do b", "status": "pending"},
	}}
	out, err := tool.Execute(context.Background(), ok)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, "do a") {
		t.Errorf("unexpected output: %q", out)
	}

	bad := map[string]any{"todos": []any{
		map[string]any{"content": "a", "status": "in_progress"},
		map[string]any{"content": "b", "status": "in_progress"},
	}}
	if _, err := tool.Execute(context.Background(), bad); err == nil {
		t.Error("expected error for two in_progress todos")
	}
}

func TestRenderTaskResult(t *testing.T) {
	out := RenderTaskResult("id1", "do a thing", "all done", false, 1234)
	if !strings.Contains(out, `<task id="id1" state="completed" elapsed_ms="1234">`) || !strings.Contains(out, "<task_result>\nall done\n</task_result>") {
		t.Errorf("unexpected completed output: %q", out)
	}
	errOut := RenderTaskResult("id2", "oops", "boom", true, 42)
	if !strings.Contains(errOut, "task_error") || !strings.Contains(errOut, `state="error"`) {
		t.Errorf("unexpected error output: %q", errOut)
	}
	if !strings.Contains(errOut, `elapsed_ms="42"`) {
		t.Errorf("expected the elapsed time on an error result: %q", errOut)
	}
}
