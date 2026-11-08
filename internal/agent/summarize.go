package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"gitee.com/kobeyoung81/nanosec/internal/messageid"
	"gitee.com/kobeyoung81/nanosec/internal/provider"
	"gitee.com/kobeyoung81/nanosec/internal/session"
)

// summarizer handles async session summarization to avoid duplicate work.
type summarizer struct {
	provider provider.Provider
	model    string
	// reasoningEffort caps the model's thinking for these mechanical
	// non-streaming calls; empty = never send reasoning_effort, "none" =
	// turn thinking off.
	reasoningEffort string
	systemPrompt    string
	active          sync.Map // sessionID -> struct{}, prevents concurrent summarization
	// agent, when set, routes these calls through the unified utility registry.
	agent *Agent
}

func newSummarizer(p provider.Provider, model, reasoningEffort string) *summarizer {
	return &summarizer{provider: p, model: model, reasoningEffort: reasoningEffort, systemPrompt: summarizeSystemPrompt}
}

// SetSystemPrompt overrides the summarizer system prompt (platform role type).
func (s *summarizer) SetSystemPrompt(p string) {
	if p != "" {
		s.systemPrompt = p
	}
}

func (s *summarizer) prompt() string {
	if s.systemPrompt != "" {
		return s.systemPrompt
	}
	return summarizeSystemPrompt
}

// requestTimeout bounds a streamed utility call using the owning agent's
// configured LLM request timeout (0 = unbounded when no agent is attached).
func (s *summarizer) requestTimeout() time.Duration {
	if s.agent != nil {
		return s.agent.config.LLMRequestTimeout
	}
	return 0
}

// summarizeSystemPrompt is the fallback summarizer prompt, kept identical to the
// bundled role_summarize prompt (the database copy is authoritative in production).
const summarizeSystemPrompt = `# Conversation summarizer

You are a conversation summarizer for a coding agent. Given a conversation, produce a concise summary that preserves:

1. The user's goal, and any explicit requirement or constraint they stated.
2. Decisions that were made, and why.
3. Concrete artefacts: file paths, symbols, commands, plan files, branch names, commit ids, version numbers.
4. Work completed versus work still pending, including the current next step.
5. Open questions, and anything the user asked to be remembered.

Preserve exact identifiers rather than paraphrasing them — a summary that loses the file path or the command is useless to the agent that reads it. Write it in the language of the conversation. Output only the summary, no preamble.`

// summaryBudget carries the per-turn inputs the compaction gate needs.
type summaryBudget struct {
	ContextWindow   int
	MaxOutputTokens int
	TriggerPercent  int
	GrowthThreshold int
	// SystemTokens is the estimated size of this turn's system prompt. It is part
	// of every request, so it counts against the budget alongside the messages.
	SystemTokens int
}

// maybeSummarize triggers an automatic summary when the size of the next
// request approaches the available context budget. It is token-budget-driven
// rather than count-driven so the injected summary (the second element of the
// prompt prefix) is rewritten as rarely as possible, preserving provider prompt
// caching. It only fires once there are new messages since the last summary and
// enough growth has accumulated (growthThreshold) to avoid re-summarizing on
// every tool-loop iteration.
//
// The gate measures what the next request would actually cost, through the same
// helper the request builder uses (promptTokens: the conversation after the
// compaction floor, bounded to the budget, plus the pinned prefix and the
// system prompt), and prefers the provider's own prompt-token count over the
// local heuristic when one is known. Previously it compared the windowed
// transcript against the full budget, so a long conversation (where truncation
// pins the window at ~budget) re-fired on almost every turn, and a catalog
// max_output_tokens set to the model's capability rather than a per-request
// reservation shrank the budget to ~46 % of the context window. Both are why
// compaction appeared to fire "at low usage".
//
// It returns the summary the turn now holds (empty when the gate did not fire),
// so the turn's completion can carry it to the client.
func (s *summarizer) maybeSummarize(ctx context.Context, sess *session.Session, b summaryBudget, ref *messageRef) string {
	msgs := sess.ConversationMessages()
	if len(msgs) == 0 {
		return ""
	}
	lastCount := summaryCount(sess)
	if lastCount >= len(msgs) {
		return "" // nothing new since the last summary
	}
	budget, threshold := compactionPlan(b.ContextWindow, b.MaxOutputTokens, b.TriggerPercent)
	size := promptTokens(sess, budget, b.SystemTokens)
	if real := lastPromptTokens(sess); real > size {
		size = real
	}
	if size < threshold {
		return ""
	}
	// Growth watermark: once a summary exists, only rewrite it after at least
	// `growthThreshold` new messages accumulate. A threshold <= 1 disables the
	// gate (the forced overflow path relies on that). This is checked after the
	// size gate so the one suppression worth reporting — the request is over the
	// threshold and only the watermark is holding the compaction back — gets a
	// log line: a watermark set wider than the context can afford used to be
	// indistinguishable from a gate that never fires.
	if b.GrowthThreshold > 1 && lastCount > 0 && len(msgs)-lastCount < b.GrowthThreshold {
		slog.Warn("auto-compaction suppressed by the growth watermark",
			"session", sess.ID, "estimated_tokens", size, "threshold", threshold,
			"new_messages", len(msgs)-lastCount, "growth_threshold", b.GrowthThreshold)
		return ""
	}
	slog.Info("auto-compaction triggered",
		"session", sess.ID, "estimated_tokens", size, "threshold", threshold,
		"budget", budget, "context_window", b.ContextWindow, "floor", compactFrom(sess))
	return s.compactDuringTurn(ctx, sess, len(msgs), ref)
}

