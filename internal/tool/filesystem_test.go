package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSafePathIn_RejectsSymlinkEscape(t *testing.T) {
	tmpDir := t.TempDir()

	workspace := filepath.Join(tmpDir, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(tmpDir, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(workspace, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	if _, err := safePath(workspace, filepath.Join(link, "secret.txt")); err == nil {
		t.Fatal("expected symlink escape to be rejected")
	}
}

func TestSafePathIn_AllowsSharedspaceSymlink(t *testing.T) {
	tmpDir := t.TempDir()

	workspace := filepath.Join(tmpDir, "workspace")
	shared := filepath.Join(tmpDir, "sharedspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(workspace, "sharedspace")
	if err := os.Symlink(shared, link); err != nil {
		t.Fatal(err)
	}

	got, err := safePathIn(workspace, shared, filepath.Join(link, "file.txt"))
	if err != nil {
		t.Fatalf("expected sharedspace symlink path allowed: %v", err)
	}
	if want := filepath.Join(link, "file.txt"); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSafePath(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "fs-test")
	defer os.RemoveAll(tmpDir)

	tests := []struct {
		name      string
		workspace string
		path      string
		wantErr   bool
	}{
		{"EmptyWorkspace", "", "/tmp/foo", false},
		{"ValidPath", tmpDir, filepath.Join(tmpDir, "file.txt"), false},
		{"RelativePath", tmpDir, "file.txt", false}, // filepath.Abs handles this based on CWD, might be tricky if CWD != tmpDir
		{"OutsideWorkspace", tmpDir, "/etc/passwd", true},
		{"TraversalAttempt", tmpDir, "../passwd", true}, // Abs resolves this
	}

	// For relative path test, we need to be careful about CWD.
	// safePath expands relative paths against CWD, not workspace base necessarily, unless we change safePath logic.
	// Looking at safePath implementation:
	// absPath, _ := filepath.Abs(path)
	// absWorkspace, _ := filepath.Abs(workspace)
	// strings.HasPrefix(absPath, absWorkspace)
	// So if path is "file.txt", it becomes CWD/file.txt. If CWD is not inside workspace, it fails.
	// We'll skip complex relative path tests that depend on CWD for now or use absolute paths.

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Skip relative path test if it depends on CWD
			if tt.name == "RelativePath" {
				return
			}
			_, err := safePath(tt.workspace, tt.path)
			if (err != nil) != tt.wantErr {
				t.Errorf("safePath() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestFilesystemTools(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "fs-tools-test")
	defer os.RemoveAll(tmpDir)

	// WriteFile
	wt := &WriteFileTool{Workspace: tmpDir}
	filePath := filepath.Join(tmpDir, "test.txt")
	_, err := wt.Execute(context.Background(), map[string]any{
		"path":    filePath,
		"content": "hello world",
	})
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	// Verify content
	content, _ := os.ReadFile(filePath)
	if string(content) != "hello world" {
		t.Errorf("Expected 'hello world', got '%s'", string(content))
	}

	// ReadFile
	rt := &ReadFileTool{Workspace: tmpDir}
	res, err := rt.Execute(context.Background(), map[string]any{"path": filePath})
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if res != "hello world" {
		t.Errorf("ReadFile returned '%s', expected 'hello world'", res)
	}

	// ReadFile Access Denied
	_, err = rt.Execute(context.Background(), map[string]any{"path": "/etc/hosts"})
	if err == nil {
		t.Error("Expected error for accessing file outside workspace")
	} else if !strings.Contains(err.Error(), "access denied") {
		t.Errorf("Expected 'access denied' error, got: %v", err)
	}

	// ListDir
	lt := &ListDirTool{Workspace: tmpDir}
	res, err = lt.Execute(context.Background(), map[string]any{"path": tmpDir})
	if err != nil {
		t.Fatalf("ListDir failed: %v", err)
	}
	if !strings.Contains(res, "test.txt") {
		t.Errorf("ListDir result '%s' does not contain 'test.txt'", res)
	}
}

func TestEditFileTool_BasicReplace(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "edit-test")
	defer os.RemoveAll(tmpDir)

	filePath := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(filePath, []byte("hello world"), 0644); err != nil {
		t.Fatal(err)
	}

	et := &EditFileTool{Workspace: tmpDir}
	res, err := et.Execute(context.Background(), map[string]any{
		"path":     filePath,
		"old_text": "world",
		"new_text": "Go",
	})
	if err != nil {
		t.Fatalf("EditFile failed: %v", err)
	}
	if res != "File edited successfully" {
		t.Errorf("unexpected result: %s", res)
	}

	content, _ := os.ReadFile(filePath)
	if string(content) != "hello Go" {
		t.Errorf("expected 'hello Go', got '%s'", string(content))
	}
}

func TestEditFileTool_NotFound(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "edit-test")
	defer os.RemoveAll(tmpDir)

	filePath := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(filePath, []byte("hello world"), 0644); err != nil {
		t.Fatal(err)
	}

	et := &EditFileTool{Workspace: tmpDir}
	_, err := et.Execute(context.Background(), map[string]any{
		"path":     filePath,
		"old_text": "nonexistent",
		"new_text": "replacement",
	})
	if err == nil {
		t.Fatal("expected error for missing old_text")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' error, got: %v", err)
	}
}

func TestEditFileTool_Ambiguous(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "edit-test")
	defer os.RemoveAll(tmpDir)

	filePath := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(filePath, []byte("aaa bbb aaa"), 0644); err != nil {
		t.Fatal(err)
	}

	et := &EditFileTool{Workspace: tmpDir}
	_, err := et.Execute(context.Background(), map[string]any{
		"path":     filePath,
		"old_text": "aaa",
		"new_text": "ccc",
	})
	if err == nil {
		t.Fatal("expected error for ambiguous match")
	}
	if !strings.Contains(err.Error(), "2 times") {
		t.Errorf("expected 'appears 2 times' error, got: %v", err)
	}
}

func TestEditFileTool_WorkspaceRestriction(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "edit-test")
	defer os.RemoveAll(tmpDir)

	et := &EditFileTool{Workspace: tmpDir}
	_, err := et.Execute(context.Background(), map[string]any{
		"path":     "/etc/passwd",
		"old_text": "root",
		"new_text": "nope",
	})
	if err == nil {
		t.Fatal("expected error for path outside workspace")
	}
	if !strings.Contains(err.Error(), "access denied") {
		t.Errorf("expected 'access denied' error, got: %v", err)
	}
}

func TestEditFileTool_EmptyNewText(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "edit-test")
	defer os.RemoveAll(tmpDir)

	filePath := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(filePath, []byte("hello world"), 0644); err != nil {
		t.Fatal(err)
	}

	et := &EditFileTool{Workspace: tmpDir}
	_, err := et.Execute(context.Background(), map[string]any{
		"path":     filePath,
		"old_text": " world",
		"new_text": "",
	})
	if err != nil {
		t.Fatalf("EditFile failed: %v", err)
	}

	content, _ := os.ReadFile(filePath)
	if string(content) != "hello" {
		t.Errorf("expected 'hello', got '%s'", string(content))
	}
}

func TestAppendNoteTool_CreateNew(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "note-test")
	defer os.RemoveAll(tmpDir)

	fixedTime := time.Date(2025, 6, 15, 14, 30, 0, 0, time.UTC)
	nt := &AppendNoteTool{
		MemoryDir: filepath.Join(tmpDir, "memory"),
		nowFunc:   func() time.Time { return fixedTime },
	}

	res, err := nt.Execute(context.Background(), map[string]any{
		"content": "first note",
	})
	if err != nil {
		t.Fatalf("AppendNote failed: %v", err)
	}
	if !strings.Contains(res, "2025-06-15.md") {
		t.Errorf("unexpected result: %s", res)
	}

	content, _ := os.ReadFile(filepath.Join(tmpDir, "memory", "2025-06-15.md"))
	s := string(content)
	if !strings.Contains(s, "# 2025-06-15") {
		t.Errorf("expected date header, got: %s", s)
	}
	if !strings.Contains(s, "[14:30] first note") {
		t.Errorf("expected timestamped entry, got: %s", s)
	}
}

