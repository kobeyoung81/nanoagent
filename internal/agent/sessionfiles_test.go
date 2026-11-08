package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitee.com/kobeyoung81/nanosec/internal/provider"
	"gitee.com/kobeyoung81/nanosec/internal/session"
)

// newSessionFilesAgent builds a bare agent (no bus, no provider) whose only job is
// to read session files from a temp home, plus a session to inject into.
func newSessionFilesAgent(t *testing.T) (*Agent, *session.Session) {
	t.Helper()
	ws := t.TempDir()
	agentDir := filepath.Join(ws, "agent")
	bootstrap := filepath.Join(agentDir, "bootstrap")
	memory := filepath.Join(agentDir, "memory")
	for _, dir := range []string{agentDir, bootstrap, memory} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(agentDir, "AGENT.md"), "# Identity\n\nI am a test agent.\n")
	writeFile(t, filepath.Join(bootstrap, "SOUL.md"), "Direct answers only.\n")
	writeFile(t, filepath.Join(memory, "MEMORY.md"), "- uses Go\n")

	cb := NewContextBuilder(ws, filepath.Join(agentDir, "AGENT.md"), bootstrap, memory, nil, 1)
	return &Agent{ctxBuilder: cb}, session.New("web:c1")
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func renderHead(msgs []provider.Message) string {
	parts := make([]string, 0, len(msgs))
	for _, m := range msgs {
		parts = append(parts, m.Role+":"+m.Content)
	}
	return strings.Join(parts, "\n---\n")
}

func countRows(msgs []provider.Message, role, contains string) int {
	n := 0
	for _, m := range msgs {
		if m.Role == role && strings.Contains(m.Content, contains) {
			n++
		}
	}
	return n
}

// TestHeadSnapshotIsFrozen pins the property the prompt cache rests on: within a
// session segment the head — head 1 plus one system message per session file — is
// byte-stable, whatever happens to the files underneath.
func TestHeadSnapshotIsFrozen(t *testing.T) {
	a, sess := newSessionFilesAgent(t)
	first := renderHead(a.headSnapshotFor(sess).messages)
	if first == "" {
		t.Fatal("expected a head with the session files")
	}
	if !strings.Contains(first, "[AGENT.md][") || !strings.Contains(first, "[MEMORY.md][") {
		t.Fatalf("head must carry a marker per file, got %q", first)
	}

	// A file change must not rewrite the head of a live segment.
	writeFile(t, filepath.Join(t.TempDir(), "ignored.md"), "x")
	sess.AddMessage(provider.Message{Role: "user", Content: "hello"})
	if second := renderHead(a.headSnapshotFor(sess).messages); second != first {
		t.Errorf("head changed after unrelated activity:\n%s\nvs\n%s", first, second)
	}
}

// TestFileChangeAppendsTailUpdate is the core of the design: a file that moved
// after the snapshot is reported once, at the tail, and never again.
func TestFileChangeAppendsTailUpdate(t *testing.T) {
	a, sess := newSessionFilesAgent(t)
	spec := ResolvedAgent{ID: "build"}

	// First turn: the head already carries the files, so only the mode note lands.
	a.injectSessionState(sess, spec)
	if n := countRows(sess.Messages, "system", "[更新"); n != 0 {
		t.Fatalf("first turn must not report updates, got %d", n)
	}
	if n := countRows(sess.Messages, "system", "【模式状态】"); n != 1 {
		t.Fatalf("first turn must append exactly one mode note, got %d", n)
	}

	// MEMORY.md grows: the next turn appends its suffix under a fresh marker.
	memPath := filepath.Join(a.ctxBuilder.memoryDir, "MEMORY.md")
	writeFile(t, memPath, "- uses Go\n- prefers Postgres\n")
	a.injectSessionState(sess, spec)

	updates := 0
	var update string
	for _, m := range sess.Messages {
		if m.Role == "system" && strings.Contains(m.Content, "[更新MEMORY.md]") {
			updates++
			update = m.Content
		}
	}
	if updates != 1 {
		t.Fatalf("expected exactly one MEMORY.md update, got %d", updates)
	}
	if !strings.Contains(update, "prefers Postgres") || !strings.Contains(update, "[MEMORY.md][") {
		t.Errorf("update must carry the new content and its marker, got %q", update)
	}
	if strings.Contains(update, "- uses Go") {
		t.Errorf("an append-only file must report just the suffix, got %q", update)
	}

	// A turn with no further change must not repeat the update.
	a.injectSessionState(sess, spec)
	if n := countRows(sess.Messages, "system", "[更新MEMORY.md]"); n != 1 {
		t.Errorf("the update must not be appended twice, got %d", n)
	}
	if n := countRows(sess.Messages, "system", "【模式状态】"); n != 3 {
		t.Errorf("the mode note must be appended on every turn, got %d", n)
	}
}

// TestModeNoteUsesConfiguredTemplate covers the configurable mode-switch prompt:
// the bus folds it into the definition settings with {mode}/{plan_path} holes.
func TestModeNoteUsesConfiguredTemplate(t *testing.T) {
	a, sess := newSessionFilesAgent(t)
	sess.SetMeta(lastPlanPathMetaKey, "/ws/plans/x.md")
	spec := ResolvedAgent{ID: "plan", Settings: map[string]string{
		"mode_note": "Now in {mode} mode; the plan is at {plan_path}.",
	}}
	got := a.modeNoteBody(sess, spec)
	if !strings.Contains(got, "Now in plan mode") || !strings.Contains(got, "/ws/plans/x.md") {
		t.Errorf("mode note = %q, want the template with both holes filled", got)
	}

	// With no plan artefact the line that talks about one is dropped, not left as a
	// placeholder.
	sess.SetMeta(lastPlanPathMetaKey, "")
	bare := a.modeNoteBody(sess, spec)
	if strings.Contains(bare, "{plan_path}") {
		t.Errorf("unfilled placeholder leaked into the mode note: %q", bare)
	}

	// Without a configured template the built-in line is used.
	sess.SetMeta(lastPlanPathMetaKey, "")
	builtin := a.modeNoteBody(sess, ResolvedAgent{ID: "build"})
	if !strings.Contains(builtin, "Mode: build.") {
		t.Errorf("built-in mode note = %q", builtin)
	}
}

// TestModeNoteCarriesTheLanguage pins where the language is stated: inside the
// per-turn mode note (the last system message before the question), so the model
// reads it immediately before answering. One row, not two.
func TestModeNoteCarriesTheLanguage(t *testing.T) {
	a, sess := newSessionFilesAgent(t)
	a.config.Language = "zh-CN"
	a.injectSessionState(sess, ResolvedAgent{ID: "build"})

	last := sess.Messages[len(sess.Messages)-1]
	if last.Role != "system" || !strings.Contains(last.Content, "【模式状态】") {
		t.Fatalf("the last row is not the mode note: %+v", last)
	}
	if !strings.Contains(last.Content, "【Language】") || !strings.Contains(last.Content, "Chinese (中文)") {
		t.Fatalf("the language line is missing or unnamed: %q", last.Content)
	}
	if n := countRows(sess.Messages, "system", "【Language】"); n != 1 {
		t.Fatalf("expected exactly one language row, got %d", n)
	}

	// No resolved language: the note is exactly what it was before.
	b, bare := newSessionFilesAgent(t)
	b.injectSessionState(bare, ResolvedAgent{ID: "build"})
	if n := countRows(bare.Messages, "system", "【Language】"); n != 0 {
		t.Fatalf("an unset language must not add a line, got %d", n)
	}
}
