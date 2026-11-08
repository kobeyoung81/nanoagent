# Parity Comparison Report

**Version**: 2.0
**Date**: 2026-02-22
**Status**: Active

This document compares **nanosec** (our Go distributed rewrite) against the two reference implementations: **nanobot** (Python original) and **picoclaw** (Go single-process clone). It identifies feature gaps and informs roadmap priorities.

---

## 1. Project Overview

| | Nanobot (Python) | Picoclaw (Go) | Nanosec (Go) |
|--|-----------------|---------------|---------------|
| **Role** | Original reference | Go reference | Our project |
| **LOC** | ~6,159 | ~11,300 | ~4,977 |
| **Source files** | 47 Python files | 48 Go files | 41 Go files |
| **Go Version** | N/A (Python 3.11+) | 1.25.7 | 1.22 |
| **License** | MIT | MIT | - |
| **Status** | Production | Production | Phase 5 complete, Phase 6 not started |

### LOC Breakdown (Nanosec)

| Package | LOC | Purpose |
|---------|-----|---------|
| `internal/tool` | 803 | 8 agent tools |
| `cmd/*` | 614 | 3 binary entry points + seed |
| `internal/agent` | 520 | Agent loop, context, subagents |
| `internal/skill` | 327 | Skill loader + registry |
| `internal/protocol` | 325 | WebSocket protocol types + client |
| `internal/cron` | 318 | Job scheduler + persistence |
| `internal/bus/store` | 307 | MySQL user/queue storage |
| `internal/gateway` | 286 | Gateway core + bus client |
| `internal/config` | 272 | Config structs + loader + defaults |
| `internal/bus` | 244 | WebSocket server + handlers |
| `internal/gateway/channel/*` | 544 | 3 channel adapters (CLI, WeCom, Feishu) |
| `internal/provider` | 218 | OpenAI-compatible provider |
| `internal/session` | 199 | JSONL session storage |

---

## 2. Architecture Comparison

### 2.1 Process Model

| | Nanobot | Picoclaw | Nanosec |
|--|---------|----------|---------|
| **Topology** | Single-process | Single-process | **3-process distributed** |
| **Message Bus** | `asyncio.Queue` (in-memory) | Go buffered channels (in-memory) | WebSocket + MySQL (networked) |
| **Concurrency** | async/await | goroutines + channels | goroutines + channels |
| **Multi-user** | Per-instance | Per-instance | Native (bus-level isolation) |

Nanosec's distributed model is the key differentiator. The bus persists messages in MySQL with atomic claiming (`SELECT ... FOR UPDATE`), enabling:

- Independent scaling of bus, gateway, and agent
- Message survival across process restarts
- Multiple agents per user (future)
- Multi-node deployment

### 2.2 Binary Architecture

| Binary | Responsibility | Key Dependencies |
|--------|---------------|-----------------|
| `nanobus` | Message routing, MySQL persistence, WebSocket server | `go-sql-driver/mysql`, `gorilla/websocket` |
| `nanogateway` | Channel adapters, user resolution, message delivery | `xen0n/go-workwx`, `larksuite/oapi-sdk-go` |
| `nanosec` | LLM calls, tool execution, session management | `sashabaranov/go-openai` |

### 2.3 Package Layout

| | Nanobot | Picoclaw | Nanosec |
|--|---------|----------|---------|
| **Layout** | Flat Python modules | `pkg/` (public) | `internal/` (private) |
| **CLI** | Typer | Cobra | Cobra |
| **Logger** | loguru | Custom logger | `slog` (stdlib) |
| **Config** | Pydantic + JSON | `caarlos0/env` + JSON | Viper + JSON |

---

## 3. Feature Parity Matrix

### 3.1 Channels

Target audience is mainland China. Priority adjusted accordingly.