func TestAppendNoteTool_AppendExisting(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "note-test")
	defer os.RemoveAll(tmpDir)

	fixedTime := time.Date(2025, 6, 15, 14, 30, 0, 0, time.UTC)
	nt := &AppendNoteTool{
		MemoryDir: filepath.Join(tmpDir, "memory"),
		nowFunc:   func() time.Time { return fixedTime },
	}

	// Create first entry
	if _, err := nt.Execute(context.Background(), map[string]any{"content": "first"}); err != nil {
		t.Fatalf("AppendNote failed: %v", err)
	}

	// Append second
	nt.nowFunc = func() time.Time { return fixedTime.Add(30 * time.Minute) }
	_, err := nt.Execute(context.Background(), map[string]any{"content": "second"})
	if err != nil {
		t.Fatalf("AppendNote failed: %v", err)
	}

	content, _ := os.ReadFile(filepath.Join(tmpDir, "memory", "2025-06-15.md"))
	s := string(content)
	if !strings.Contains(s, "[14:30] first") {
		t.Errorf("expected first entry, got: %s", s)
	}
	if !strings.Contains(s, "[15:00] second") {
		t.Errorf("expected second entry, got: %s", s)
	}
}

func TestAppendNoteTool_MultipleEntries(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "note-test")
	defer os.RemoveAll(tmpDir)

	baseTime := time.Date(2025, 6, 15, 9, 0, 0, 0, time.UTC)
	nt := &AppendNoteTool{
		MemoryDir: filepath.Join(tmpDir, "memory"),
		nowFunc:   func() time.Time { return baseTime },
	}

	entries := []string{"morning standup", "code review", "lunch break"}
	for i, entry := range entries {
		nt.nowFunc = func() time.Time { return baseTime.Add(time.Duration(i) * time.Hour) }
		if _, err := nt.Execute(context.Background(), map[string]any{"content": entry}); err != nil {
			t.Fatalf("AppendNote failed: %v", err)
		}
	}

	content, _ := os.ReadFile(filepath.Join(tmpDir, "memory", "2025-06-15.md"))
	s := string(content)
	for _, entry := range entries {
		if !strings.Contains(s, entry) {
			t.Errorf("expected entry '%s' in file, got: %s", entry, s)
		}
	}
}

