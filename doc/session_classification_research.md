# Session Classification & Segmentation Research

**Date**: 2026-03-08
**Status**: Research Complete

---

## 1. Motivation

`doc/context_research.md` covers **context compression** — reducing a long session's token footprint in place (summarization, sliding windows, tool-result pruning). This document covers a complementary and more aggressive strategy: **proactive session segmentation**.

The idea: if the agent detects that a user has switched topics or tasks, it starts a fresh context window for the new topic instead of appending to the existing one. The old topic's tokens are never sent to the LLM again.

| Strategy | Mechanism | Max Token Savings |
|---|---|---|
| Summarization (existing research) | Compress history in-place | 60–70% of old tokens |
| Tool-result pruning (existing research) | Drop oldest tool outputs | 84% in tool-heavy tasks |
| **Session segmentation (this doc)** | Old topic context never sent | **100% of old topic tokens** |

These are complementary. Compression handles the within-session growth problem. Segmentation handles the between-topic cross-contamination problem.

---

## 2. Current State: Reference Implementations

Both reference implementations use the same session model:

```
session key = channel + ":" + chat_id
```

**Session boundaries are 100% identity-based.** The agent has no awareness of whether the user switched from "help me refactor this Go function" to "book a flight to Tokyo". Both messages land in the same JSONL history. There is zero topic classification before the LLM call.

This is the same default across all major frameworks:

| System | Session Basis | Auto Topic Detection |
|---|---|---|
| OpenAI Assistants | `thread_id` | No |
| OpenAI Agents SDK | Explicit session object | No |
| LangGraph | Checkpoint thread | No (custom graph edges required) |
| AutoGen | `AgentSession` | No (custom auto-reply required) |
| Claude Code | File-backed history | No (only `/compact` + auto-compaction) |
| picoclaw | `channel:chat_id` | No |
| nanobot | `channel:chat_id` | No |
| nanosec (current) | `channel:chat_id` | No |

**The industry default: sessions are platform-scoped, not topic-scoped.** Context pressure is handled by compression, not segmentation. Automatic topic detection is left to the developer and is almost never implemented in lightweight agents.

---

## 3. Session Lifecycle in Reference Implementations

### 3.1 Session Key Formation

**picoclaw**:
```go
sessionKey := fmt.Sprintf("%s:%s", c.name, chatID)
msg := bus.InboundMessage{
    Channel:    c.name,
    SenderID:   senderID,
    ChatID:     chatID,
    SessionKey: sessionKey,
}
```

**nanobot**:
```python
@property
def session_key(self) -> str:
    return f"{self.channel}:{self.chat_id}"
```

### 3.2 Session Storage

Both implementations use the same two-tier model:
- **In-memory cache**: `map[string]*Session` (Go) / `dict[str, Session]` (Python) — fast lookup
- **Disk persistence**: JSON files (picoclaw) / JSONL files (nanobot) — survive restarts

### 3.3 Explicit Reset

nanobot's Telegram adapter handles `/reset`:
```python
async def _on_reset(self, update, context):
    session_key = f"{self.name}:{chat_id}"
    session = self.session_manager.get_or_create(session_key)
    session.clear()
    self.session_manager.save(session)
```

This is the **only** session boundary mechanism in either reference implementation. There is no implicit/automatic reset.

### 3.4 Key Gap

The session key is a **flat, unversioned string**. There is no way to start a new session for the same user without wiping the old one. Adding session versioning (e.g., `feishu:12345:v2`) is the foundational primitive needed for all session segmentation approaches described below.

---

## 4. Session/Topic Classification Techniques

### 4.1 Time-Gap Heuristics (Free, Low Accuracy)

The most widely deployed production approach. No LLM, no embedding — just a timer. A long idle gap is a strong signal that the next message opens a new topic.

| Product | Idle Threshold → Action |
|---|---|
| Amazon Lex | 5 min → new session |
| Dialogflow | 30 min → reset to fallback intent |
| Zendesk | 10 min inactive, 20 min → session end |
| OpenAI Realtime API | Configurable `idle_timeout_ms` |
| Microsoft Bot Framework | Per-channel `ActivityTypes.EndOfConversation` |