| Channel | Nanobot | Picoclaw | Nanosec | Status |
|---------|---------|----------|---------|--------|
| **CLI** | Interactive REPL | Interactive (readline) | Interactive + one-shot | ✅ Done |
| **WeCom** (Enterprise WeChat) | - | - | HTTP callback via go-workwx | ✅ Done |
| **Feishu** | WebSocket | WebSocket | WebSocket via oapi-sdk-go | ✅ Done |
| **DingTalk** | - | Stream SDK | - | Not planned |
| **Telegram** | Long polling | Webhook | - | Not planned |
| **Discord** | WebSocket | WebSocket | - | Not planned |
| **Slack** | - | Socket mode | - | Not planned |
| **WhatsApp** | Bridge (Baileys) | Bridge | - | Not planned |
| **QQ** | - | BotGo SDK | - | Not planned |
| **MaixCAM** | - | HTTP endpoint | - | Not planned |

**Summary:** Nanosec has **3 channels** (CLI, WeCom, Feishu). Nanobot has 4 (Telegram, Discord, WhatsApp, Feishu). Picoclaw has 8. All three priority channels for mainland China enterprise users are implemented.

### 3.2 LLM Providers

| Provider | Nanobot | Picoclaw | Nanosec | Notes |
|----------|---------|----------|---------|-------|
| **OpenAI-compatible** | Via LiteLLM | HTTP provider | ✅ Working | Covers DeepSeek, Groq, vLLM, OpenRouter via baseURL |
| **Anthropic (Claude)** | Via LiteLLM | Direct SDK | ❌ Not started | Highest-priority addition |
| **Zhipu (GLM)** | Via LiteLLM | Direct | ⚡ Via OpenAI-compat | Works if provider has OpenAI-compat endpoint |
| **DeepSeek** | Via LiteLLM | Direct | ⚡ Via OpenAI-compat | OpenAI-compatible API |
| **Groq** | Via LiteLLM | Direct | ⚡ Via OpenAI-compat | OpenAI-compatible API |
| **Gemini** | Via LiteLLM | Direct | ❌ Not started | Needs native SDK |
| **OpenRouter** | Via LiteLLM | Direct | ⚡ Via OpenAI-compat | OpenAI-compatible gateway |
| **DashScope (Qwen)** | Via LiteLLM | - | ⚡ Via OpenAI-compat | OpenAI-compatible API |
| **Moonshot (Kimi)** | Via LiteLLM | - | ⚡ Via OpenAI-compat | OpenAI-compatible API |
| **vLLM (local)** | Via LiteLLM | Direct | ⚡ Via OpenAI-compat | Any local OpenAI-compat server |
| **Streaming** | Yes | Yes | ❌ Not implemented | Blocking API calls only |

Nanobot supports 11+ providers via LiteLLM. Picoclaw has 7 native providers. Nanosec has 1 (OpenAI-compatible), but the configurable `baseURL` means it practically reaches 7+ services that expose OpenAI-compatible endpoints.

**Gap:** Low for most use cases. Native Anthropic Claude is the main missing provider. Streaming would improve UX for long responses.

### 3.3 Tools

| Tool | Nanobot | Picoclaw | Nanosec | Status |
|------|---------|----------|---------|--------|
| **shell/exec** | Yes (deny list) | Yes (deny list) | Yes (deny list) | ✅ Done |
| **read_file** | Yes | Yes | Yes | ✅ Done |
| **write_file** | Yes | Yes | Yes | ✅ Done |
| **list_dir** | Yes | Yes | Yes | ✅ Done |
| **edit_file** | Yes (find & replace) | Yes (line-based) | Yes (find & replace) | ✅ Done |
| **append_note** | `memory/YYYY-MM-DD.md` | append_file (generic) | Yes (`memory/YYYY-MM-DD.md`) | ✅ Done |
| **web_search** | Brave API | Brave API | Brave API | ✅ Done |
| **web_fetch** | Yes (readability) | Yes (readability) | Yes (**raw HTML**, no readability) | ⚠️ No HTML→text |
| **message** | Yes (cross-channel) | Yes (contextual) | Yes (via bus) | ✅ Done |
| **spawn** (subagent) | Yes (asyncio.Task) | Yes (goroutine) | Yes (goroutine) | ✅ Done |
| **cron** (schedule) | Yes (croniter) | Yes (gronx) | Yes (robfig/cron) | ✅ Done |