// forceCompact compacts everything the session holds, without the budget gate.
// The overflow path uses it: the provider already rejected the request for
// length, so there is nothing left to measure. It reports the summary the turn
// now holds.
func (s *summarizer) forceCompact(ctx context.Context, sess *session.Session, ref *messageRef) string {
	return s.compactDuringTurn(ctx, sess, len(sess.ConversationMessages()), ref)
}

// compactDuringTurn is the one compaction routine that runs inside a turn
// (automatic, forced, or the /compact command's own sequence): announce it,
// mark the session, flush durable context to memory, summarize up to `upTo`
// while streaming the summary, then write the ephemeral divider the transcript
// renders.
//
// Every chunk is posted to `ref` — the message this turn belongs to — never to
// the agent-wide lastMessage: the agent processes several messages at once, so
// the shared slot can hold another conversation by the time the summary is out
// (which used to show a compaction in the wrong conversation, or nowhere).
//
// It returns the summary now stored on the session: the freshly written one, or
// the existing one when there was nothing new to summarize.
func (s *summarizer) compactDuringTurn(ctx context.Context, sess *session.Session, upTo int, ref *messageRef) string {
	// Announce before summarizing so the client opens the pulsing divider that
	// the streamed summary then fills in — compaction used to be silent.
	s.emitStatusTo(ref, "compaction", "started")
	// Mark the compaction for clients that read the session mid-flight: the
	// transcript's own divider is only written when the summary is in.
	sess.SetMeta(compactingMetaKey, "true")
	// Before lossy compression, flush durable context to long-term memory.
	s.preCompactionFlush(ctx, sess)
	summaryID := messageid.New()
	summary := s.summarizeUpToStream(ctx, sess, upTo, func(delta string) {
		s.emitSummaryTo(ref, summaryID, delta)
	})
	sess.DeleteMeta(compactingMetaKey)
	if summary == "" {
		// Nothing new to summarize, or the call failed: the session's own
		// summary is still what the transcript should show, and the caller's
		// completion carries it (that is what makes a client that never saw a
		// summary chunk show the result anyway).
		slog.Warn("compaction produced no new summary", "session", sess.ID,
			"up_to", upTo, "summary_count", summaryCount(sess))
		s.emitStatusTo(ref, "compaction", "failed")
		return sess.GetSummary()
	}
	// Persist the same ephemeral divider the /compact command writes, so the
	// compaction is visible in the transcript (click-to-reveal the summary).
	sess.AddMessage(provider.Message{Role: "compaction", Content: summary})
	// Drop the summarized prefix from THIS turn's request. Without the floor the
	// compacting turn still sent the whole transcript plus the fresh summary —
	// the request the compaction was triggered for kept exactly the size that
	// set it off, and only the next turn's segment split shed the history. The
	// floor resumes at a user message so no tool call or result is orphaned.
	if floor := floorOfTurnStart(sess.ConversationMessages(), upTo); floor > 0 {
		sess.SetMeta(compactFromMetaKey, strconv.Itoa(floor))
	} else {
		slog.Info("compaction has no floor: the segment holds a single turn", "session", sess.ID)
	}
	// Re-read the session files into the head: the floor just dropped the tail
	// update rows that carried a file's new content in earlier turns, so the head
	// has to carry the files' current text or the rest of this turn would run on a
	// stale snapshot (see refreshHeadSnapshot). Memory flushed above is part of it.
	if s.agent != nil {
		if moved := s.agent.refreshHeadSnapshot(sess); len(moved) > 0 {
			slog.Info("session files re-snapshotted at compaction", "session", sess.ID, "files", moved)
		}
	}
	// The segment itself is closed at the start of the next turn (see
	// processMessage): splitting mid-turn would leave this turn's user message
	// behind in the old segment. The divider above is the visible boundary and
	// stays with the archiving segment.
	sess.SetMeta(splitPendingMetaKey, "true")
	s.emitStatusTo(ref, "compaction", "applied")
	return summary
}