**Strengths:** Zero cost, trivial to implement, handles the "picked up next day" case perfectly.
**Weaknesses:** Misses mid-session topic switches that happen rapidly. Threshold is arbitrary.

```go
// Sketch for nanosec
type Session struct {
    Key          string
    Messages     []provider.Message
    Summary      string
    LastActivity time.Time
}

func (s *Session) IsStale(threshold time.Duration) bool {
    return time.Since(s.LastActivity) > threshold
}
```

### 4.2 Explicit User Commands (Free, Perfect When Used)

User-initiated. No heuristic can match the precision of the user explicitly saying "new topic".

Standard commands across the ecosystem: `/new`, `/reset`, `/clear`, `/compact`, `/topic`.

nanobot already implements `/reset` on Telegram. The gap is that it **wipes** the old session rather than **archiving** it. A better model is to version the session key so the old history is preserved but no longer in the active context.

**Strengths:** Zero cost, 100% accurate when the user bothers to use it.
**Weaknesses:** Requires user discipline. Most users will not use it.

### 4.3 Embedding Cosine Similarity — DriftOS Pattern (Sub-200ms, Zero LLM Cost)

The most directly applicable open-source implementation. Uses a lightweight sentence embedding model (paraphrase-MiniLM-L6-v2, 22M params, ~30ms inference) to compare each incoming message against the running semantic centroid of the current session.

```
cosine_similarity(embed(new_message), session_centroid):
  > 0.42  → STAY: same topic, continue session
  > 0.38  → ROUTE: related subtopic, stay or merge
  < 0.15  → NEW CLUSTER: entirely new domain, start fresh session
  else    → AMBIGUOUS: soft topic shift, fall through to next layer
```

The centroid is a running average of all message embeddings in the session. Q&A-style messages get a 1.3× similarity boost (questions are topically flexible and should not trigger false positives).

When the threshold crossing fires, the system branches to a new session key while keeping the old one accessible. The new session can optionally receive a one-sentence handoff: "Previous session summary: ..."

**Go implementation options:**
- `onnxruntime-go` — run MiniLM locally via ONNX, ~5MB model file
- `cloudwego/eino` — LangChain-style Go framework with built-in embedding integrations
- Fallback: `len(text)/4` character hashing for a very rough centroid (no accuracy guarantees)

**Strengths:** Zero API cost, works in real time, no LLM round-trip.
**Weaknesses:** ~5MB binary overhead for the embedding model. Worse for Chinese/CJK (embedding models trained primarily on English). Cannot distinguish synonyms from antonyms reliably.

### 4.4 LLM-Based Pre-Filter Router (Accurate, +500–2000ms, +Cost)

Before the main LLM call, send a cheap classification prompt to a small/fast model:

```
Given the following context, classify the user's new message.

Current session summary (last ~200 tokens):
<summary>

New user message:
<message>

Is this message:
A) A continuation of the current topic
B) A new, unrelated topic — start a fresh session
C) Ambiguous — continue current session with caution

Answer with A, B, or C only.
```

Use GPT-4o-mini, Haiku, or any small model to minimize cost and latency. Only trigger this as a second-stage filter when the embedding-based approach returns an ambiguous score.

**Two-phase hybrid (recommended):**
1. Embedding similarity → fast, free (covers 80%+ of cases clearly)
2. LLM classifier → accurate, slower (covers the ambiguous middle band)

**Strengths:** Very high accuracy. Works well for nuanced, domain-specific topic shifts.
**Weaknesses:** Adds latency and API cost to every ambiguous turn. Requires an LLM call before the main call.

### 4.5 SegLLM Two-Phase Architecture (Offline + Online)

A research-grade approach from ICASSP 2024.

- **Offline phase:** LLM generates synthetic sentences for each known topic domain, building semantic distribution models.
- **Online phase:** Real-time messages are classified against pre-built distributions at low cost — no LLM call needed at inference time.

Best suited for domain-specific deployments where the set of possible topics is known in advance (e.g., a customer service bot with fixed intents).

**Strengths:** Low online latency, high accuracy for known domains.
**Weaknesses:** Requires offline preparation. Fails on out-of-distribution topics.

### 4.6 Knowledge Graph Memory (Zep / A-MEM Pattern)

