package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"gitee.com/kobeyoung81/nanosec/internal/config"
	"gitee.com/kobeyoung81/nanosec/internal/skill"
)

// ContextBuilder assembles the system prompt for the agent.
type ContextBuilder struct {
	workspaceDir     string
	identityFile     string
	bootstrapDir     string
	memoryDir        string
	skills           *skill.Registry
	memoryRecentDays int
	channelCaps      map[string]config.ChannelCapabilities
}

// NewContextBuilder creates a ContextBuilder.
func NewContextBuilder(workspaceDir, identityFile, bootstrapDir, memoryDir string, skills *skill.Registry, memoryRecentDays int) *ContextBuilder {
	if memoryRecentDays <= 0 {
		memoryRecentDays = 3
	}
	return &ContextBuilder{
		workspaceDir:     workspaceDir,
		identityFile:     identityFile,
		bootstrapDir:     bootstrapDir,
		memoryDir:        memoryDir,
		skills:           skills,
		memoryRecentDays: memoryRecentDays,
	}
}

// SetMemoryRecentDays sets how many recent daily notes are loaded into context.
func (b *ContextBuilder) SetMemoryRecentDays(days int) {
	if days <= 0 {
		days = 3
	}
	b.memoryRecentDays = days
}

// AppendMemory appends a block of text to MEMORY.md, creating the memory
// directory and file as needed. It is used to persist pre-compaction memory
// flushes promptly so extracted facts are never lost.
func (b *ContextBuilder) AppendMemory(content string) error {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}
	if err := os.MkdirAll(b.memoryDir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(b.memoryDir, "MEMORY.md"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString("\n\n" + content + "\n"); err != nil {
		return err
	}
	return nil
}

// SetChannelCapabilities supplies the per-channel capability map the agent
// uses to adapt its output (markdown, length limits, interactive elements).
func (b *ContextBuilder) SetChannelCapabilities(caps map[string]config.ChannelCapabilities) {
	b.channelCaps = caps
}

// BuildSystemPrompt renders head 1: the static, mode-independent part of the
// request head — the active definition's own prompt (for a session agent the bus
// folds in the *shared* head prompt, so build and plan get identical bytes) plus
// the per-channel notes.
//
// Everything that can change while a conversation runs (AGENT.md, bootstrap, the
// memory files, the skills index) is deliberately absent: it is delivered as its
// own frozen head segment plus tail updates (see HeadSegments). Rewriting any of
// it inside head 1 would invalidate the whole cached conversation.
func (b *ContextBuilder) BuildSystemPrompt(channel, chatID string, prompt string) string {
	var parts []string

	if typePrompt := strings.TrimSpace(prompt); typePrompt != "" {
		parts = append(parts, typePrompt)
	}

	if channel != "" && chatID != "" {
		parts = append(parts, fmt.Sprintf("# Current Session\nChannel: %s\nChat ID: %s", channel, chatID))
	}

	if cap, ok := b.channelCaps[channel]; ok {
		parts = append(parts, b.channelCapabilitiesSection(channel, cap))
	}

	return strings.Join(parts, "\n\n---\n\n")
}

// HeadSegment is one session-file block of the request head. Each file is its own
// system message so that "this file changed" can be appended at the tail as an
// update instead of rewriting the head — which would invalidate the cached
// conversation. The marker line renders as "[MEMORY.md][a1b2c3d4]"; the tail
// rescan finds the newest marker per file to decide whether an update is due.
type HeadSegment struct {
	// Name is the marker name, e.g. "AGENT.md" or "memory/2026-10-16.md".
	Name string
	// Text is what is rendered under the marker. For AGENT.md it carries the
	// runtime details too (those are not hashed: a date change is not an edit).
	Text string
	// Hash is the short content hash of the underlying file, as it appears in the
	// marker and as the tail update reports it.
	Hash string
	// AppendOnly marks files that only ever grow (MEMORY.md, the daily notes), so
	// their tail update can carry just the appended suffix.
	AppendOnly bool
}

// HeadMarker renders the marker line a head segment or a tail update carries.
func HeadMarker(name, hash string) string { return "[" + name + "][" + hash + "]" }

// TextHash is the short content hash used in head markers: truncated sha256 over
// the file's text. A comparison ever only happens between values this code
// produced.
func TextHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])[:16]
}

// newSeg builds one segment from a file's text.
func newSeg(name, text string, appendOnly bool) HeadSegment {
	return HeadSegment{Name: name, Text: text, Hash: TextHash(text), AppendOnly: appendOnly}
}

