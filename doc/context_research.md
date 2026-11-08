# Context Window Management: Research Report

**Date**: 2026-03-08
**Status**: Research Complete

---

## 1. Current State of Nanosec

Nanosec has **no context window management**. The session (`internal/session/session.go`) is a plain append-only message list. Every message is kept forever and sent to the LLM verbatim on every turn. This will eventually cause API `context_length_exceeded` errors and degraded quality as sessions grow.

| Capability | Nanosec | PicoClaw |
|---|---|---|
| Token counting | — | `len/4` heuristic |
| Summarization trigger | — | `>20 msgs OR >75% window` |
| LLM summarization | — | async goroutine |
| Summary injection into system prompt | — | appended to system prompt |
| Keep-last-N after compression | — | keep last 4 |
| Summary persistence | — | `Summary` field on Session |
| Tool result size capping | — | oversized messages skipped |
| API usage tracking | — | not in PicoClaw either |

---

## 2. PicoClaw (Go Reference — Most Relevant)

PicoClaw implements a **summary buffer hybrid** — the most practical approach for a conversational agent.

### 2.1 Architecture

Three interlocking mechanisms:

**A. Two-tier session storage**
- Sessions store `[]providers.Message` in memory, keyed by `"channel:chatID"`
- Sessions persist to JSON files in `workspace/sessions/`
- Each session holds a `Summary string` field alongside its raw `Messages`
- `GetSummary()` / `SetSummary()` / `TruncateHistory()` are first-class operations

**B. Summarization-based context compression** (`summarizeSession`)

Trigger condition (dual threshold, `maybeSummarize()`):
```go
if len(newHistory) > 20 || tokenEstimate > contextWindow * 75 / 100 {
```
Both conditions are checked after each user turn. The threshold is 75% of `cfg.Agents.Defaults.MaxTokens` (default 8192 tokens). If either fires and no summarization is already running for that session, a goroutine is launched.

What gets summarized: All history except the last 4 messages (`toSummarize := history[:len(history)-4]`). Tool messages are excluded — only `user` and `assistant` roles are summarized.

**Oversized message guard:** Individual messages larger than 50% of the context window (`len(m.Content)/4 > contextWindow/2`) are silently skipped from summarization, with a footnote added: `[Note: Some oversized messages were omitted from this summary for efficiency.]`

**Multi-part summarization:** If the batch exceeds 10 messages, it is split at the midpoint, summarized as two independent halves, then merged with a third LLM call: `"Merge these two conversation summaries into one cohesive summary"`. For shorter batches, a single call is made, optionally seeded with the existing summary.

**Summarization prompt:**
```
"Provide a concise summary of this conversation segment, preserving core context and key points.\n"
+ "Existing context: " + existingSummary + "\n"
+ "\nCONVERSATION:\n"
+ formatted messages
```
Parameters: `max_tokens: 1024, temperature: 0.3`.

After summarization: `SetSummary(sessionKey, finalSummary)` then `TruncateHistory(sessionKey, 4)` — only 4 most recent messages remain.

**C. Summary injection into context** (`context.go`, `BuildMessages()`)

The summary is appended to the system prompt:
```go
if summary != "" {
    systemPrompt += "\n\n## Summary of Previous Conversation\n\n" + summary
}
```
The message list sent to the LLM is then:
```
[system_prompt_with_summary] + recent_4_messages + current_user_message
```

### 2.2 Token Estimation

```go
func (al *AgentLoop) estimateTokens(messages []providers.Message) int {
    total := 0
    for _, m := range messages {
        total += len(m.Content) / 4 // ~4 chars per token
    }
    return total
}
```
Simple heuristic, accurate within ±15–25% for English. Worse for Chinese or code (~1.5–2 chars/token). Tool call arguments and tool result content are not counted — only `.Content` fields.

### 2.3 Concurrency Safety

`sync.Map` (`al.summarizing`) prevents duplicate concurrent summarization goroutines for the same session. `SessionManager` uses `sync.RWMutex` for all read/write operations.

### 2.4 Long-Term File Memory (`memory.go`)

`MemoryStore` is a persistent external store of long-term facts, loaded into the system prompt every turn. Survives session resets. Not part of context compression — it is a fixed injected layer.

- `MEMORY.md`: Agent-managed long-term facts
- `memory/YYYY-MM-DD.md`: Daily notes

### 2.5 Tradeoffs

- **Keep-last-4** is aggressive — complex multi-step tool chains can span more than 4 messages and get cut
- **Async goroutine** means summarization happens after-the-fact; the next turn may still get bloated context before summarization completes
- **Tool messages excluded from summary** — tool result content is permanently lost after summarization (intentional: ephemeral)
- **75% of MaxTokens** is the output token limit, not the model's full context window — may trigger too early for large-context models

---

## 3. Nanobot Python (Other Reference)

No summarization at all. Pure sliding window with a hard cap:
```python
recent = self.messages[-max_messages:] if len(self.messages) > max_messages else self.messages
# default max_messages = 50
```
PicoClaw's Go rewrite was a deliberate upgrade over this approach.

---

## 4. Claude Code (Anthropic's CLI)

### 4.1 Server-Side Auto-Compaction

**Trigger:** Context reaches ~83.5% of the 200K window (~167K tokens).

**What happens:**
1. API detects token count exceeds threshold
2. A `compaction` block is generated (structured summary by the model)
3. On the next request, all messages prior to the compaction block are discarded

**Custom instructions:** The summarization prompt is fully configurable via the `instructions` field.

**`pause_after_compaction`:** Setting this to `true` causes the API to pause after generating the summary, letting you inject pinned instructions, recent file contents, etc., before resuming. Useful for preserving critical context across compaction boundaries.

**Multiple compactions:** Long conversations can trigger multiple compactions; only the most recent block matters.

