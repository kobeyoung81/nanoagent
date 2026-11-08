package tool

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gitee.com/kobeyoung81/nanosec/internal/provider"
)

// safePathIn checks if the path is within the workspace or the shared workspace.
// If both roots are empty, any path is allowed. Symlinks are resolved to their
// canonical location before the containment check so a symlink cannot escape
// the allowed roots (the deliberate sharedspace symlink is allowed).
func safePathIn(workspace, shared, path string) (string, error) {
	if workspace == "" && shared == "" {
		return path, nil
	}

	// Expand tilde in workspace
	if strings.HasPrefix(workspace, "~/") {
		dirname, _ := os.UserHomeDir()
		workspace = filepath.Join(dirname, workspace[2:])
	}
	if strings.HasPrefix(shared, "~/") {
		dirname, _ := os.UserHomeDir()
		shared = filepath.Join(dirname, shared[2:])
	}

	// Expand tilde in path
	if strings.HasPrefix(path, "~/") {
		dirname, _ := os.UserHomeDir()
		path = filepath.Join(dirname, path[2:])
	}

	// Resolve relative paths against the primary workspace root.
	if workspace != "" && !filepath.IsAbs(path) {
		path = filepath.Join(workspace, path)
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("invalid path: %w", err)
	}

	for _, root := range []string{workspace, shared} {
		if root == "" {
			continue
		}
		canonPath, err := canonical(absPath)
		if err != nil {
			continue
		}
		canonRoot, err := canonical(root)
		if err != nil {
			continue
		}
		if within(canonPath, canonRoot) {
			return absPath, nil
		}
	}

	return "", fmt.Errorf("access denied: path %s is outside the allowed workspaces", path)
}

// ResolveWorkspacePath is safePathIn for callers outside this package: the MCP
// bridge resolves a file argument's path with the same rule the file tools use,
// so a tool argument cannot reach outside the workspace either.
func ResolveWorkspacePath(workspace, shared, path string) (string, error) {
	return safePathIn(workspace, shared, path)
}

// canonical resolves symlinks for path (which may not exist yet) by resolving
// the nearest existing ancestor and re-appending the remaining suffix.
func canonical(path string) (string, error) {
	current := path
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			rel, rerr := filepath.Rel(current, path)
			if rerr != nil {
				return "", rerr
			}
			return filepath.Join(resolved, rel), nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		current = parent
	}
}