Instead of partitioning history into isolated sessions, build a graph of entities and facts. Topic "sessions" emerge naturally as graph communities — the user's conversation about Go programming and their conversation about travel bookings cluster into separate subgraphs, sharing nodes only where the topics genuinely intersect.

**Zep** (temporal knowledge graph):
- Stores conversation as: episode subgraph → semantic entity subgraph → community subgraph
- Each message updates the graph, resolving new entities against existing ones
- Supports multiple "sessions" sharing the same entity graph (user-level memory)
- Outperforms MemGPT: 94.8% vs 93.4% on DMR benchmark, 90% latency reduction on LongMemEval
- Go SDK available

**A-MEM** (Zettelkasten note structure, NeurIPS 2025):
- Each memory is a "note" with keywords, tags, and contextual description
- LLM-driven link generation between related notes
- New experiences retroactively refine existing notes

**Strengths:** Best long-term accuracy. Cross-topic facts are retained naturally. No session boundary decision needed.
**Weaknesses:** Very high complexity. Significant infrastructure. Incompatible with nanosec's <5,000 LOC target for the core.

---

## 5. When to Start a New Context vs. Continue

The field has converged on seven signals that indicate a context break, in priority order:

| Priority | Signal | Type | Cost |
|---|---|---|---|
| 1 | Explicit user command (`/new`, `/reset`) | Hard | Free |
| 2 | Token budget ≥ ~75–98% of context window | Hard | Free (char/4 heuristic) |
| 3 | Message count ≥ threshold (20–50 msgs) | Hard | Free |
| 4 | Time gap ≥ idle threshold (5–60 min) | Soft | Free |
| 5 | Embedding cosine < drift threshold (<0.15) | Soft | ~30ms local |
| 6 | Task completion milestone reached | Soft | Domain logic |
| 7 | Context poisoning (error cascade detected) | Soft | LLM call |

**Signal 7 — Context poisoning** deserves special attention. Identified by Anthropic's context engineering research: when a hallucination or error from an early step propagates through all subsequent reasoning steps, the correct response is to start a new context window with a clean summary rather than continuing to compress a poisoned history. This is qualitatively different from the other signals — it is about correctness, not just efficiency.

### Decision Tree

```
Incoming message
       │
       ▼
[1] Explicit /new command? ──YES──► Archive session, start fresh
       │NO
       ▼
[2/3] Tokens > 75% OR msgs > 20? ──YES──► maybeSummarize() in-place (existing logic)
       │NO                                  (or start fresh if also matches signal 4)
       ▼
[4] Time gap > idle threshold? ──YES──► Archive session, start fresh
       │NO
       ▼
[5] Embedding cosine < 0.15? ──YES──► Branch new session (keep old accessible)
       │NO/AMBIGUOUS
       ▼
[LLM] Classify A/B/C? ──B──► Branch new session
       │A or C
       ▼
Continue current session
```

---

## 6. Multi-Session Management

### 6.1 Parallel Session Patterns

**Thread-per-topic** (LangGraph, OpenAI Assistants):
- Each topic gets its own `thread_id` or checkpoint thread
- Long-term cross-topic memory stored in a separate namespace store
- Sessions are isolated; shared facts go through the cross-session store

**Subagent isolation** (Claude Code, picoclaw):
- Each subagent runs in its own fresh context window
- Main agent maintains a thin conversation; subagents are stateless workers
- Results return as condensed summaries (1,000–2,000 tokens)
- 67% fewer tokens vs. keeping everything in one context window

**Namespace-keyed memory** (Zep, MemGPT/Letta):
- Memory namespaced as `user/{user_id}/topic/{topic}` or similar
- Topic detection feeds namespace selection
- Queries retrieve from the relevant namespace via semantic search

### 6.2 Session Key Versioning (Required Primitive)

The current flat key (`channel:chat_id`) must be versioned to support segmentation without data loss:

```
Current:  feishu:12345
Versioned: feishu:12345:2026-03-08T10:30:00Z

Or:        feishu:12345:v1   (original session)
           feishu:12345:v2   (new topic, started automatically)
           feishu:12345:v3   (another new topic)
```