// emitStatusTo surfaces a compaction lifecycle event through the owning agent
// to one specific chat (no-op when the summarizer has no agent, e.g. in tests).
func (s *summarizer) emitStatusTo(ref *messageRef, event, state string) {
	if s.agent != nil {
		s.agent.emitStatusTo(ref, event, state)
	}
}

// emitSummaryTo posts one streamed summary chunk through the owning agent (no-op
// when the summarizer has no agent).
func (s *summarizer) emitSummaryTo(ref *messageRef, messageID, content string) {
	if s.agent != nil {
		s.agent.postSummaryChunk(ref, messageID, content)
	}
}

// lastPromptTokens returns the provider-reported prompt tokens of the most
// recent LLM call in the session (0 when unknown).
func lastPromptTokens(sess *session.Session) int {
	raw := sess.GetMeta(lastPromptTokensMetaKey)
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// summarizeUpTo incrementally summarizes the LLM-facing messages in
// [summaryCount, upTo) using the existing summary as the preamble, then records
// upTo as the new summary cursor. upTo is an index into ConversationMessages().
// Guarded by a per-session lock so concurrent summarization is impossible. It
// returns the new summary, or "" when there was nothing to do or it failed.
func (s *summarizer) summarizeUpTo(ctx context.Context, sess *session.Session, upTo int) string {
	return s.summarizeUpToStream(ctx, sess, upTo, nil)
}

// summarizeUpToStream is summarizeUpTo with an optional delta sink for streaming
// the summary to the client (nil = collect only).
func (s *summarizer) summarizeUpToStream(ctx context.Context, sess *session.Session, upTo int, onDelta func(string)) string {
	msgs := sess.ConversationMessages()
	if upTo > len(msgs) {
		upTo = len(msgs)
	}
	lastCount := summaryCount(sess)
	if upTo <= lastCount {
		return "" // nothing new to summarize
	}

	sessionID := sess.ID
	if _, loaded := s.active.LoadOrStore(sessionID, struct{}{}); loaded {
		return "" // already being summarized
	}
	defer s.active.Delete(sessionID)

	slog.Info("summarizing session", "session", sessionID, "from", lastCount, "to", upTo)

	// Feed only the messages added since the last summary (plus the existing
	// summary as the preamble) so repeated compaction stays cheap and yields a
	// stable summary across iterations.
	summary, err := s.summarizeMessages(ctx, msgs[lastCount:upTo], sess.GetSummary(), sessionID, onDelta)
	if err != nil {
		slog.Error("summarization failed", "session", sessionID, "error", err)
		return ""
	}

	sess.SetSummary(summary)
	sess.SetMeta(session.SummaryCountMetaKey, strconv.Itoa(upTo))

	// NOTE: the stored transcript is kept intact (no truncation) so the full
	// conversation history survives for display. The LLM context is bounded
	// separately in loop.go (compaction floor + truncateToBudget).

	slog.Info("summarization complete", "session", sessionID, "summary_len", len(summary), "cursor", upTo)
	return summary
}

// summaryCount returns the conversation count recorded at the last successful
// summarization, or 0 when the session has never been summarized.
func summaryCount(sess *session.Session) int {
	raw := sess.GetMeta(session.SummaryCountMetaKey)
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// summarizeMessages calls the LLM to summarize messages.
func (s *summarizer) summarizeMessages(ctx context.Context, msgs []provider.Message, existingSummary, sessionID string, onDelta func(string)) (string, error) {
	var contentParts []string

	if existingSummary != "" {
		contentParts = append(contentParts, "Previous summary:\n"+existingSummary+"\n\n---\n\nNew conversation since last summary:")
	}

	for _, m := range msgs {
		switch m.Role {
		case "user":
			contentParts = append(contentParts, fmt.Sprintf("User: %s", m.Content))
		case "assistant":
			content := m.Content
			if len(m.ToolCalls) > 0 {
				var toolNames []string
				for _, tc := range m.ToolCalls {
					toolNames = append(toolNames, tc.Name)
				}
				content += fmt.Sprintf(" [used tools: %s]", strings.Join(toolNames, ", "))
			}
			contentParts = append(contentParts, fmt.Sprintf("Assistant: %s", content))
		case "tool":
			// Abbreviate tool results in summary input
			result := m.Content
			if len(result) > 500 {
				result = result[:500] + "..."
			}
			contentParts = append(contentParts, fmt.Sprintf("Tool result: %s", result))
		}
	}

	resp, used, err := s.utilityCall(ctx, "role_summarize", strings.Join(contentParts, "\n"), sessionID, onDelta)
	if err != nil {
		return "", fmt.Errorf("summarize LLM call: %w", err)
	}
	if used {
		return resp, nil
	}
	r, err := chatStreamCollectWith(ctx, s.provider, provider.ChatRequest{
		Model:           s.model,
		ReasoningEffort: s.reasoningEffort,
		SessionID:       sessionID + ":summarize",
		Messages: []provider.Message{
			{Role: "system", Content: s.prompt()},
			{Role: "user", Content: strings.Join(contentParts, "\n")},
		},
	}, s.requestTimeout(), onDelta)
	if err != nil {
		return "", fmt.Errorf("summarize LLM call: %w", err)
	}
	return r.Content, nil
}

// utilityCall routes a utility invocation through the unified registry when an
// owner agent is attached; it reports whether the registry handled the call.
func (s *summarizer) utilityCall(ctx context.Context, id, content, sessionID string, onDelta func(string)) (string, bool, error) {
	if s.agent == nil {
		return "", false, nil
	}
	return s.agent.runUtility(ctx, id, content, sessionID, onDelta)
}

// preCompactionFlush triggers the agent to write important context to memory
// before lossy compression occurs. This is the OpenClaw pattern.
func (s *summarizer) preCompactionFlush(ctx context.Context, sess *session.Session) {
	if sess.MemoryFlushed {
		return
	}

	sessionID := sess.ID
	slog.Info("pre-compaction memory flush", "session", sessionID)

	msgs := sess.ConversationMessages()
	if len(msgs) == 0 {
		return
	}

	// Build a prompt asking the LLM to identify what should be persisted
	var contentParts []string
	for _, m := range msgs {
		switch m.Role {
		case "user":
			contentParts = append(contentParts, fmt.Sprintf("User: %s", m.Content))
		case "assistant":
			contentParts = append(contentParts, fmt.Sprintf("Assistant: %s", m.Content))
		}
	}

	content, used, err := s.utilityCall(ctx, "utility_memory_flush", strings.Join(contentParts, "\n"), sessionID, nil)
	if !used {
		resp, cerr := chatStreamCollect(ctx, s.provider, provider.ChatRequest{
			Model:           s.model,
			ReasoningEffort: s.reasoningEffort,
			SessionID:       sessionID + ":summarize",
			Messages: []provider.Message{
				{Role: "system", Content: memoryFlushSystemPrompt},
				{Role: "user", Content: strings.Join(contentParts, "\n")},
			},
		}, s.requestTimeout())
		if cerr != nil {
			slog.Error("pre-compaction flush failed", "session", sessionID, "error", cerr)
			return
		}
		content = resp.Content
	} else if err != nil {
		slog.Error("pre-compaction flush failed", "session", sessionID, "error", err)
		return
	}

	if strings.TrimSpace(content) == "NOTHING_TO_SAVE" {
		slog.Debug("pre-compaction flush: nothing to save", "session", sessionID)
	} else {
		// Store flush result in session metadata and persist it to long-term
		// memory immediately so a later turn (or a session that ends first) can
		// never lose the extracted facts.
		sess.SetMeta("_pending_memory_flush", content)
		if s.agent != nil {
			s.agent.persistMemoryFlush(sess)
		}
		slog.Info("pre-compaction flush: pending write", "session", sessionID, "content_len", len(content))
	}

	sess.MemoryFlushed = true
}

// memoryFlushSystemPrompt is the fallback memory-flush prompt, kept identical to
// the bundled utility_memory_flush prompt. Note the NOTHING_TO_SAVE sentinel: the
// runtime compares against it exactly.
const memoryFlushSystemPrompt = `# Memory flush

You are about to lose older conversation context because it is being compressed. Review this conversation and extract everything that must survive:

- decisions and their reasons;
- user preferences, constraints and stated norms;
- project facts: paths, commands, versions, environment details;
- unfinished work, with its next concrete step.

Produce short bullet points to append to a MEMORY.md file: it is read again on every future turn, so keep each line terse. Extract only durable facts — do not summarise the conversation. Write in the language of the conversation. No secrets unless the user asked for them to be kept. If nothing qualifies, respond with exactly: NOTHING_TO_SAVE`