// HeadSegments reads the session files in their fixed order: AGENT.md (identity,
// with the runtime block), the bootstrap files, MEMORY.md, the recent daily
// notes, the always-on skills and the skills index. The order is part of the
// prompt contract — head 1 documents it and the tail updates mirror it.
func (b *ContextBuilder) HeadSegments() []HeadSegment {
	var segs []HeadSegment

	segs = append(segs, HeadSegment{
		Name: "AGENT.md",
		Text: b.identity(),
		Hash: TextHash(b.agentFileText()),
	})

	for _, name := range []string{"SOUL.md", "USER.md", "TOOLS.md"} {
		if data, err := os.ReadFile(filepath.Join(b.bootstrapDir, name)); err == nil {
			if text := strings.TrimSpace(string(data)); text != "" {
				segs = append(segs, newSeg(name, text, false))
			}
		}
	}

	if data, err := os.ReadFile(filepath.Join(b.memoryDir, "MEMORY.md")); err == nil {
		if text := strings.TrimSpace(string(data)); text != "" {
			segs = append(segs, newSeg("MEMORY.md", text, true))
		}
	}

	now := time.Now()
	seen := make(map[string]bool)
	for i := 0; i < b.memoryRecentDays; i++ {
		dateStr := now.AddDate(0, 0, -i).Format("2006-01-02")
		if seen[dateStr] {
			continue
		}
		seen[dateStr] = true
		data, err := os.ReadFile(filepath.Join(b.memoryDir, dateStr+".md"))
		if err != nil {
			continue
		}
		if text := strings.TrimSpace(string(data)); text != "" {
			segs = append(segs, newSeg("memory/"+dateStr+".md", text, true))
		}
	}

	if b.skills != nil {
		if alwaysNames := b.skills.GetAlwaysSkills(); len(alwaysNames) > 0 {
			if content := b.skills.LoadForContext(alwaysNames); content != "" {
				segs = append(segs, newSeg("active-skills", "# Active Skills\n\n"+content, false))
			}
		}
		if summary := b.skills.BuildSummary(); summary != "" {
			segs = append(segs, newSeg("skills-index", "# Skills\n\nThe following skills extend your capabilities. "+
				"Read a skill's SKILL.md file using the read tool to use it.\n\n"+summary, false))
		}
	}

	return segs
}

// agentFileText reads AGENT.md (the identity file) on its own, without the
// runtime block that accompanies it in the prompt: the hash and the tail updates
// are about the file's text.
func (b *ContextBuilder) agentFileText() string {
	content, err := os.ReadFile(b.identityFile)
	if err != nil {
		slog.Warn("context: failed to load identity file", "path", b.identityFile, "error", err)
		return "# nanosec\n\nYou are nanosec, a helpful AI assistant."
	}
	return strings.TrimSpace(string(content))
}

// channelCapabilitiesSection tells the agent how to format output for the
// current channel so it does not emit unsupported constructs.
func (b *ContextBuilder) channelCapabilitiesSection(channel string, cap config.ChannelCapabilities) string {
	features := []string{}
	if cap.Streaming {
		features = append(features, "replies stream in real time")
	}
	if cap.Markdown {
		features = append(features, "markdown formatting is rendered")
	} else {
		features = append(features, "plain text only (no markdown)")
	}
	if cap.RichMedia {
		features = append(features, "media attachments supported")
	}
	if cap.Interactive {
		features = append(features, "interactive elements supported")
	}
	if cap.AsyncReplies {
		features = append(features, "replies are asynchronous")
	}
	if cap.SessionResetSupported {
		features = append(features, "user can start a fresh session (/new)")
	}
	lengthNote := ""
	if cap.MaxContentLen > 0 {
		lengthNote = fmt.Sprintf(" Keep replies under %d characters.", cap.MaxContentLen)
	}
	return fmt.Sprintf("# Channel Capabilities (%s)\n%s.%s", channel, strings.Join(features, "; "), lengthNote)
}

// identity builds the core identity section from AGENT.md and appends runtime details.
func (b *ContextBuilder) identity() string {
	content, err := os.ReadFile(b.identityFile)
	if err != nil {
		slog.Warn("context: failed to load identity file", "path", b.identityFile, "error", err)
		content = []byte("# nanosec\n\nYou are nanosec, a helpful AI assistant.")
	}

	return strings.TrimSpace(string(content)) + "\n\n" + b.runtimeDetails()
}

func (b *ContextBuilder) runtimeDetails() string {
	// Date granularity only: the system prompt is the request prefix, so a
	// value that changes every minute would invalidate the provider's prompt
	// cache on every turn. Date+weekday keeps it stable within a day; exact
	// wall-clock time is available to the agent via the bash `date` tool.
	now := time.Now().Format("2006-01-02 (Monday)")
	goos := runtime.GOOS
	arch := runtime.GOARCH

	return fmt.Sprintf(`## Current Time
%s

## Runtime
%s/%s, Go agent

## Workspace
%s

## Memory
Long-term: %s
Daily notes: %s
`,
		now,
		goos,
		arch,
		b.workspaceDir,
		filepath.Join(b.memoryDir, "MEMORY.md"),
		filepath.Join(b.memoryDir, "YYYY-MM-DD.md"),
	)
}

// loadBootstrapFiles loads optional markdown context files from the bootstrap directory.
func (b *ContextBuilder) loadBootstrapFiles() string {
	files := []string{"SOUL.md", "USER.md", "TOOLS.md"}
	var parts []string

	for _, name := range files {
		path := filepath.Join(b.bootstrapDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		parts = append(parts, "## "+name+"\n\n"+string(data))
	}

	return strings.Join(parts, "\n\n")
}

// loadMemory loads MEMORY.md and recent daily notes from the configured memory directory.
func (b *ContextBuilder) loadMemory() string {
	var parts []string

	if data, err := os.ReadFile(filepath.Join(b.memoryDir, "MEMORY.md")); err == nil {
		parts = append(parts, "## Long-term Memory\n\n"+string(data))
	}

	now := time.Now()
	seen := make(map[string]bool)
	for i := 0; i < b.memoryRecentDays; i++ {
		day := now.AddDate(0, 0, -i)
		dateStr := day.Format("2006-01-02")
		if seen[dateStr] {
			continue
		}
		seen[dateStr] = true

		data, err := os.ReadFile(filepath.Join(b.memoryDir, dateStr+".md"))
		if err != nil {
			continue
		}

		label := "Today's Notes"
		if i > 0 {
			label = dateStr + " Notes"
		}
		parts = append(parts, "## "+label+"\n\n"+string(data))
	}

	return strings.Join(parts, "\n\n")
}