**Summary:** 10 of 11 tools implemented. Remaining gap:
- **web_fetch readability**: Both refs extract clean text from HTML. Nanosec returns raw HTML (100KB limit).

### 3.4 Skills System

| Feature | Nanobot | Picoclaw | Nanosec | Status |
|---------|---------|----------|---------|--------|
| **Skill loader** | Yes (YAML frontmatter) | Yes (YAML frontmatter) | Yes (YAML frontmatter) | ✅ Done |
| **Progressive loading** | Summary in prompt, full on demand | Summary in prompt, full on demand | Summary in prompt, always-skills injected | ✅ Done |
| **Built-in skills** | 6 (weather, github, tmux, summarize, skill-creator, cron) | 5 (weather, github, tmux, summarize, skill-creator) | **3** (weather, github, tmux) | ⚠️ Missing 2-3 |
| **Workspace override** | Workspace directory | Workspace + global + builtin | Workspace + builtin | ✅ Done |
| **Requirements check** | Binary + env var checks | Binary + env var checks | Binary + env var checks | ✅ Done |
| **Skill install CLI** | - | `skills install/remove/search` | **Not implemented** | ❌ Gap |

**Summary:** Core skill system is fully operational. Missing the `summarize` and `skill-creator` builtin skills, and the CLI skill management commands from picoclaw.

### 3.5 Memory & Context

| Feature | Nanobot | Picoclaw | Nanosec | Status |
|---------|---------|----------|---------|--------|
| **Session storage** | JSONL files | JSON + auto-summarization | JSONL files | ✅ Done |
| **Long-term memory** | `MEMORY.md` in workspace | `MEMORY.md` in workspace | Auto-loaded via context builder | ✅ Done |
| **Daily notes** | `memory/YYYY-MM-DD.md` | `memory/YYYYMM/YYYYMMDD.md` | `memory/YYYY-MM-DD.md` via append_note | ✅ Done |
| **Bootstrap files** | AGENTS.md, SOUL.md, USER.md, IDENTITY.md, TOOLS.md | Same | AGENTS.md, SOUL.md, USER.md, TOOLS.md | ✅ Done (4 of 5) |
| **Context builder** | Full assembly (identity + memory + skills + runtime) | Full assembly (identity + memory + skills + runtime) | Identity + bootstrap + memory + skills + runtime | ✅ Done |
| **Auto-summarization** | Via LLM when history long | At 20 msgs or 75% tokens | **Not implemented** | ❌ Gap |

**Summary:** The context builder assembles system prompts from bootstrap files, memory files, skills, and runtime info. The main remaining gap is:
- **Auto-summarization**: Sessions grow unbounded. Both refs summarize to manage context window.

### 3.6 Cron & Scheduling

| Feature | Nanobot | Picoclaw | Nanosec | Status |
|---------|---------|----------|---------|--------|
| **Cron expressions** | Yes (croniter) | Yes (gronx) | Yes (robfig/cron) | ✅ Done |
| **Interval (every N sec)** | Yes | Yes | Yes (every_seconds) | ✅ Done |
| **One-shot (at time)** | Yes | Yes | Yes (at timestamp) | ✅ Done |
| **Job persistence** | JSON file | JSON file | JSON file | ✅ Done |
| **Enable/disable** | Yes | Yes | **Not implemented** | ❌ Gap |
| **CLI management** | add/list/remove/enable/disable/run | Same | **add/list/remove** (via tool only) | ⚠️ No CLI commands |

**Summary:** Core scheduling is complete. Missing enable/disable toggle and dedicated CLI commands (currently only accessible via the LLM tool interface).

### 3.7 Subagents

| Feature | Nanobot | Picoclaw | Nanosec | Status |
|---------|---------|----------|---------|--------|
| **Spawn background task** | Yes (asyncio.Task) | Yes (goroutine) | Yes (goroutine) | ✅ Done |
| **Limited tool set** | No message/spawn/cron | Similar | No message/spawn/cron | ✅ Done |
| **Result announcement** | System message to channel | Similar | Via bus PostOutbound | ✅ Done |
| **Max iterations** | 15 | Configurable | Uses agent config | ✅ Done |
| **Origin tracking** | Routes to original channel | Similar | Stores channel + chatID | ✅ Done |