// within reports whether child is the same as or inside parent.
func within(child, parent string) bool {
	if child == parent {
		return true
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// safePath checks a path against a single workspace root.
func safePath(workspace, path string) (string, error) {
	return safePathIn(workspace, "", path)
}

// Read window defaults. The byte cap is injected from the agent's tool-result
// budget (see main.go) so the paging footer always survives the global
// truncation, which keeps the head of a result and would otherwise cut the
// footer — the one line that says how to continue — straight off.
const (
	defaultReadWindowLines = 2000
	defaultReadWindowBytes = 12000
	maxReadLineBytes       = 2000
	// countLinesMaxSize bounds the streaming line count: beyond this the read
	// reports the byte size only and lets the caller page by offset.
	countLinesMaxSize = 16 << 20
	// encodedReadMaxSize bounds whole-file decoding of UTF-16/32 text.
	encodedReadMaxSize = 4 << 20
	// editMaxBytes refuses an edit that would load the whole file.
	editMaxBytes = 8 << 20
	// maxInlineWriteBytes bounds the inline text of one write or edit call. The
	// upstream can stop a model mid-argument (finish_reason "length"), and an
	// argument cut in half is a call that never ran — so a body past this size
	// belongs in several calls: write the first chunk, then continue with
	// append=true. The number sits just above the read window so a chunk can be
	// read back in one or two calls.
	maxInlineWriteBytes = 16 << 10
)

// ReadFileTool reads a window of a file. A call never pulls a whole large file
// into the model's context: it returns at most MaxBytes (default
// defaultReadWindowBytes) and at most limit lines, plus a footer naming the line
// range, the file size and the offset that continues the read. Binary content is
// refused with a structured report instead of being handed over as mojibake.
type ReadFileTool struct {
	Workspace       string
	SharedWorkspace string
	// MaxBytes caps one call's returned text (0 = defaultReadWindowBytes).
	MaxBytes int
}

func (t *ReadFileTool) Name() string { return "read" }
func (t *ReadFileTool) Description() string {
	return `Read a text file. At most one window is returned per call (default 2000 lines and a byte cap), ` +
		`followed by a footer naming the line range and the offset that continues it — read a large file in pages ` +
		`instead of asking for all of it. Binary content (images, archives, databases, UTF-16 without a BOM) is ` +
		`refused with a report; a UTF-16/32 file with a BOM is decoded.`
}
func (t *ReadFileTool) Parameters() provider.ParameterSchema {
	return provider.ParameterSchema{
		Type: "object",
		Properties: map[string]provider.PropertySchema{
			"path":   {Type: "string", Description: "Path to the file to read"},
			"offset": {Type: "integer", Description: "First line to return (1-based; default 1)"},
			"limit":  {Type: "integer", Description: "Maximum lines to return (default 2000; the byte cap still applies)"},
		},
		Required: []string{"path"},
	}
}
func (t *ReadFileTool) Execute(ctx context.Context, params map[string]any) (string, error) {
	path, ok := params["path"].(string)
	if !ok {
		return "", fmt.Errorf("missing path")
	}

	safe, err := safePathIn(t.Workspace, t.SharedWorkspace, path)
	if err != nil {
		return "", err
	}

	f, err := os.Open(safe)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory — use the list tool", path)
	}

	capBytes := t.MaxBytes
	if capBytes <= 0 {
		capBytes = defaultReadWindowBytes
	}
	offset := intParam(params, "offset")
	limit := intParam(params, "limit")
	if offset < 1 {
		offset = 1
	}
	if limit <= 0 {
		limit = defaultReadWindowLines
	}

	kind := sniffHead(f, info.Size())

	if kind.Encoding != "" && kind.Encoding != "utf-8" {
		return readEncodedFile(safe, path, info.Size(), kind, offset, limit, capBytes)
	}
	if kind.Binary {
		return binaryReport(path, info.Size(), kind), nil
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	window, err := readLinesWindow(f, info.Size() <= countLinesMaxSize, offset, limit, capBytes)
	if err != nil {
		return "", err
	}
	return windowResult(path, info.Size(), kind, window), nil
}

// WriteFileTool writes content to a file.
type WriteFileTool struct {
	Workspace       string
	SharedWorkspace string
}

func (t *WriteFileTool) Name() string { return "write" }
func (t *WriteFileTool) Description() string {
	return `Write content to a file. Overwrites by default; pass append=true to add to the end (creating the file when missing). ` +
		`One call carries at most 16 KiB of content — for a larger file, write the first chunk, then continue with append=true, one chunk per call.`
}
func (t *WriteFileTool) Parameters() provider.ParameterSchema {
	return provider.ParameterSchema{
		Type: "object",
		Properties: map[string]provider.PropertySchema{
			"path":    {Type: "string", Description: "Path to the file"},
			"content": {Type: "string", Description: "Content to write"},
			"append":  {Type: "boolean", Description: "Append to the file instead of overwriting it (default false; creates the file if missing)"},
		},
		Required: []string{"path", "content"},
	}
}
func (t *WriteFileTool) Execute(ctx context.Context, params map[string]any) (string, error) {
	path, ok := params["path"].(string)
	if !ok {
		return "", fmt.Errorf("missing path")
	}
	content, ok := params["content"].(string)
	if !ok {
		return "", fmt.Errorf("missing content")
	}

	safe, err := safePathIn(t.Workspace, t.SharedWorkspace, path)
	if err != nil {
		return "", err
	}

	// The limit exists so a body too long for one response is written in pieces
	// instead of being re-emitted whole after the upstream cuts it — see
	// maxInlineWriteBytes.
	if len(content) > maxInlineWriteBytes {
		return "", fmt.Errorf("content is %d bytes, past the %d-byte limit for one call — write the first chunk, then continue with append=true (each call ≤ %d bytes)",
			len(content), maxInlineWriteBytes, maxInlineWriteBytes)
	}

	appendMode := boolParam(params, "append")
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if appendMode {
		flags = os.O_WRONLY | os.O_CREATE | os.O_APPEND
	}
	f, err := os.OpenFile(safe, flags, 0644)
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return "", err
	}
	info, statErr := f.Stat()
	if err := f.Close(); err != nil {
		return "", err
	}
	verb := "File written"
	if appendMode {
		verb = "Appended"
	}
	if statErr != nil {
		return fmt.Sprintf("%s %d bytes", verb, len(content)), nil
	}
	return fmt.Sprintf("%s %d bytes (file now %d bytes)", verb, len(content), info.Size()), nil
}

// EditFileTool performs a find-and-replace edit on a file.
type EditFileTool struct {
	Workspace       string
	SharedWorkspace string
}

func (t *EditFileTool) Name() string { return "edit" }
func (t *EditFileTool) Description() string {
	return `Edit a file by replacing an exact text match. The old_text must appear exactly once in the file — ` +
		`keep it to the shortest unique fragment. One call carries at most 16 KiB in old_text and new_text; ` +
		`a larger rewrite belongs in write (first chunk) followed by append=true.`
}
func (t *EditFileTool) Parameters() provider.ParameterSchema {
	return provider.ParameterSchema{
		Type: "object",
		Properties: map[string]provider.PropertySchema{
			"path":     {Type: "string", Description: "Path to the file to edit"},
			"old_text": {Type: "string", Description: "Exact text to find (must appear exactly once)"},
			"new_text": {Type: "string", Description: "Text to replace with (empty string to delete)"},
		},
		Required: []string{"path", "old_text", "new_text"},
	}
}
func (t *EditFileTool) Execute(ctx context.Context, params map[string]any) (string, error) {
	path, ok := params["path"].(string)
	if !ok {
		return "", fmt.Errorf("missing path")
	}
	oldText, ok := params["old_text"].(string)
	if !ok {
		return "", fmt.Errorf("missing old_text")
	}
	newText, _ := params["new_text"].(string)

	safe, err := safePathIn(t.Workspace, t.SharedWorkspace, path)
	if err != nil {
		return "", err
	}

	// A match or a replacement past the inline limit is a body that would have
	// to travel in one response — the shape the upstream cuts in half. Name the
	// smaller route instead.
	if len(oldText) > maxInlineWriteBytes {
		return "", fmt.Errorf("old_text is %d bytes, past the %d-byte limit for one call — match the shortest unique fragment instead",
			len(oldText), maxInlineWriteBytes)
	}
	if len(newText) > maxInlineWriteBytes {
		return "", fmt.Errorf("new_text is %d bytes, past the %d-byte limit for one call — rewrite the section with write (first chunk) followed by append=true",
			len(newText), maxInlineWriteBytes)
	}

	// An edit loads the file to count and replace the match, so a file past the
	// limit is refused with a route that does not need the whole body.
	if info, err := os.Stat(safe); err == nil && info.Size() > editMaxBytes {
		return "", fmt.Errorf("file is %d bytes, past the %d-byte edit limit — locate the text with grep/read and rewrite the section with write",
			info.Size(), int64(editMaxBytes))
	}

	data, err := os.ReadFile(safe)
	if err != nil {
		return "", err
	}
	content := string(data)

	count := strings.Count(content, oldText)
	if count == 0 {
		return "", fmt.Errorf("old_text not found in file")
	}
	if count > 1 {
		return "", fmt.Errorf("old_text appears %d times — provide more context to make a unique match", count)
	}

	newContent := strings.Replace(content, oldText, newText, 1)
	if err := os.WriteFile(safe, []byte(newContent), 0644); err != nil {
		return "", err
	}
	return "File edited successfully", nil
}

// ListDirTool lists files in a directory.
type ListDirTool struct {
	Workspace       string
	SharedWorkspace string
}

func (t *ListDirTool) Name() string        { return "list" }
func (t *ListDirTool) Description() string { return "List files in a directory." }
func (t *ListDirTool) Parameters() provider.ParameterSchema {
	return provider.ParameterSchema{
		Type: "object",
		Properties: map[string]provider.PropertySchema{
			"path": {Type: "string", Description: "Directory path (default: current/workspace)"},
		},
	}
}
func (t *ListDirTool) Execute(ctx context.Context, params map[string]any) (string, error) {
	path, _ := params["path"].(string)
	if path == "" {
		path = "."
	}

	safe, err := safePathIn(t.Workspace, t.SharedWorkspace, path)
	if err != nil {
		return "", err
	}

	entries, err := os.ReadDir(safe)
	if err != nil {
		return "", err
	}

	var names []string
	for _, e := range entries {
		suffix := ""
		if e.IsDir() {
			suffix = "/"
		}
		names = append(names, e.Name()+suffix)
	}
	return strings.Join(names, "\n"), nil
}

// AppendNoteTool appends a timestamped note to today's daily memory file.
type AppendNoteTool struct {
	MemoryDir string
	// nowFunc allows overriding time.Now for testing.
	nowFunc func() time.Time
}

func (t *AppendNoteTool) now() time.Time {
	if t.nowFunc != nil {
		return t.nowFunc()
	}
	return time.Now()
}

func (t *AppendNoteTool) Name() string { return "append_note" }
func (t *AppendNoteTool) Description() string {
	return "Append a timestamped note to today's daily memory file ({memoryDir}/YYYY-MM-DD.md)."
}
func (t *AppendNoteTool) Parameters() provider.ParameterSchema {
	return provider.ParameterSchema{
		Type: "object",
		Properties: map[string]provider.PropertySchema{
			"content": {Type: "string", Description: "The note content to append"},
		},
		Required: []string{"content"},
	}
}
func (t *AppendNoteTool) Execute(ctx context.Context, params map[string]any) (string, error) {
	content, ok := params["content"].(string)
	if !ok {
		return "", fmt.Errorf("missing content")
	}

	now := t.now()
	date := now.Format("2006-01-02")
	ts := now.Format("15:04")

	if err := os.MkdirAll(t.MemoryDir, 0755); err != nil {
		return "", fmt.Errorf("create memory directory: %w", err)
	}

	filePath := filepath.Join(t.MemoryDir, date+".md")

	// Create with header if file doesn't exist
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		header := fmt.Sprintf("# %s\n", date)
		if err := os.WriteFile(filePath, []byte(header), 0644); err != nil {
			return "", err
		}
	}

	f, err := os.OpenFile(filePath, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return "", err
	}
	defer f.Close()

	entry := fmt.Sprintf("\n- [%s] %s\n", ts, content)
	if _, err := f.WriteString(entry); err != nil {
		return "", err
	}

	return fmt.Sprintf("Note appended to %s", date+".md"), nil
}