The session store then needs:
- `ActiveSession(userID, channel, chatID) → SessionKey` — returns the current live key
- `ArchiveSession(key)` — mark as closed, keep on disk
- `NewSession(userID, channel, chatID) → SessionKey` — create versioned key, set as active
- `ListSessions(userID, channel, chatID) → []SessionKey` — for `/history` command

### 6.3 Parallel Session Pitfalls

**Context drift** in parallel agents is a major production problem:
- Each parallel agent may develop conflicting assumptions about shared state
- Solution: shared task spec (single source of truth), summarization before handoff, context scoping

---

## 7. Token Savings Comparison

| Approach | Token Reduction | LLM Calls | Complexity |
|---|---|---|---|
| Tool result pruning | **84%** for tool-heavy tasks | 0 extra | Low |
| Summarization (in-place) | 60–70% compression | +1 per trigger | Medium |
| Time-gap auto-session | 100% of old topic tokens | 0 extra | Trivial |
| Explicit `/new` command | 100% of old topic tokens | 0 extra | Trivial |
| Embedding cosine branch | 100% of old topic tokens | 0 extra (local model) | Medium |
| LLM pre-filter router | 100% of old topic tokens | +1 per turn (small model) | Medium |
| Subagent isolation | 67% vs single context | 0 extra | Medium |

Starting a new session is the most aggressive token reduction for the new topic — the LLM literally never sees the old conversation. The trade-off is loss of cross-topic context awareness.

---

## 8. Go-Specific Implementation Notes

### Token Counting Options

| Option | Accuracy | Cost | Notes |
|---|---|---|---|
| `len(text)/4` heuristic | ±15–25% (worse for CJK) | Zero | Used by picoclaw today |
| `tiktoken-go` (`github.com/pkoukk/tiktoken-go`) | Exact for OpenAI | ~5MB binary | Native Go BPE port |
| API-reported `usage.prompt_tokens` | Exact | Retroactive | nanosec discards this today |

### Embedding Options

| Option | Model Size | Latency | Notes |
|---|---|---|---|
| `onnxruntime-go` + MiniLM-L6 | ~22MB | ~30ms | Best quality/size trade-off |
| `cloudwego/eino` embeddings | Varies | API round-trip | If local model is too heavy |
| Character hash centroid | 0 | <1ms | Extremely rough, not recommended |

### Recommended Libraries

| Library | Purpose |
|---|---|
| `onnxruntime-go` | Local ONNX inference for embedding model |
| `cloudwego/eino` | Go LangChain alternative, multi-agent graph support |
| `getzep/zep-go` | Temporal knowledge graph memory (if heavy solution warranted) |

---

## 9. Recommended Implementation for Nanosec

### Layer 0 — Trivial (Add Now, Zero Dependencies)

1. **Session key versioning**: Change flat `channel:chat_id` to `channel:chat_id:timestamp`. Add `ActiveSession()`, `ArchiveSession()`, `NewSession()` to `session/store.go`.
2. **`/new` command**: Gateway-level command that calls `NewSession()` and confirms to the user. Analogous to nanobot's `/reset` but archives rather than wipes.
3. **Idle timeout**: Add `LastActivity time.Time` to `Session`. On next inbound message, if `time.Since(LastActivity) > idleThreshold` (configurable, default 30 min), auto-archive and start fresh.

### Layer 1 — Low Cost (Add With Context Compression Work)

4. **Time-aware summarization**: When `maybeSummarize()` triggers, also check the time gap. If gap > 4 hours, prefer starting a new session (carry summary as handoff) over compressing in-place. Handles the "morning after" case.
5. **`Usage` field on `ChatResponse`**: Expose `prompt_tokens` from the API response so token counting uses exact numbers instead of the char/4 heuristic. This improves all trigger thresholds.

### Layer 2 — Medium Complexity (Post-MVP)

6. **LLM continuity check (piggybacked)**: During the summarization call (which already has an LLM round-trip), ask the model to also score topic continuity:
   ```
   "After summarizing: on a scale of 0-10, how related is the most recent
   user message to the main theme of this conversation? (0 = completely new
   topic, 10 = direct continuation)"
   ```
   Score < 3 → branch new session. Zero added API cost since it's part of an existing call.

7. **Embedding-based drift detection**: Add an optional embedding centroid per session. Use `tiktoken-go` or `onnxruntime-go` + MiniLM-L6. Compute cosine similarity on each inbound message. Below threshold (0.15) → auto-branch. This enables real-time topic detection with zero LLM cost.

