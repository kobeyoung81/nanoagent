package tool

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"
)

func readFile(t *testing.T, dir, name string, params map[string]any) string {
	t.Helper()
	tool := &ReadFileTool{Workspace: dir}
	p := map[string]any{"path": name}
	for k, v := range params {
		p[k] = v
	}
	out, err := tool.Execute(context.Background(), p)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return out
}

func writeFixture(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		t.Fatalf("write fixture %s: %v", name, err)
	}
}

// TestRead_LargeTextIsWindowed pins the paging contract: a big file comes back
// as one page plus a footer naming the offset that continues it.
func TestRead_LargeTextIsWindowed(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	for i := 1; i <= 20000; i++ {
		fmt.Fprintf(&b, "line %05d %s\n", i, strings.Repeat("x", 40))
	}
	writeFixture(t, dir, "big.txt", []byte(b.String()))

	out := readFile(t, dir, "big.txt", nil)
	if len(out) > 14000 {
		t.Fatalf("window should stay within the budget, got %d bytes", len(out))
	}
	if !strings.Contains(out, "[window: lines 1-") || !strings.Contains(out, "of 20000") {
		t.Fatalf("missing window footer, got tail: %q", tailOf(out, 200))
	}
	if !strings.Contains(out, "continue with offset=") {
		t.Fatalf("footer must say how to continue, got: %q", tailOf(out, 200))
	}
	if !strings.Contains(out, "line 00001 ") {
		t.Error("window should start at the first line")
	}

	// The footer's offset must resume exactly where the window stopped.
	next := intParam(map[string]any{}, "x")
	_ = next
	footerIdx := strings.LastIndex(out, "continue with offset=")
	rest := out[footerIdx+len("continue with offset="):]
	var resume int
	if _, err := fmt.Sscanf(rest, "%d", &resume); err != nil {
		t.Fatalf("cannot parse the resume offset from %q: %v", rest, err)
	}
	second := readFile(t, dir, "big.txt", map[string]any{"offset": resume})
	if second == out {
		t.Fatal("resuming at the footer offset returned the same page")
	}
	if !strings.Contains(second, fmt.Sprintf("line %05d ", resume)) {
		t.Errorf("resumed page should start at line %d, got: %q", resume, tailOf(second, 120))
	}

	// The end of the file is announced instead of a next offset.
	last := readFile(t, dir, "big.txt", map[string]any{"offset": 19999})
	if !strings.Contains(last, "[end of file: lines 19999-20000 of 20000") {
		t.Errorf("expected an end-of-file footer, got: %q", tailOf(last, 200))
	}

	// A window past the end says so rather than returning nothing.
	past := readFile(t, dir, "big.txt", map[string]any{"offset": 999999})
	if !strings.Contains(past, "past the end") {
		t.Errorf("expected a past-the-end notice, got: %q", past)
	}
}

func TestRead_SmallFileIsUnchanged(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "small.txt", []byte("hello world"))
	if got := readFile(t, dir, "small.txt", nil); got != "hello world" {
		t.Errorf("small read = %q, want the file verbatim", got)
	}
	writeFixture(t, dir, "trailing.txt", []byte("with newline\n"))
	if got := readFile(t, dir, "trailing.txt", nil); got != "with newline\n" {
		t.Errorf("small read = %q, want the file verbatim", got)
	}
	writeFixture(t, dir, "empty.txt", nil)
	if got := readFile(t, dir, "empty.txt", nil); got != "" {
		t.Errorf("empty read = %q, want empty", got)
	}
}

// TestRead_BinaryIsRefusedNotDumped pins the type check: binary content never
// becomes model context, and an extension that disagrees with the content is
// reported.
func TestRead_BinaryIsRefusedNotDumped(t *testing.T) {
	dir := t.TempDir()
	png := append([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}, []byte(strings.Repeat("\x00\x01\x02\x03", 400))...)
	writeFixture(t, dir, "image.png", png)
	writeFixture(t, dir, "notes.md", png)

	real := readFile(t, dir, "image.png", nil)
	if !strings.Contains(real, "not read —") || !strings.Contains(real, "image/png") {
		t.Errorf("expected a content-type report, got: %q", real)
	}
	if strings.Contains(real, "\x00") {
		t.Error("the refusal must not carry the file's bytes")
	}

	mismatch := readFile(t, dir, "notes.md", nil)
	if !strings.Contains(mismatch, "not read —") {
		t.Fatalf("a binary body behind .md must be refused, got: %q", mismatch)
	}
	if !strings.Contains(mismatch, "extension .md says text") {
		t.Errorf("expected a mismatch note, got: %q", mismatch)
	}

	// Text behind a binary-looking extension is returned, with a note.
	writeFixture(t, dir, "fake.png", []byte("this is plain text\n"))
	textOut := readFile(t, dir, "fake.png", nil)
	if !strings.Contains(textOut, "this is plain text") {
		t.Errorf("text behind .png should be readable, got: %q", textOut)
	}
	if !strings.Contains(textOut, "extension .png says binary") {
		t.Errorf("expected a mismatch note, got: %q", textOut)
	}
}