**Summary:** Full parity on subagent functionality.

### 3.8 Other Features

| Feature | Nanobot | Picoclaw | Nanosec | Status |
|---------|---------|----------|---------|--------|
| **Voice transcription** | Groq Whisper (Telegram) | Groq Whisper (Telegram/Discord/Slack) | Not implemented | ❌ Gap |
| **Heartbeat service** | 30min proactive prompts via HEARTBEAT.md | 30min intervals | Not implemented | ❌ Gap |
| **Auth system** | API keys only | OAuth/PKCE + API keys | API keys only | ✅ Sufficient |
| **Migration tool** | - | From OpenClaw | - | N/A |
| **Onboard command** | Interactive setup | Interactive setup | Not implemented | ❌ Phase 6 |
| **Status command** | Show config + API keys | Show config + status | Not implemented | ❌ Phase 6 |
| **User management** | N/A (single-user) | N/A (single-user) | `nanobus user add/list/link` | ✅ Nanosec-only |

---

## 4. What Nanosec Has That Others Don't

Nanosec's distributed architecture provides capabilities neither reference has:

1. **MySQL message persistence** — Messages survive process restarts. Both references lose all in-flight messages on crash.
2. **Horizontal scaling** — Bus, gateway, and agent on separate machines. References are single-process only.
3. **Multi-user by design** — Bus routes messages to per-user agents with identity resolution. References need one instance per user.
4. **Independent deployment** — Update gateway (add channels) without touching the agent. Update agent (new tools) without touching the bus.
5. **Atomic message claiming** — `SELECT ... FOR UPDATE` prevents duplicate processing. References use in-memory channels with no durability.
6. **WeCom support** — Neither reference has Enterprise WeChat integration. Nanosec does.
7. **User management CLI** — `nanobus user add/list/link` for multi-user administration.

---

## 5. Parity Scorecard

### Overall Feature Coverage

| Category | Nanobot Features | Nanosec Has | Coverage |
|----------|-----------------|-------------|----------|
| Channels | 4 | 3 (different set) | ~75% |
| LLM Providers | 11+ (via LiteLLM) | 1 (covers ~7 via compat) | ~64% |
| Tools | 10 | 10 | ~100% |
| Skills | 6 builtin + loader | 3 builtin + loader | 50% |
| Memory/Context | 5 features | 4.5 features | ~90% |
| Cron | Full (6 operations) | Basic (3 operations) | 50% |
| Subagents | Full | Full | 100% |
| Voice | Yes | No | 0% |
| Heartbeat | Yes | No | 0% |
| CLI UX | 6 commands | 1 command (user mgmt) | ~17% |

| Category | Picoclaw Features | Nanosec Has | Coverage |
|----------|------------------|-------------|----------|
| Channels | 8 | 3 | ~38% |
| LLM Providers | 7 native | 1 (covers ~7 via compat) | ~100% |
| Tools | 11 | 10 | ~91% |
| Skills | 5 builtin + loader + installer | 3 builtin + loader | 50% |
| Memory/Context | 5 features | 4.5 features | ~90% |
| Cron | Full (6 operations) | Basic (3 operations) | 50% |
| Subagents | Full | Full | 100% |
| Voice | Yes | No | 0% |
| Heartbeat | Yes | No | 0% |
| CLI UX | 15+ commands | 1 command | ~7% |

---

## 6. Gap Summary & Priorities

### Closed Since v1.0 (2026-02-12)

These gaps from the original parity report are now resolved:

