package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitee.com/kobeyoung81/nanosec/internal/config"
)

// headText renders head 1 plus the per-file segments — what the model actually
// receives as its context sections. The session files moved out of the single
// system prompt into one system message per file (see HeadSegments), so the
// content assertions go through this helper.
func headText(b *ContextBuilder) string { return headTextFor(b, "", "") }

// headTextFor is headText with an explicit channel, for the channel-capability
// assertions.
func headTextFor(b *ContextBuilder, channel, chatID string) string {
	var parts []string
	if h1 := b.BuildSystemPrompt(channel, chatID, ""); h1 != "" {
		parts = append(parts, h1)
	}
	for _, seg := range b.HeadSegments() {
		parts = append(parts, HeadMarker(seg.Name, seg.Hash)+"\n"+seg.Text)
	}
	return strings.Join(parts, "\n\n")
}

func newTestContextBuilder(t *testing.T, workspaceDir string) *ContextBuilder {
	t.Helper()

	identityPath := filepath.Join(workspaceDir, "AGENT.md")
	if err := os.WriteFile(identityPath, []byte("# nanosec\n\nCore identity"), 0o644); err != nil {
		t.Fatalf("write AGENT.md: %v", err)
	}

	bootstrapDir := filepath.Join(workspaceDir, "bootstrap")
	if err := os.MkdirAll(bootstrapDir, 0o755); err != nil {
		t.Fatalf("create bootstrap dir: %v", err)
	}

	return NewContextBuilder(
		workspaceDir,
		identityPath,
		bootstrapDir,
		filepath.Join(workspaceDir, "memory"),
		nil,
		3,
	)
}

func TestContextBuilder_MemoryLoading(t *testing.T) {
	tmpDir := t.TempDir()

	memDir := filepath.Join(tmpDir, "memory")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(memDir, "MEMORY.md"), []byte("I prefer Go over Python."), 0o644); err != nil {
		t.Fatal(err)
	}

	cb := newTestContextBuilder(t, tmpDir)
	prompt := headText(cb)

	if !strings.Contains(prompt, "[MEMORY.md]") {
		t.Error("expected a MEMORY.md head segment")
	}
	if !strings.Contains(prompt, "I prefer Go over Python.") {
		t.Error("expected memory content in prompt")
	}
}

func TestContextBuilder_DailyNotesLoading(t *testing.T) {
	tmpDir := t.TempDir()

	memDir := filepath.Join(tmpDir, "memory")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatal(err)
	}

	today := time.Now().Format("2006-01-02")
	if err := os.WriteFile(filepath.Join(memDir, today+".md"), []byte("- [09:00] standup done"), 0o644); err != nil {
		t.Fatal(err)
	}

	cb := newTestContextBuilder(t, tmpDir)
	prompt := headText(cb)

	if !strings.Contains(prompt, "[memory/") {
		t.Error("expected a daily-note head segment")
	}
	if !strings.Contains(prompt, "standup done") {
		t.Error("expected daily notes content in prompt")
	}
}

func TestContextBuilder_NoMemoryFiles(t *testing.T) {
	tmpDir := t.TempDir()

	cb := newTestContextBuilder(t, tmpDir)
	prompt := headText(cb)

	if strings.Contains(prompt, "## Long-term Memory") {
		t.Error("expected no Long-term Memory section when no memory files exist")
	}
	if strings.Contains(prompt, "## Today's Notes") {
		t.Error("expected no Today's Notes section when no memory files exist")
	}
	if !strings.Contains(prompt, "Core identity") {
		t.Error("expected AGENT.md identity content in prompt")
	}
}

func TestContextBuilder_BothMemoryFiles(t *testing.T) {
	tmpDir := t.TempDir()

	memDir := filepath.Join(tmpDir, "memory")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(memDir, "MEMORY.md"), []byte("long-term info"), 0o644); err != nil {
		t.Fatal(err)
	}

	today := time.Now().Format("2006-01-02")
	if err := os.WriteFile(filepath.Join(memDir, today+".md"), []byte("daily info"), 0o644); err != nil {
		t.Fatal(err)
	}

	cb := newTestContextBuilder(t, tmpDir)
	prompt := headText(cb)

	if !strings.Contains(prompt, "[MEMORY.md]") {
		t.Error("expected a MEMORY.md head segment")
	}
	if !strings.Contains(prompt, "long-term info") {
		t.Error("expected long-term memory content")
	}
	if !strings.Contains(prompt, "[memory/") {
		t.Error("expected a daily-note head segment")
	}
	if !strings.Contains(prompt, "daily info") {
		t.Error("expected daily notes content")
	}
}