func TestAppendNoteTool_CreatesMemoryDir(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "note-test")
	defer os.RemoveAll(tmpDir)

	memDir := filepath.Join(tmpDir, "memory")
	// Confirm memory dir does not exist
	if _, err := os.Stat(memDir); !os.IsNotExist(err) {
		t.Fatal("memory dir should not exist yet")
	}

	nt := &AppendNoteTool{
		MemoryDir: memDir,
		nowFunc:   func() time.Time { return time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC) },
	}
	_, err := nt.Execute(context.Background(), map[string]any{"content": "test"})
	if err != nil {
		t.Fatalf("AppendNote failed: %v", err)
	}

	if _, err := os.Stat(memDir); os.IsNotExist(err) {
		t.Error("memory dir should have been created")
	}
}

func TestWriteFileTool_AppendCreatesAndAppends(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "big.md")
	wt := &WriteFileTool{Workspace: dir}

	res, err := wt.Execute(context.Background(), map[string]any{
		"path":    file,
		"content": "first",
		"append":  true,
	})
	if err != nil {
		t.Fatalf("append to a missing file should create it: %v", err)
	}
	if !strings.Contains(res, "file now 5 bytes") {
		t.Errorf("result should report the file size, got %q", res)
	}

	if _, err := wt.Execute(context.Background(), map[string]any{
		"path":    file,
		"content": "-second",
		"append":  true,
	}); err != nil {
		t.Fatalf("second append: %v", err)
	}

	content, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "first-second" {
		t.Errorf("append should keep the earlier bytes, got %q", string(content))
	}
}

func TestWriteFileTool_OverwriteIsDefault(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(file, []byte("old content"), 0644); err != nil {
		t.Fatal(err)
	}

	wt := &WriteFileTool{Workspace: dir}
	if _, err := wt.Execute(context.Background(), map[string]any{
		"path":    file,
		"content": "new",
	}); err != nil {
		t.Fatal(err)
	}

	content, _ := os.ReadFile(file)
	if string(content) != "new" {
		t.Errorf("write without append must overwrite, got %q", string(content))
	}
}

func TestWriteFileTool_RefusesOversizeContent(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "big.txt")
	wt := &WriteFileTool{Workspace: dir}

	atLimit := strings.Repeat("a", maxInlineWriteBytes)
	if _, err := wt.Execute(context.Background(), map[string]any{"path": file, "content": atLimit}); err != nil {
		t.Fatalf("content at the limit should be accepted: %v", err)
	}

	over := atLimit + "a"
	_, err := wt.Execute(context.Background(), map[string]any{"path": file, "content": over})
	if err == nil {
		t.Fatal("content past the limit should be refused")
	}
	if !strings.Contains(err.Error(), "append=true") {
		t.Errorf("the refusal must name the smaller route, got %q", err.Error())
	}

	content, _ := os.ReadFile(file)
	if len(content) != maxInlineWriteBytes {
		t.Errorf("a refused write must not touch the file, size is %d", len(content))
	}
}

func TestWriteFileTool_ReportsBytes(t *testing.T) {
	dir := t.TempDir()
	wt := &WriteFileTool{Workspace: dir}

	res, err := wt.Execute(context.Background(), map[string]any{
		"path":    filepath.Join(dir, "b.txt"),
		"content": "你好",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "File written 6 bytes (file now 6 bytes)") {
		t.Errorf("result should carry the written and total bytes, got %q", res)
	}
}

func TestEditFileTool_RefusesOversizeOldText(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(file, []byte("short"), 0644); err != nil {
		t.Fatal(err)
	}

	et := &EditFileTool{Workspace: dir}
	_, err := et.Execute(context.Background(), map[string]any{
		"path":     file,
		"old_text": strings.Repeat("a", maxInlineWriteBytes+1),
		"new_text": "b",
	})
	if err == nil {
		t.Fatal("an oversize old_text should be refused")
	}
	if !strings.Contains(err.Error(), "shortest unique fragment") {
		t.Errorf("the refusal must name the smaller route, got %q", err.Error())
	}

	content, _ := os.ReadFile(file)
	if string(content) != "short" {
		t.Errorf("a refused edit must not touch the file, got %q", string(content))
	}
}

func TestEditFileTool_RefusesOversizeNewText(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(file, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}

	et := &EditFileTool{Workspace: dir}
	_, err := et.Execute(context.Background(), map[string]any{
		"path":     file,
		"old_text": "old",
		"new_text": strings.Repeat("b", maxInlineWriteBytes+1),
	})
	if err == nil {
		t.Fatal("an oversize new_text should be refused")
	}
	if !strings.Contains(err.Error(), "append=true") {
		t.Errorf("the refusal must name the smaller route, got %q", err.Error())
	}

	content, _ := os.ReadFile(file)
	if string(content) != "old" {
		t.Errorf("a refused edit must not touch the file, got %q", string(content))
	}
}