### Layer 3 — Heavy (Post-Launch, Optional)

8. **Cross-session semantic memory**: Integrate Zep or build a lightweight entity store. Cross-topic facts (user preferences, long-term goals) persist in a shared namespace injected into all sessions. This eliminates the main downside of session segmentation (loss of cross-topic awareness).

---

## 10. Open Source References

| Project | Language | Key Feature |
|---|---|---|
| [DriftOS/driftos-embed](https://github.com/DriftOS/driftos-embed) | Python | Embedding cosine topic routing, sub-200ms |
| [Letta (MemGPT successor)](https://www.letta.com/) | Python | Tiered virtual context management |
| [Zep](https://www.getzep.com/) | Python/Go | Temporal knowledge graph, Go SDK |
| [A-MEM](https://github.com/agiresearch/A-mem) | Python | Zettelkasten memory, NeurIPS 2025 |
| [openai/openai-agents-python](https://github.com/openai/openai-agents-python) | Python | Session backends, trim/compress utilities |
| [cloudwego/eino](https://github.com/cloudwego/eino) | Go | LangChain-style framework for Go |

---

## 11. Research Papers

1. **MemGPT: Towards LLMs as Operating Systems** — foundational virtual context management; OS paging analogy — [arxiv.org/abs/2310.08560](https://arxiv.org/abs/2310.08560)
2. **Zep: A Temporal Knowledge Graph Architecture for Agent Memory** — beats MemGPT, Go SDK — [arxiv.org/abs/2501.13956](https://arxiv.org/abs/2501.13956)
3. **A-MEM: Agentic Memory for LLM Agents** (NeurIPS 2025) — Zettelkasten notes, LLM-driven links — [arxiv.org/abs/2502.12110](https://arxiv.org/abs/2502.12110)
4. **SEGLLM: Topic-Oriented Call Segmentation via LLM-Based Conversation Synthesis** (ICASSP 2024) — offline/online two-phase — [IEEE 10446156](https://ieeexplore.ieee.org/document/10446156/)
5. **When F1 Fails: Granularity-Aware Evaluation for Dialogue Topic Segmentation** — DistilBERT boundary classifier — [arxiv.org/abs/2512.17083](https://arxiv.org/html/2512.17083v1)
6. **Fast Intent Classification for LLM Routing** — statistical analysis of LLM representations — [OpenReview](https://openreview.net/forum?id=UMuVvvIEvA)
7. **Acon: Optimizing Context Compression for Long-horizon LLM Agents** — 26–54% peak token reduction — [arxiv.org/abs/2510.00615](https://arxiv.org/html/2510.00615v1)
8. **Effective Context Engineering for AI Agents** — Anthropic engineering blog, context poisoning signal — [anthropic.com](https://www.anthropic.com/engineering/effective-context-engineering-for-ai-agents)

---

## Sources

- Anthropic: [Effective Context Engineering for AI Agents](https://www.anthropic.com/engineering/effective-context-engineering-for-ai-agents)
- Anthropic: [Compaction — Claude API Docs](https://platform.claude.com/docs/en/build-with-claude/compaction)
- OpenAI: [Sessions — Agents SDK](https://openai.github.io/openai-agents-python/sessions/)
- OpenAI: [Context Engineering for OpenAI Agents SDK](https://cookbook.openai.com/examples/agents_sdk/session_memory)
- LangChain: [Memory Overview — LangGraph](https://docs.langchain.com/oss/python/langgraph/memory)
- Zep: [Semantic Similarity as an Intent Router](https://blog.getzep.com/building-an-intent-router-with-langchain-and-zep/)
- Lumenalta: [8 Tactics to Reduce Context Drift with Parallel AI Agents](https://lumenalta.com/insights/8-tactics-to-reduce-context-drift-with-parallel-ai-agents)
- Quidget: [Chatbot Session Timeout Settings: Best Practices](https://quidget.ai/blog/ai-automation/chatbot-session-timeout-settings-best-practices/)
- DeepWiki: [Claude Code Context Window & Compaction](https://deepwiki.com/anthropics/claude-code/3.3-context-window-and-compaction)