### 4.2 Tool Result Clearing

Oldest tool call results are cleared first (they are the bulkiest), replaced with placeholders so the model knows they were removed.

**Impact:** Shown to reduce token consumption by **84%** in a 100-turn web search evaluation without breaking task completion. This is the highest-ROI first step before full summarization.

### 4.3 Manual `/compact` Command

Summarizes the entire conversation and starts fresh with the summary preloaded. Equivalent to what nanosec could expose as a `/reset` or `/compact` user command.

### 4.4 CLAUDE.md as Pinned Context

CLAUDE.md is always loaded fresh, surviving compaction. Persistent rules and facts live there rather than in conversation history.

### 4.5 Sub-Agent Isolation

Each sub-agent operates in a fresh context window and returns only a 1,000–2,000 token summary to the parent. Prevents cascading context bloat from exploration-heavy tasks.

---

## 5. OpenAI Agents SDK

### 5.1 Built-in Auto-Truncation

`ModelSettings.truncation = "auto"`: Oldest messages are dropped first. No summarization. No continuity for dropped content.

`truncation = "disabled"`: Raises `context_length_exceeded` error immediately. Used when you prefer to fail fast.

### 5.2 Summary Buffer Pattern

Monitor `usage.prompt_tokens` in the response. When it exceeds `SUMMARY_TRIGGER` and there are more than `KEEP_LAST_TURNS` turns, a background summarization coroutine fires. Older turns are summarized into a single assistant message; recent verbatim turns are kept.

### 5.3 Token Budget Allocation (Guidance)

Recommended split for an 8K token model:
- ~1K: system instructions
- ~1K: recent conversation
- ~5K: tool outputs / retrieved knowledge
- ~1K: margin for model output

---

## 6. General Strategies Compared

| Strategy | LLM call needed | Continuity | Complexity | Best for |
|---|---|---|---|---|
| **Sliding window** (keep last N) | No | None — hard cut | Minimal | Simple Q&A bots |
| **Tool result pruning** (drop oldest tool outputs first) | No | Good — semantic flow preserved | Low | Tool-heavy agents |
| **Summary buffer hybrid** (summarize old turns, keep last N) | Yes (on trigger) | Good — summary preserves key facts | Medium | Conversational agents |
| **Hierarchical memory** (working / episodic / semantic tiers) | Yes | Excellent | High | Production, long-term relationships |
| **Sub-agent isolation** (each sub-agent has fresh context) | No | Excellent — by design | Medium | Exploration / research tasks |
| **RAG / vectorized memory** | Yes (embedding + retrieval) | Excellent for recurring topics | High | FAQ bots, large knowledge bases |

---

## 7. Token Counting Options for Go

| Option | Accuracy | Cost | Notes |
|---|---|---|---|
| `len(text)/4` heuristic | ±15–25% (worse for CJK/code) | Zero | Used by PicoClaw |
| `tiktoken-go` (`github.com/pkoukk/tiktoken-go`) | Exact for OpenAI models | ~5MB binary overhead | Native Go BPE port |
| API-reported `usage.prompt_tokens` | Exact | Retroactive (after the spend) | Nanosec discards this today — `ChatResponse` has no `Usage` field |
| Anthropic token counting API | Exact | Extra HTTP round-trip | `/v1/messages/count_tokens` endpoint |

**Note:** Nanosec's `provider.ChatResponse` currently has no `Usage` field, so API-reported token counts are being silently discarded.

---

## 8. Recommended Implementation for Nanosec

### Minimum Viable (PicoClaw pattern)

1. Add `Summary string` and `ContextWindow int` to `Session` struct
2. Add `TruncateHistory(keepLast int)` to `Session`
3. Add `estimateTokens(msgs []provider.Message) int` (`len(content)/4` heuristic)
4. Add `summarizeSession()` in `agent/loop.go` — calls the LLM with a summarization prompt at `temperature: 0.3, max_tokens: 1024`
5. Inject `session.Summary` into the system prompt in `agent/context.go`
6. Call `maybeSummarize()` after each user turn: trigger at `len(history) > 20 || estimateTokens > contextWindow * 0.75`
7. Use `sync.Map` to deduplicate concurrent summarization goroutines

### Recommended Enhancements

8. Add `Usage` to `provider.ChatResponse` and expose `prompt_tokens` — use API-reported counts to tune the heuristic trigger
9. Cap individual tool results at a configurable max (e.g., 8,000 chars) before adding to session — highest ROI, zero extra LLM calls
10. Add `ContextWindow int` to agent config (maps to the model's actual context size: 128K for GPT-4o, 200K for Claude)
11. Expose a `/compact` or `/reset` user command for manual compaction

---

## Sources

- [Compaction — Claude API Docs](https://platform.claude.com/docs/en/build-with-claude/compaction)
- [Context editing — Claude API Docs](https://platform.claude.com/docs/en/build-with-claude/context-editing)
- [Effective context engineering for AI agents — Anthropic](https://www.anthropic.com/engineering/effective-context-engineering-for-ai-agents)
- [Claude Code context compaction — ClaudeLog FAQ](https://claudelog.com/faqs/what-is-claude-code-auto-compact/)
- [Context Window Management Strategies — Maxim AI](https://www.getmaxim.ai/articles/context-window-management-strategies-for-long-context-ai-agents-and-chatbots/)
- [Context Engineering for OpenAI Agents SDK — OpenAI Cookbook](https://cookbook.openai.com/examples/agents_sdk/session_memory)
- [tiktoken-go — native Go tokenizer](https://github.com/pkoukk/tiktoken-go)
- [Effective harnesses for long-running agents — Anthropic](https://www.anthropic.com/engineering/effective-harnesses-for-long-running-agents)