| Gap | Resolution | Date |
|-----|-----------|------|
| WeCom channel | HTTP callback server via go-workwx | 2026-02-12 |
| Feishu channel | WebSocket via oapi-sdk-go | 2026-02-12 |
| Skills system | Loader + registry + 3 builtin skills | 2026-02-21 |
| Context builder | Bootstrap files + skills + runtime | 2026-02-21 |
| Cron scheduler | JSON-persisted, 3 schedule types | 2026-02-21 |
| Subagent spawning | Goroutine-based with isolated tools | 2026-02-21 |
| edit_file tool | Find-and-replace in `internal/tool/filesystem.go` | 2026-02-22 |
| Shell deny list | 8 patterns blocking rm -rf, mkfs, dd, fork bomb, etc. | 2026-02-22 |
| MEMORY.md auto-loading | `ContextBuilder.loadMemory()` injects long-term + daily notes | 2026-02-22 |
| Daily notes (append_note) | Timestamped entries to `memory/YYYY-MM-DD.md` | 2026-02-22 |

### Remaining Critical Gaps (Block production usage)

| Gap | Impact | Effort | Notes |
|-----|--------|--------|-------|
| Auto-summarization | Sessions grow unbounded, will exceed context window | Medium | Need LLM-based summarization at threshold |

### Remaining Significant Gaps (Reduce usefulness)

| Gap | Impact | Effort | Notes |
|-----|--------|--------|-------|
| web_fetch readability | Raw HTML is noisy; clean text extraction needed | Medium | Use go-readability or similar |
| Cron enable/disable | Can't pause jobs without deleting | Small | Add Enabled flag to Job |
| Cron/status CLI commands | Scheduling only via LLM tool, not command line | Medium | Phase 6 Cobra commands |

### Moderate Gaps (Quality of life)

| Gap | Impact | Effort | Notes |
|-----|--------|--------|-------|
| append_file tool | Minor; write_file covers most cases | Small | |
| Streaming responses | Long responses feel slow without streaming | Medium | Needs provider + gateway changes |
| Native Anthropic provider | Can't use Claude without OpenAI-compat proxy | Medium | New provider implementation |
| Onboard command | No guided first-run setup | Small | Phase 6 |
| Status command | No system health visibility | Small | Phase 6 |
| Missing builtin skills | No summarize, skill-creator, cron skill | Small | Port from refs |
| Voice transcription | No audio support on any channel | Medium | Groq Whisper integration |
| Heartbeat service | No proactive agent behaviors | Medium | Port from refs |

---

## 7. Recommendations

### Immediate (Pre-Phase 6)

These items were completed on 2026-02-22 — see "Closed Since v1.0" above:
- ~~Add edit_file tool~~
- ~~Add shell command deny list~~
- ~~Load MEMORY.md in context~~
- ~~Add daily notes (append_note)~~

### Phase 6 (Polish)

5. **CLI commands** — `onboard`, `status`, `cron list/add/remove`, `skills list`
6. **Auto-summarization** — Summarize session when messages exceed threshold
7. **web_fetch readability** — Clean text extraction from HTML
8. **Streaming support** — For better UX on long responses
9. **Missing builtin skills** — Port `summarize` and `skill-creator`

### Post-v0.1.0

10. **Native Anthropic provider** — Direct Claude API
11. **Voice transcription** — Groq Whisper on channels that support audio
12. **Heartbeat service** — Proactive periodic checks via HEARTBEAT.md
13. **Additional channels** — DingTalk, Telegram if user demand exists

### Reference Usage Guide

| Task | Best Reference |
|------|---------------|
| edit_file implementation | Nanobot — find & replace |
| Shell deny list patterns | Both (similar blocked patterns list) |
| Memory system (daily notes) | Nanobot |
| Auto-summarization | Picoclaw |
| Voice transcription | Picoclaw |
| Heartbeat service | Both (similar pattern) |
| Skill installer CLI | Picoclaw |
| Context builder enrichment | Both (similar approach) |

---

## Revision History

| Version | Date | Author | Changes |
|---------|------|--------|---------|
| 1.0 | 2026-02-12 | Claude | Initial parity analysis |
| 2.0 | 2026-02-22 | Claude | Full update: WeCom/Feishu/skills/cron/subagents now done. Corrected LOC counts. Added parity scorecard. Reclassified gaps. |
| 2.1 | 2026-02-22 | Claude | Mark 4 pre-Phase-6 items complete: edit_file, shell deny list, MEMORY.md loading, append_note/daily notes. Updated scorecard (tools ~91-100%, memory ~90%). |
