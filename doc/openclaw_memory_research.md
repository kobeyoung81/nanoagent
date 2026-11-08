# OpenClaw Memory & Context Research

**Date**: 2026-03-09
**Status**: Research Complete

---

## 1. What Is OpenClaw?

**OpenClaw** is a real, major, actively developed open-source AI agent framework — and the most directly relevant reference for this project.

| Fact | Detail |
|---|---|
| GitHub | [github.com/openclaw/openclaw](https://github.com/openclaw/openclaw) |
| Stars (March 2026) | **247,000** — one of the fastest-growing GitHub projects ever |
| Language | TypeScript (~500K LOC, 53 config files, 70+ dependencies) |
| License | MIT |
| Docs | [docs.openclaw.ai](https://docs.openclaw.ai) |

### Brief History

- **November 2025**: Released as **Clawdbot** by Peter Steinberger (founder of PSPDFKit)
- **January 27, 2026**: Renamed to **Moltbot** after Anthropic trademark complaints
- **January 30, 2026**: Renamed to **OpenClaw**; reached 60,000 stars in 72 hours
- **February 14, 2026**: Steinberger joined OpenAI; project transferred to independent open-source foundation

### What It Does

OpenClaw is a **self-hosted, local-first AI agent platform**. A persistent process (the *Gateway*) runs on your own hardware, connects to messaging platforms (WhatsApp, Telegram, Signal, Slack, Discord, iMessage), routes messages to an LLM, executes tools, and sends responses back. It is not a chatbot — it actively acts: sends emails, runs code, scrapes websites, calls APIs, and executes scheduled tasks.

---

## 2. The "Claw" Agent Family

OpenClaw's viral growth spawned an entire ecosystem of lighter-weight alternatives:

| Agent | Language | RAM Usage | Key Differentiator |
|---|---|---|---|
| **OpenClaw** | TypeScript | ~1.52 GB | Full-featured, 247K stars, OpenAI-affiliated |
| **NanoClaw** | TypeScript | Minimal | Container-first security, 700 LOC, Anthropic SDK |
| **Nanobot** | Python | ~191 MB | HKUDS research, 9K stars, Raspberry Pi |
| **PicoClaw** | Go | **< 10 MB** | Single binary, RISC-V/embedded, AI self-bootstrapped |
| **ZeroClaw** | Rust | < 5 MB | 194× smaller than OpenClaw, production-grade |
| **IronClaw** | Unknown | Unknown | WebAssembly sandboxed tools, TEE execution |
| **NullClaw** | Zig | ~1 MB | 678 KB binary |

**PicoClaw** (`github.com/sipeed/picoclaw`) is the most architecturally similar to nanosec: a single Go binary, built by a team of Sipeed (RISC-V hardware makers), **95% of its core code written by an AI agent through self-bootstrapping** — directly mirroring what this project is doing. Described as "inspired by nanobot, refactored from the ground up in Go."

---

## 3. OpenClaw Memory Architecture

OpenClaw's core design principle: **Markdown files on disk are the source of truth. If it's not written to a file, it doesn't exist.**

### 3.1 The Four Layers

| Layer | What It Is | Durability |
|---|---|---|
| **Bootstrap Files** | `SOUL.md`, `AGENTS.md`, `USER.md`, `MEMORY.md`, `TOOLS.md` — reloaded from disk at every session start | **Survives all compaction events** |
| **Session Transcript** | JSONL conversation history on disk | Semi-permanent, subject to lossy compaction |
| **LLM Context Window** | Active ~200K token container where all content competes for space | Ephemeral — overflows trigger compaction |
| **Retrieval Index** | Searchable vector + keyword index over memory files | Rebuilt on-demand |

### 3.2 Bootstrap Files (Pinned Permanent Memory)

These are Markdown files in the workspace root. They are loaded at every session start — they survive compaction because they live outside the session transcript.

| File | Purpose |
|---|---|
| `SOUL.md` | Agent personality, communication style, ethical limits |
| `AGENTS.md` | Operational workflows, decision rules, tool conventions |
| `USER.md` | User context, preferences, ongoing projects |
| `MEMORY.md` | Curated long-term facts (kept under 100 lines for best results) |
| `TOOLS.md` | Custom tool descriptions and usage guidelines |

**Critical constraint**: `MEMORY.md` is loaded **only in the main private session**, not in group chats, sub-agents, or cron jobs. This is both a feature (privacy) and a known footgun (agents called from groups cannot access long-term memory without explicit injection).

### 3.3 Two-Tier Memory Within Bootstrap

Within the memory system, OpenClaw distinguishes short-term and long-term:

**Short-term — Daily logs** (`memory/YYYY-MM-DD.md`):
- Append-only session journals
- Today's and yesterday's logs are loaded at session start
- Ephemeral by design: they provide short-term continuity without permanently bloating the long-term file
- Not curated by the agent — raw event log

**Long-term — MEMORY.md**:
- Curated durable facts, learned preferences, cross-session decisions
- Agent actively manages this: adds new facts, removes stale ones
- Recommended to keep under 100 lines
- Loaded only in private sessions

The two files serve different roles: MEMORY.md is a condensed knowledge base; daily logs are a journal. Both survive compaction because they live on disk, not in the conversation transcript.

### 3.4 Context Window Compaction

#### Normal Path (Pre-compaction Memory Flush)

When session tokens cross `contextWindow - reserveTokensFloor - softThresholdTokens`:

1. **Silent agentic turn fires automatically** — the agent is prompted:
   > "Before we compact, write anything important to memory files. When done, reply NO_REPLY."
2. Agent writes to `memory/YYYY-MM-DD.md` or `MEMORY.md`
3. Replies `NO_REPLY` — user never sees it
4. Tracked in `sessions.json` — one flush per compaction cycle to prevent infinite loops
5. **Then** the conversation is compacted (older messages summarized/dropped)
6. Session resumes with summary + recent messages + everything reloaded from disk

This pre-compaction flush is the key pattern: **save important context to durable storage *before* the lossy compression step**. The agent participates in its own context management.

#### Bad Path (Overflow Recovery, No Flush)

If context gets too large and the API rejects the request, OpenClaw compresses everything at once just to recover. No pre-save. Maximum context loss.

#### Configuration

```json
"agents": {
  "defaults": {
    "compaction": {
      "memoryFlush": {
        "enabled": true,
        "reserveTokensFloor": 20000,
        "softThresholdTokens": 4000
      }
    }
  }
}
```

**Known issues:**
- `softThresholdTokens` is absolute, not relative to model context size — breaks when switching from 200K to 1M token models ([#17034](https://github.com/openclaw/openclaw/issues/17034))
- `memoryFlush` is **disabled by default** — most users never benefit from it ([#25633](https://github.com/openclaw/openclaw/discussions/25633))

### 3.5 Agent-Facing Memory Tools

| Tool | Description |
|---|---|
| `memory_search` | Semantic recall over indexed snippets. Supports vector, BM25 keyword, and hybrid modes with weighted score fusion. |
| `memory_get` | Targeted read of a specific Markdown file and optional line range. Degrades gracefully on missing files. |

### 3.6 Community Memory Plugins

| Plugin | What It Does |
|---|---|
| **QMD Hybrid Search** | BM25 + vector + LLM re-ranking. Two simultaneous retrieval channels. |
| **Mem0 Integration** | External memory outside the context window. Long-term: user-scoped; short-term: session-scoped. Unaffected by compaction. |
| **Mnemon** ([#22960](https://github.com/openclaw/openclaw/discussions/22960)) | LLM-supervised memory. Binary handles deterministic storage/graph indexing/decay; LLM makes judgment calls on what to remember. |
| **LCM (Lossless Context Management)** ([#22251](https://github.com/openclaw/openclaw/discussions/22251)) | Incremental compaction keeps token use below 100K at all times. |
| **12-layer community architecture** | Knowledge graph (3K+ facts), semantic search, activation/decay system, domain RAG. |

---

## 4. PicoClaw Memory Architecture

PicoClaw is the closest analogue to nanosec: Go binary, single-file deployment, workspace-based, AI-bootstrapped.

### 4.1 Two-Tier Memory (Same Pattern as OpenClaw)

| Tier | Location | Purpose |
|---|---|---|
| **Long-term** | `workspace/memory/MEMORY.md` | Curated persistent facts, user preferences, learned rules |
| **Daily logs** | `workspace/memory/YYYYMM/YYYYMMDD.md` | Date-stamped journal entries |

The `MemoryStore` struct exposes:
```go
type MemoryStore struct {
    workspace  string  // e.g. ~/.picoclaw/workspace
    memoryDir  string  // workspace/memory/
    memoryFile string  // workspace/memory/MEMORY.md
}

// Key methods:
ReadLongTerm() string                      // Read MEMORY.md
WriteLongTerm(content string) error        // Overwrite MEMORY.md
ReadToday() string                         // Read YYYYMM/YYYYMMDD.md
AppendToday(content string) error          // Append to today's file
GetRecentDailyNotes(days int) string       // Last N days joined with "---"
GetMemoryContext() string                  // Formatted block for system prompt
```

### 4.2 Bootstrap Files (Pinned Permanent Memory)

Same set as OpenClaw. Loaded at every session start from workspace root:

| File | Purpose |
|---|---|
| `MEMORY.md` | Long-term curated memory |
| `AGENTS.md` | Agent operational rules |
| `IDENTITY.md` | Agent identity and persona |
| `SOUL.md` | Personality and values |
| `USER.md` | User info and preferences |

### 4.3 Session Storage

```go
type Session struct {
    Key      string              // "channel:chat_id"
    Messages []providers.Message // Full conversation history
    Summary  string              // Compressed context (empty until summarization fires)
    Created  time.Time
    Updated  time.Time
}
```

- **In-memory**: `map[string]*Session` with `sync.RWMutex`
- **On-disk**: `sessions/{key}.json` (full JSON, including summary)
- Loaded lazily on first access; saved after every exchange

### 4.4 Full Message Type

```go
type Message struct {
    Role       string     // "system", "user", "assistant", "tool"
    Content    string
    ToolCalls  []ToolCall // Present when role="assistant" and tool use requested
    ToolCallID string     // Present when role="tool" (links result to request)
}
```

Tool messages use the `"tool"` role (OpenAI format). The Claude provider adapter translates these to Anthropic's `tool_result` blocks before sending.

### 4.5 How the System Prompt Is Assembled (Every Turn)

`ContextBuilder.BuildSystemPrompt()` assembles five sections joined with `---` separators:

```
1. Identity Section
   - Agent name, current time, runtime info
   - Workspace path, memory file paths

2. Bootstrap Files (if present)
   - AGENTS.md, SOUL.md, USER.md, IDENTITY.md

3. Skills Summary
   - Installed skill descriptions from skills/{name}/SKILL.md

4. Memory Context
   - Long-term memory (MEMORY.md)
   - Recent daily notes (last 3 days)

5. [Added dynamically in BuildMessages()]
   - Current session info: channel + chat_id
   - Previous conversation summary (if summarization has fired)
```

### 4.6 Final Message Array Sent to LLM (Every Turn)

```
[0] system:    Full assembled system prompt
[1..n]         Session history (user/assistant/tool interleaved)
[n+1] user:    Current user message
```

The session history is the raw message array — **not re-summarized on every call**. The summary only exists in the system prompt. The recent `keepLast=4` messages are appended verbatim after the summary.

### 4.7 Summarization (Triggered Asynchronously)

**Trigger** (checked after each user turn in `maybeSummarize()`):
```go
threshold := al.contextWindow * 75 / 100  // 75% of MaxTokens (default 8192)
if len(history) > 20 || estimateTokens(history) > threshold {
    go summarizeSession(sessionKey)  // non-blocking goroutine
}
```

`sync.Map` prevents duplicate concurrent summarizations for the same session.

**Token estimation**: `len(message.Content) / 4` — simple heuristic, ±15–25% for English, worse for CJK.

**Summarization process**:
1. Take all history except last 4 messages
2. Skip messages with `role == "tool"` or `role == "system"` (only user + assistant summarized)
3. **Oversized guard**: skip individual messages > 50% of context window
4. If batch > 10 messages: split at midpoint → summarize each half → LLM-merge the two summaries
5. If batch ≤ 10: single LLM call, seeded with existing summary
6. Parameters: `max_tokens: 1024, temperature: 0.3`
7. After: `SetSummary(key, finalSummary)` + `TruncateHistory(key, 4)`
8. Save session to disk

**Summarization prompt**:
```
Provide a concise summary of this conversation segment, preserving core context and key points.
Existing context: {existingSummary}

CONVERSATION:
user: ...
assistant: ...
```

### 4.8 LLM Provider Layer

PicoClaw has a native Claude provider:

```go
type LLMResponse struct {
    Content      string
    ToolCalls    []ToolCall
    FinishReason string
    Usage        *UsageInfo  // PromptTokens, CompletionTokens, TotalTokens
}
```

Usage is returned and available. The Claude adapter translates:
- `role: "system"` → Anthropic system block (separate from messages array)
- `role: "tool"` → `anthropic.NewToolResultBlock(toolCallID, content, false)`
- `role: "assistant"` with ToolCalls → `anthropic.NewToolUseBlock(...)` in content blocks

### 4.9 Complete Workspace Layout

```
~/.picoclaw/workspace/
├── memory/
│   ├── MEMORY.md                    # Long-term curated memory
│   └── 202603/                      # Month directory
│       ├── 20260301.md              # Daily notes
│       └── ...
├── sessions/
│   └── {channel}:{chat_id}.json    # Session state (messages + summary)
├── AGENTS.md                        # Agent instructions (bootstrap)
├── SOUL.md                          # Personality (bootstrap)
├── USER.md                          # User context (bootstrap)
├── IDENTITY.md                      # Identity (bootstrap)
├── skills/                          # Installed skill packs
│   └── {name}/SKILL.md
└── cron/
    └── jobs.json                    # Scheduled tasks
```

---

## 5. Claude Code Memory Architecture

Claude Code (this tool) implements the same patterns at the Anthropic API level. Documented here for cross-reference.

### 5.1 Four Memory Tiers

| Tier | What It Is | Scope |
|---|---|---|
| **CLAUDE.md** | Human-curated permanent rules, project conventions | Loaded every session, survives all compaction |
| **Auto Memory** | Claude-written notes (`~/.claude/projects/<proj>/memory/`) | First 200 lines auto-injected; topic files on-demand |
| **Session Transcript** | `~/.claude/sessions/<id>/transcript.jsonl` | Per-conversation; `--continue` / `--resume` across sessions |
| **Memory Tool** | Agent-controlled persistent key-value store (`create_memory`, `read_memory`) | Survives across conversations; just-in-time retrieval |

The **200-line hard limit** on auto-loaded MEMORY.md content is a known architectural constraint — files longer than 200 lines are truncated in the injected context.

### 5.2 Auto-Compaction

Threshold: **~98% of effective context window** (200K tokens, minus ~30–40K consumed by system prompt + tool definitions).

Order of operations:
1. **Tool outputs cleared first** (oldest, largest, least-valuable tokens) — replaced with placeholders
2. Remaining conversation summarized
3. Summary replaces old messages
4. Session continues: system prompt is reloaded fresh from disk (bootstrap files re-read)

Manual: `/compact` (entire conversation) or `/compact focus on the API changes` (focused).

### 5.3 PreCompact Hook

A user-defined hook that fires before compaction. Configured in `.claude/settings.local.json`:
```json
{
  "hooks": {
    "PreCompact": [{
      "matcher": "auto",
      "hooks": [{
        "type": "command",
        "command": "claude -p 'Write a HANDOVER-$(date +%Y-%m-%d).md with key context before compaction'"
      }]
    }]
  }
}
```

This is the Claude Code equivalent of OpenClaw's pre-compaction memory flush: save important context to a durable file before the lossy compression step.

### 5.4 Tool Result Clearing (Context Editing API)

Distinct from compaction — a surgical mechanism to clear only tool outputs:

```http
anthropic-beta: context-management-2025-06-27
```

Strategy: `clear_tool_uses_20250919`
- Clears oldest tool results first (chronologically)
- Each cleared result replaced with a placeholder ("result was cleared")
- `clear_tool_inputs: true` also clears the input parameters
- Runs **server-side** — client retains full unmodified history
- Strategy `clear_thinking_20251015` handles extended-thinking blocks separately

**Documented impact**: 84% reduction in token consumption in a 100-turn web search evaluation, with no measurable degradation in task completion quality. Highest-ROI single step.

### 5.5 Sub-Agent Context Isolation

- Each sub-agent (spawned via the `Task` tool) gets its own isolated 200K token context window
- Sub-agents start fresh — no parent conversation history
- When done, the entire sub-agent context is discarded; only the result summary returns to the parent
- Up to 10 concurrent sub-agents → ~2M tokens total effective context across all sub-agents
- Sub-agents cannot spawn further sub-agents (no nested delegation)
- 67% fewer tokens vs. keeping all work in a single context

---

## 6. Cross-System Comparison

| Feature | OpenClaw | PicoClaw | Claude Code | Nanosec (current) |
|---|---|---|---|---|
| **Long-term memory file** | `MEMORY.md` | `MEMORY.md` | `MEMORY.md` (auto-written) | — |
| **Daily notes** | `memory/YYYY-MM-DD.md` | `memory/YYYYMM/YYYYMMDD.md` | Session transcripts | — |
| **Bootstrap files** | SOUL, AGENTS, USER, TOOLS | SOUL, AGENTS, IDENTITY, USER | CLAUDE.md | — |
| **Session storage** | JSONL on disk | JSON on disk | JSONL on disk | JSONL on disk ✓ |
| **Session key** | `channel:chatID` | `channel:chatID` | Session ID (UUID) | `channel:chatID` ✓ |
| **Summarization trigger** | Token threshold + message count | `>20 msgs OR >75% tokens` | ~98% context window | — |
| **Pre-compaction flush** | ✓ (silent agentic turn) | — (not implemented) | ✓ (PreCompact hook) | — |
| **Tool result clearing** | — | — | ✓ (server-side API) | — |
| **Sub-agent isolation** | ✓ | `spawn` tool (planned) | ✓ (Task tool) | `spawn` tool (planned) |
| **Usage token tracking** | ✓ | ✓ (Usage field present) | ✓ | ✗ (discarded today) |
| **Memory search tool** | ✓ (`memory_search`) | — | ✓ (Memory tool) | — |
| **Memory write tool** | ✓ (`memory_get`) | ✓ (write_file to MEMORY.md) | ✓ (`create_memory`) | ✓ (write_file) |

---

## 7. Key Patterns Shared Across All Systems

From comparing OpenClaw, PicoClaw, Claude Code, and the broader ecosystem, five architectural patterns appear universally:

### Pattern 1: Pinned Permanent Memory (Bootstrap Files)
**Every system** uses a set of Markdown files that are loaded at session start and re-read after compaction. They serve as an "immune" memory layer — they cannot be lost to context overflow because they live on disk outside the session transcript.

Nanosec has `workspace` support and the `write_file` tool already. The gap is formalizing a `MEMORY.md` convention and injecting it into every system prompt.

### Pattern 2: Two-Tier Temporal Memory (Long-term + Daily)
All mature agents separate curated long-term facts (MEMORY.md, kept small) from raw daily logs (date-stamped files, append-only). The daily logs provide short-term continuity without bloating the permanent memory file.

### Pattern 3: Pre-Compaction Flush (Save Before Compress)
OpenClaw and Claude Code both trigger a memory-write step **before** the lossy compression step. This ensures the agent's own judgment about "what is worth keeping" is applied before the context is truncated. PicoClaw does not implement this — it is one of its main architectural gaps.

### Pattern 4: Tool Result Pruning Before Summarization
Claude Code clears tool outputs first (largest, most ephemeral tokens) before touching conversation messages. This is also the highest single-ROI optimization — 84% token reduction in tool-heavy workflows.

### Pattern 5: Sub-Agent Isolation as Context Budget Management
All frameworks treat sub-agents as isolated context windows. The main agent delegates expensive work (file scanning, web research, test execution) to sub-agents whose verbose output never appears in the parent context.

---

## 8. Gaps in Nanosec vs. Reference Systems

| Gap | Impact | Effort to Close |
|---|---|---|
| No `MEMORY.md` injection into system prompt | Agent cannot retain facts across sessions | Low — add to `agent/context.go` |
| No daily notes pattern | No short-term cross-session continuity | Low — add `AppendToday()` to session store |
| No bootstrap files (`AGENTS.md`, `USER.md`) | No pinned permanent operational rules | Low — file-reading, inject in context |
| No summarization | Sessions grow unbounded; eventual token overflow | Medium — already researched in `doc/context_research.md` |
| No pre-compaction flush | Summarization is lossy with no agent save step | Medium — requires agent LLM call before compress |
| No tool result pruning | Tool-heavy sessions blow up context fast | Low — cap output before adding to session |
| `ChatResponse` has no `Usage` field | Token counting uses char/4 heuristic; triggers miscalibrated | Low — add `Usage` to `provider.ChatResponse` |
| No `memory_search` tool | Agent cannot recall specific old facts selectively | High — requires embedding/BM25 index |
| No session versioning | Cannot start fresh session for same user without wiping history | Medium — add timestamp suffix to key |

---

## 9. Recommended Implementation Sequence for Nanosec

Based on what all reference systems share, from lowest to highest effort:

### Step 1 — Bootstrap Memory Injection (Zero New Dependencies)
Add `MEMORY.md` and other bootstrap files to the context builder in `agent/context.go`:
```go
func (cb *ContextBuilder) BuildSystemPrompt(session *Session) string {
    parts := []string{coreIdentity}

    // Bootstrap files (loaded from disk every turn — survive compaction)
    for _, name := range []string{"MEMORY.md", "AGENTS.md", "USER.md"} {
        if content := cb.readWorkspaceFile(name); content != "" {
            parts = append(parts, "## "+name+"\n\n"+content)
        }
    }

    // Session summary (from in-session summarization)
    if session.Summary != "" {
        parts = append(parts, "## Summary of Previous Conversation\n\n"+session.Summary)
    }

    return strings.Join(parts, "\n\n---\n\n")
}
```

### Step 2 — Daily Notes Append (Zero New Dependencies)
Add `AppendDailyNote(content string)` to the session store. After each exchange, let the agent write a one-liner to `memory/YYYYMMDD.md`. Load today's + yesterday's notes into the system prompt.

### Step 3 — Tool Result Capping (Zero New Dependencies, Highest ROI)
Before appending any tool result to the session, cap it:
```go
const maxToolResultBytes = 8000
if len(result) > maxToolResultBytes {
    result = result[:maxToolResultBytes] + "\n[... output truncated ...]"
}
```
No summarization needed. Prevents shell/file tool outputs from exploding context.

### Step 4 — `Usage` Field on `ChatResponse`
Expose `PromptTokens` from the API response. Use exact token counts for summarization triggers instead of the char/4 heuristic. This properly calibrates all thresholds.

### Step 5 — Summarization (Already Researched)
Implement picoclaw's `maybeSummarize()` pattern as documented in `doc/context_research.md`.

### Step 6 — Pre-Compaction Memory Flush
Before `maybeSummarize()` fires, trigger a silent agent turn:
```
"Before we compress this conversation: please write any important facts,
decisions, or context to your MEMORY.md or today's daily note. Reply with
NO_REPLY when done."
```
Add a `memoryFlushed bool` flag to the session to ensure it fires at most once per compaction cycle.

### Step 7 — Session Versioning + Auto-Archive
See `doc/session_classification_research.md` for the full plan. This enables starting a new context for a new topic without wiping old history.

---

## 10. Sources

- [OpenClaw — Official Site](https://openclaw.ai)
- [OpenClaw — Memory Docs](https://docs.openclaw.ai/concepts/memory)
- [OpenClaw — Wikipedia](https://en.wikipedia.org/wiki/OpenClaw)
- [openclaw/openclaw — GitHub](https://github.com/openclaw)
- [Memory flush softThresholdTokens issue #17034](https://github.com/openclaw/openclaw/issues/17034)
- [OpenClaw Memory Is Broken By Default — Discussion #25633](https://github.com/openclaw/openclaw/discussions/25633)
- [LCM: Lossless Context Management — Discussion #22251](https://github.com/openclaw/openclaw/discussions/22251)
- [Mnemon Plugin — Discussion #22960](https://github.com/openclaw/openclaw/discussions/22960)
- [Agent-Controlled Context Pruning — Issue #16951](https://github.com/openclaw/openclaw/issues/16951)
- [OpenClaw Memory Masterclass — VelvetShark](https://velvetshark.com/openclaw-memory-masterclass)
- [Advanced Memory Management in OpenClaw — LumaDock](https://lumadock.com/tutorials/openclaw-advanced-memory-management)
- [We Built Persistent Memory for OpenClaw — Mem0](https://mem0.ai/blog/mem0-memory-for-openclaw)
- [12-layer OpenClaw Memory Architecture — GitHub](https://github.com/coolmanns/openclaw-memory-architecture)
- [sipeed/picoclaw — GitHub](https://github.com/sipeed/picoclaw)
- [PicoClaw — AI Agent Store](https://aiagentstore.ai/ai-agent/picoclaw)
- [Agent Configuration — DeepWiki/picoclaw](https://deepwiki.com/erha2025/picoclaw/3.1-agent-configuration)
- [Multi-agent framework Issue #294 — sipeed/picoclaw](https://github.com/sipeed/picoclaw/issues/294)
- [qwibitai/nanoclaw — GitHub](https://github.com/qwibitai/nanoclaw)
- [The Claw AI Agent Family — PeterWoods](https://peterwoods.online/blog/the-claw-ai-agent-family)
- [How Claude Code works — Official Docs](https://code.claude.com/docs/en/how-claude-code-works)
- [Context Window & Compaction — DeepWiki Claude Code](https://deepwiki.com/anthropics/claude-code/3.3-context-window-and-compaction)
- [Context editing — Claude API Docs](https://platform.claude.com/docs/en/build-with-claude/context-editing)
- [Memory tool — Claude API Docs](https://platform.claude.com/docs/en/agents-and-tools/tool-use/memory-tool)
- [Claude Code Auto Memory and PreCompact Hooks — YuanChang](https://yuanchang.org/en/posts/claude-code-auto-memory-and-hooks/)
- [Context Management with Subagents — RichSnapp](https://www.richsnapp.com/article/2025/10-05-context-management-with-subagents-in-claude-code)