func TestContextBuilder_LoadsBootstrapDirOnly(t *testing.T) {
	tmpDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(tmpDir, "AGENTS.md"), []byte("legacy bootstrap"), 0o644); err != nil {
		t.Fatalf("write AGENTS.md: %v", err)
	}

	bootstrapDir := filepath.Join(tmpDir, "bootstrap")
	if err := os.MkdirAll(bootstrapDir, 0o755); err != nil {
		t.Fatalf("create bootstrap dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bootstrapDir, "TOOLS.md"), []byte("tool rules"), 0o644); err != nil {
		t.Fatalf("write TOOLS.md: %v", err)
	}

	cb := newTestContextBuilder(t, tmpDir)
	prompt := headText(cb)

	if !strings.Contains(prompt, "tool rules") {
		t.Error("expected bootstrap directory content in prompt")
	}
	if strings.Contains(prompt, "legacy bootstrap") {
		t.Error("did not expect AGENTS.md from workspace root to be loaded")
	}
}

// TestContextBuilder_DefinitionPromptLeads pins the composition after the base
// prompt was removed: the active definition's own prompt is the first section
// and the only prompt section — each definition carries the norms its mode
// needs instead of sharing a platform base.
func TestContextBuilder_DefinitionPromptLeads(t *testing.T) {
	tmpDir := t.TempDir()

	cb := newTestContextBuilder(t, tmpDir)
	prompt := headText(cb)
	if h1 := cb.BuildSystemPrompt("", "", "DEFINITION_PROMPT_TEXT"); h1 != "" {
		prompt = h1 + "\n\n" + prompt
	}

	if !strings.HasPrefix(prompt, "DEFINITION_PROMPT_TEXT") {
		t.Errorf("expected the definition prompt at the start, got: %.60q…", prompt)
	}
	if !strings.Contains(prompt, "Core identity") {
		t.Error("expected AGENT.md identity content after the definition prompt")
	}
	if got := strings.Count(prompt, "DEFINITION_PROMPT_TEXT"); got != 1 {
		t.Errorf("definition prompt appears %d times, want exactly 1", got)
	}
}

// TestContextBuilder_NoSharedBasePrompt verifies no platform base prompt is
// injected when the definition has none: the identity section leads instead.
func TestContextBuilder_NoSharedBasePrompt(t *testing.T) {
	tmpDir := t.TempDir()

	cb := newTestContextBuilder(t, tmpDir)
	prompt := headText(cb)

	if strings.Contains(prompt, "BOOTSTRAP") {
		t.Error("expected no base prompt section at all")
	}
	if !strings.Contains(prompt, "[AGENT.md]") {
		t.Errorf("the AGENT.md segment must be present, got: %.60q…", prompt)
	}
	if idx := strings.Index(prompt, "# nanosec"); idx != 0 {
		identity := strings.Index(prompt, "[AGENT.md][")
		if identity != 0 {
			t.Errorf("the AGENT.md segment must lead the head, got: %.60q…", prompt)
		}
		_ = idx
	}
}

func TestContextBuilder_ChannelCapabilitiesInjection(t *testing.T) {
	b := NewContextBuilder("/ws", "/ws/AGENT.md", "/ws/bootstrap", "/ws/memory", nil, 3)
	b.SetChannelCapabilities(config.DefaultChannelCapabilities())

	prompt := headTextFor(b, "wechatkf", "kf-1")
	if !strings.Contains(prompt, "Channel Capabilities (wechatkf)") {
		t.Error("expected a channel capabilities section for wechatkf")
	}
	if !strings.Contains(prompt, "plain text only") {
		t.Error("wechatkf should be told plain text only (no markdown)")
	}
	if !strings.Contains(prompt, "under 4000 characters") {
		t.Error("wechatkf should be told the 4000 char length limit")
	}

	webPrompt := headTextFor(b, "web", "chat-1")
	if !strings.Contains(webPrompt, "markdown formatting is rendered") {
		t.Error("web should be told markdown is rendered")
	}

	// Unknown channels get no capability section.
	if strings.Contains(headTextFor(b, "unknown-channel", "c1"), "Channel Capabilities") {
		t.Error("no capability section expected for an unknown/empty channel")
	}
}