// TestRead_UTF16IsDecoded pins the encoding path.
func TestRead_UTF16IsDecoded(t *testing.T) {
	dir := t.TempDir()
	text := "你好，世界\n第二行\n"
	units := utf16.Encode([]rune(text))
	buf := []byte{0xff, 0xfe}
	for _, u := range units {
		buf = binary.LittleEndian.AppendUint16(buf, u)
	}
	writeFixture(t, dir, "cn.txt", buf)

	out := readFile(t, dir, "cn.txt", nil)
	if !strings.Contains(out, "你好，世界") || !strings.Contains(out, "第二行") {
		t.Fatalf("utf-16 text was not decoded, got: %q", out)
	}
	if !strings.Contains(out, "decoded from utf-16le") {
		t.Errorf("expected an encoding note, got: %q", out)
	}
}

// TestRead_CJKIsNotBinary pins the counter-example to the NUL/UTF-8 checks: a
// large valid UTF-8 Chinese file must read normally.
func TestRead_CJKIsNotBinary(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	for i := 0; i < 4000; i++ {
		fmt.Fprintf(&b, "第 %d 行：这是一段中文文本，用来验证编码判定。\n", i)
	}
	writeFixture(t, dir, "cn.md", []byte(b.String()))

	out := readFile(t, dir, "cn.md", nil)
	if strings.Contains(out, "not read —") {
		t.Fatalf("valid UTF-8 CJK text was refused: %q", out)
	}
	if !strings.Contains(out, "这是一段中文文本") {
		t.Error("expected Chinese content in the window")
	}
	if !strings.Contains(out, "continue with offset=") {
		t.Errorf("expected a paging footer, got: %q", tailOf(out, 160))
	}
}

// TestRead_NulBeyondTheSampleIsReported pins the case the head sample cannot
// see: the window itself carries NUL bytes, so the footer says so.
func TestRead_NulBeyondTheSampleIsReported(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	b.WriteString("start\n")
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&b, "line %03d %s\n", i, strings.Repeat("y", 40))
	}
	b.WriteString("has\x00nul\n")
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&b, "tail %03d %s\n", i, strings.Repeat("z", 40))
	}
	writeFixture(t, dir, "sneaky.txt", []byte(b.String()))

	out := readFile(t, dir, "sneaky.txt", nil)
	if strings.Contains(out, "not read —") {
		t.Fatalf("the head is text, so the file should still be readable: %q", tailOf(out, 200))
	}
	if !strings.Contains(out, "contains NUL bytes") {
		t.Errorf("expected a NUL notice in the footer, got: %q", tailOf(out, 260))
	}
}

// TestRead_LongLineIsTruncated pins the minified-bundle case.
func TestRead_LongLineIsTruncated(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "bundle.js", []byte(strings.Repeat("a", 50000)+"\n"))

	out := readFile(t, dir, "bundle.js", nil)
	if !strings.Contains(out, "a line was truncated at") {
		t.Errorf("expected a long-line notice, got: %q", tailOf(out, 200))
	}
	if len(out) > 14000 {
		t.Errorf("window %d bytes exceeds the budget", len(out))
	}
}

// TestEdit_RefusesHugeFile pins the edit size guard: the honest answer is a
// route that does not load the whole file, not a silent multi-gigabyte read.
func TestEdit_RefusesHugeFile(t *testing.T) {
	dir := t.TempDir()
	big := make([]byte, editMaxBytes+1)
	writeFixture(t, dir, "huge.txt", big)

	tool := &EditFileTool{Workspace: dir}
	_, err := tool.Execute(context.Background(), map[string]any{
		"path": "huge.txt", "old_text": "a", "new_text": "b",
	})
	if err == nil {
		t.Fatal("expected a size refusal")
	}
	if !strings.Contains(err.Error(), "past the") || !strings.Contains(err.Error(), "edit limit") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestRead_WindowRespectsInjectedBudget pins that the tool never returns more
// than the budget main.go injects.
func TestRead_WindowRespectsInjectedBudget(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&b, "line %05d %s\n", i, strings.Repeat("q", 60))
	}
	writeFixture(t, dir, "budget.txt", []byte(b.String()))

	tool := &ReadFileTool{Workspace: dir, MaxBytes: 2000}
	out, err := tool.Execute(context.Background(), map[string]any{"path": "budget.txt"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(out) > 2600 {
		t.Errorf("window %d bytes exceeds the injected 2000-byte budget", len(out))
	}
}

// tailOf returns the last n bytes of s, for readable failure messages.
func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
