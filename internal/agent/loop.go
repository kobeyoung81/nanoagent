package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanosec/internal/messageid"
	"gitee.com/kobeyoung81/nanosec/internal/provider"
	"gitee.com/kobeyoung81/nanosec/internal/question"
	"gitee.com/kobeyoung81/nanosec/internal/session"
	"gitee.com/kobeyoung81/nanosec/internal/tool"
	"github.com/google/uuid"
)

// toolNames indexes tool definitions by name, for the caller policy handed to
// the delegation tools.
func toolNames(defs []provider.ToolDef) map[string]bool {
	out := make(map[string]bool, len(defs))
	for _, d := range defs {
		out[d.Name] = true
	}
	return out
}

// callLLM wraps chatStreamTurn with bounded retries for transient stream-open
// failures (429 / 5xx / network). Only open failures are retried — a
// mid-stream failure has already emitted chunks, and retrying would duplicate
// them.
func (a *Agent) callLLM(ctx context.Context, req provider.ChatRequest, msg *nanoprotocol.InboundMessage, messageID string) (*provider.ChatResponse, error) {
	// Inline any image attachments for the duration of this call. The session
	// row keeps only the path references.
	req.Messages = a.expandImages(req.Messages)
	delays := []time.Duration{time.Second, 3 * time.Second}
	for attempt := 0; ; attempt++ {
		resp, err := a.chatStreamTurn(ctx, req, msg, messageID)
		if err == nil {
			return resp, nil
		}
		if resp != nil {
			return resp, err
		}
		category, _ := provider.ClassifyProviderError(err, "")
		if !provider.RetryableTurnError(category) || attempt >= a.config.LLMMaxRetries || attempt >= len(delays) {
			return resp, err
		}
		delay := delays[attempt]
		slog.Warn("llm stream open failed; retrying",
			"attempt", attempt+1, "delay", delay, "category", category, "error", err)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// failTurn finalizes a failed turn: it persists an error marker into the
// session, posts exactly one completion carrying the failure category (plus
// any partial content), and returns a wrapped error for the caller's logs.
func (a *Agent) failTurn(sess *session.Session, msg *nanoprotocol.InboundMessage, messageID string, resp *provider.ChatResponse, err error, category string) error {
	category, detail := provider.ClassifyProviderError(err, category)
	var partial string
	if resp != nil {
		partial = resp.Content
	}
	if sess != nil {
		sess.AddMessage(provider.Message{
			Role:    "error",
			Content: category,
			Detail:  detail,
		})
		sess.SetTurnRunning(false)
		if serr := a.sessions.Save(sess); serr != nil {
			slog.Warn("failed to save session after turn error", "session", sess.ID, "error", serr)
		}
		a.sessions.SyncNow()
	}
	if perr := a.busClient.PostOutboundComplete(nanoprotocol.OutboundComplete{
		UserID:      a.config.UserID,
		AgentID:     a.config.AgentID,
		SessionID:   msg.SessionID,
		Channel:     msg.Channel,
		ChatID:      msg.ChatID,
		MessageID:   messageID,
		FullContent: partial,
		Error:       category,
		ErrorDetail: detail,
	}); perr != nil {
		slog.Warn("failed to post error completion", "error", perr)
	}
	return fmt.Errorf("llm turn failed (category=%s): %w", category, err)
}

// stopTurn finalizes a turn the user stopped: it clears the running flag,
// persists the session, and posts a completion carrying any partial content
// plus the "stopped" marker so clients can render a neutral note.
func (a *Agent) stopTurn(sess *session.Session, msg *nanoprotocol.InboundMessage, messageID string, resp *provider.ChatResponse) error {
	var partial string
	if resp != nil {
		partial = resp.Content
	}
	if sess != nil {
		sess.SetTurnRunning(false)
		if serr := a.sessions.Save(sess); serr != nil {
			slog.Warn("failed to save session after stop", "session", sess.ID, "error", serr)
		}
		a.sessions.SyncNow()
	}
	if perr := a.busClient.PostOutboundComplete(nanoprotocol.OutboundComplete{
		UserID:      a.config.UserID,
		AgentID:     a.config.AgentID,
		SessionID:   msg.SessionID,
		Channel:     msg.Channel,
		ChatID:      msg.ChatID,
		MessageID:   messageID,
		FullContent: partial,
		Error:       nanoprotocol.TurnErrorStopped,
	}); perr != nil {
		slog.Warn("failed to post stop completion", "error", perr)
	}
	return nil
}

// startTurnPersistence snapshots the session on a fixed cadence for the
// duration of a turn, so partial assistant content produced before a
// crash/disconnect is recoverable. The returned stop function is idempotent
// and blocks until the saver goroutine has exited.
func (a *Agent) startTurnPersistence(sess *session.Session) func() {
	const interval = 2 * time.Second
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if err := a.sessions.Save(sess); err != nil {
					slog.Debug("periodic session save failed", "session", sess.ID, "error", err)
				}
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(stop)
			<-done
		})
	}
}

// processMessage handles a single inbound message.
func (a *Agent) processMessage(ctx context.Context, msg *nanoprotocol.InboundMessage) error {
	// Active agent definition for this message: per-message metadata override,
	// else the agent's default session definition.
	spec := a.resolveSession(msg)
	// Apply the active mode's per-(agent, mode) runtime policy before the turn.
	a.applyModeSettings(spec)

	// Resolve the live segment before anything else touches the session: a
	// client holding an archived id (a stale tab, a retry that raced a
	// compaction) is moved forward along the version chain instead of appending
	// to a transcript the portal no longer shows.
	a.rememberMessage(msg)
	sess := a.resolveActiveSession(msg)

	// Propagate session context to tools that need it (e.g. CronTool, SpawnTool,
	// MediaUploadTool, plan_execute).
	a.tools.SetSessionContext(msg.SessionID, msg.Channel, msg.ChatID)
	a.tools.SetPlanPath(sess.GetMeta(lastPlanPathMetaKey))

	// A scheduled task's turn has nobody to answer a question; tools that would
	// block on a user must refuse instead (see tool.HeadlessTool).
	a.tools.SetHeadless(isScheduledTurn(msg))
	// A goal round's binding: which goal, which round, and whether this is the
	// verification round. Outside a goal round this is the zero value, so the
	// report tool refuses instead of writing into a stale goal.
	a.tools.SetGoalContext(goalContextOf(msg))

	// A task names its conversation on its first run: the task name is a better
	// entry in the conversation list than the truncated instruction, and a
	// preset title counts as a real one, so the title model does not overwrite
	// it on the first exchange.
	if name := scheduledTaskName(msg); name != "" && sess.MessageCount() == 0 {
		sess.SetMeta(titleMetaKey, name)
	}

	// Handle /new command — archive the current segment and start a new version.
	// History is kept: the old segment stays readable through the version chain,
	// and the client follows the new id carried by the completion below.
	if strings.TrimSpace(msg.Content) == "/new" {
		baseKey, _ := session.ParseSessionKey(sess.ID)
		sess = a.splitSession(ctx, sess, baseKey, msg, splitOnReset)
		a.clearSessionGrants(sess)
		if err := a.sessions.Save(sess); err != nil {
			slog.Warn("failed to save reset session", "session", sess.ID, "error", err)
		}
		return a.busClient.PostOutboundComplete(nanoprotocol.OutboundComplete{
			UserID:      a.config.UserID,
			AgentID:     a.config.AgentID,
			SessionID:   msg.SessionID,
			Channel:     msg.Channel,
			ChatID:      msg.ChatID,
			MessageID:   messageid.New(),
			FullContent: "New session started.",
		})
	}

	// Handle /compact — force context compaction now: flush memory, summarize
	// the prefix that will be dropped, keep the last few text messages, and reply
	// without running the ReAct loop. The segment itself is archived at the start
	// of the NEXT turn: splitting here would leave this command's reply behind in
	// the old segment or tear a later turn in half.
	if strings.TrimSpace(msg.Content) == "/compact" {
		ref := messageRefOf(msg)
		msgs := sess.ConversationMessages()
		floor := compactionFloor(msgs, compactKeepTextMessages)
		// The command counts as a turn as far as clients are concerned: the
		// memory flush and the summary are long inner LLM calls that emit nothing
		// to the client, and a client treats a session row that says "not running"
		// as a finished turn (it clears its busy state after a few seconds of
		// stream silence). Keep the flag lit and refreshed for the whole command.
		sess.SetTurnRunning(true)
		sess.SetMeta(compactingMetaKey, "true")
		if err := a.sessions.Save(sess); err != nil {
			slog.Warn("failed to save session at compaction start", "session", sess.ID, "error", err)
		}
		stopPersist := a.startTurnPersistence(sess)
		defer stopPersist()
		// Announce compaction before summarizing so the client opens a pulsing
		// "Compacting…" divider while the summary streams. Both the status and the
		// summary chunks go to THIS message's chat: the shared lastMessage can
		// point at another conversation while the command runs.
		a.emitStatusTo(ref, "compaction", "started")
		if a.summarizer != nil {
			a.summarizer.preCompactionFlush(ctx, sess)
			summaryMessageID := messageid.New()
			a.summarizer.summarizeUpToStream(ctx, sess, floor, func(delta string) {
				a.postSummaryChunk(ref, summaryMessageID, delta)
			})
		}
		// The compaction is over: stop the mid-turn saver first, then clear the
		// flags, save and sync — before the completion goes out, so a client that
		// re-reads the metadata on completion sees a finished turn.
		stopPersist()
		sess.DeleteMeta(compactingMetaKey)
		sess.SetTurnRunning(false)
		sess.SetMeta(compactFromMetaKey, strconv.Itoa(floor))
		sess.SetMeta(splitPendingMetaKey, "true")
		// Same head refresh the automatic path does: the floor above just dropped
		// the tail updates for files that moved earlier in this segment, and the
		// usage estimate below is about to report the context size to the client.
		if moved := a.refreshHeadSnapshot(sess); len(moved) > 0 {
			slog.Info("session files re-snapshotted at compaction", "session", sess.ID, "files", moved)
		}
		// Persisted ephemeral marker (excluded from the LLM context) renders as a
		// divider and carries the summary snapshot for click-to-reveal.
		sess.AddMessage(provider.Message{Role: "compaction", Content: sess.GetSummary()})
		if err := a.sessions.Save(sess); err != nil {
			slog.Warn("failed to save session after compaction", "session", sess.ID, "error", err)
		}
		a.sessions.SyncNow()
		a.emitStatusTo(ref, "compaction", "applied")
		a.emitUsageValues(msg, a.estimateUsage(sess, msg.Channel, msg.ChatID, spec.Prompt), a.config.ContextWindow)
		// The completion carries the summary: a client whose stream lost the
		// summary chunks (or whose agent summarized nothing new) still shows the
		// result without a reload.
		return a.busClient.PostOutboundComplete(nanoprotocol.OutboundComplete{
			UserID:    a.config.UserID,
			AgentID:   a.config.AgentID,
			SessionID: msg.SessionID,
			Channel:   msg.Channel,
			ChatID:    msg.ChatID,
			MessageID: messageid.New(),
			Summary:   sess.GetSummary(),
		})
	}

	// Session lifecycle: compaction is the only automatic boundary. A compacted
	// segment is archived here — at the start of the next turn and before the
	// user message is recorded — so a turn never spans two segments, and the new
	// segment starts from the system prompt plus the carried summary. The count
	// and idle-timeout rules that used to split sessions here are gone: they cut
	// on rows rather than on context pressure, and each cut silently dropped the
	// prior context.
	if sess.GetMeta(splitPendingMetaKey) == "true" {
		baseKey, _ := session.ParseSessionKey(sess.ID)
		sess = a.splitSession(ctx, sess, baseKey, msg, splitOnCompaction)
		sess.DeleteMeta(splitPendingMetaKey)
	}
	a.noteSessionAgent(sess, spec)

	// 2. Add user message to session. Image attachments become lightweight
	// ImageRefs (expanded to inline data URLs only at request time); if the
	// message has no text, a description is built so the content is non-empty.
	content := msg.Content
	var images []provider.ImageRef
	if len(msg.Media) > 0 {
		notes := make([]string, 0, len(msg.Media))
		for _, item := range msg.Media {
			if ref, ok := imageRefFromMedia(item); ok {
				images = append(images, ref)
			}
			notes = append(notes, mediaNote(item))
		}
		if content == "" {
			content = strings.Join(notes, "\n")
		}
	}
	sess.AddMessage(provider.Message{
		Role:    "user",
		Content: content,
		Images:  images,
	})

	// Persist the session as soon as the user message is recorded so the row
	// exists (and the conversation is durable/retrievable) during the whole
	// turn — the web UI can then find it in the session list instead of
	// treating it as unknown mid-generation. Mark the turn running so a client
	// that reconnects can reconcile and show the indicator.
	sess.SetTurnRunning(true)
	if err := a.sessions.Save(sess); err != nil {
		slog.Warn("failed to save session after user message", "session", sess.ID, "error", err)
	}
	// Throttled mid-turn snapshots make a still-running turn recoverable after
	// a crash/disconnect; stopped when this turn returns.
	stopPersist := a.startTurnPersistence(sess)
	defer stopPersist()

	// 3. Tool definitions: the union over the agent's session modes, so the block
	// is byte-identical whichever mode is active (the upstream caches the request
	// prefix by content; a per-mode list made every mode switch a full miss).
	// Whether a call is allowed is still decided per turn from spec.Permissions.
	tools := a.tools.Definitions()
	builtinNames, mcpServerByTool := a.toolProvenance()
	unionSpecs := a.config.sessionUnionSpecs(spec)
	tools = unionByDefinition(tools, builtinNames, mcpServerByTool, unionSpecs)
	tools = unionByRules(tools, unionSpecs)

	// Push the same view into the delegation tools: a subagent may never use a
	// tool the model was not offered here, and a mode that cannot write must not
	// start a subagent that can.
	a.tools.SetPolicy(tool.CallerPolicy{
		AllowedTools: toolNames(tools),
		DeniesWrite: permissionFor("write", spec.Permissions) == nanoprotocol.PermissionDeny &&
			permissionFor("edit", spec.Permissions) == nanoprotocol.PermissionDeny,
	})

	// 3b. Tail injections: the mode note (which now carries the tools this mode
	// may call) and one row per session file whose content moved after the head
	// snapshot. Both are persisted rows rather than per-request text: a row written
	// once belongs to the prefix of every later call, while an ephemeral tail is
	// paid for again on each one. The mode note repeats every turn on purpose —
	// measured, a note left behind by an earlier turn stops steering the mode.
	a.injectSessionState(sess, spec)

	// 4. Build context: the head (head 1 + one system message per session file)
	// goes in front, then the session history (not saved to the session).
	budget := budgetAvailable(a.config.ContextWindow, a.config.MaxOutputTokens)
	// The head is part of every request, so size it once per turn: the compaction
	// trigger accounts for it. Only the size is kept — buildMessages reads the
	// head per call (a compaction can re-snapshot it mid-turn).
	systemTokens := tokenEstimateHeuristic(a.headMessages(sess, msg.Channel, msg.ChatID, spec.Prompt))
	buildMessages := func() []provider.Message {
		// Thoughts (and other ephemeral rows) are persisted but never fed back to
		// the LLM; the compaction floor drops the summarized prefix.
		sessionMsgs := provider.SanitizeMessages(a.contextMessages(sess))

		// The running summary is pinned ahead of the conversation and budgeted
		// separately: the truncator drops the OLDEST turns first, so a summary inside
		// its slice is the first casualty — exactly the case that made a compaction
		// pay for a summary nobody read.
		pinned := pinnedPrefix(sess)

		// The head is re-read here rather than captured before the loop: a
		// compaction inside this turn re-snapshots the session files (see
		// refreshHeadSnapshot), and the iterations after it — including the overflow
		// retry below — have to send what the files say now. With no compaction the
		// snapshot is cached, so this is a map lookup plus a string join.
		head := a.headMessages(sess, msg.Channel, msg.ChatID, spec.Prompt)
		all, dropped := buildRequestMessages(head, pinned, sessionMsgs, budget)
		if dropped > 0 {
			slog.Warn("context truncated to budget",
				"session", sess.ID, "dropped_messages", dropped,
				"conversation_messages", len(sessionMsgs), "message_budget", budget)
		}
		return all
	}

	// 5. Iteration loop (Reasoning + Tool Use) — one shared executor. The
	// definition's max_rounds is authoritative; 0 falls back to the process cap.
	maxIter := spec.MaxRounds
	if maxIter <= 0 {
		maxIter = a.config.MaxIterations
	}
	// The summary this turn ended up holding, when a compaction ran inside it.
	// It rides on the completion so a client that never received a summary chunk
	// still shows the result (see compactDuringTurn).
	appliedSummary := ""
	ref := messageRefOf(msg)
	hooks := reactHooks{
		beforeIteration: func(iteration int) {
			// Summarize at most once per turn (iteration 0). The tool loop grows
			// the transcript every iteration, so evaluating on every iteration
			// re-fired the summarizer on nearly every tool round.
			if iteration != 0 || a.summarizer == nil {
				return
			}
			appliedSummary = a.summarizer.maybeSummarize(ctx, sess, summaryBudget{
				ContextWindow:   a.config.ContextWindow,
				MaxOutputTokens: a.config.MaxOutputTokens,
				TriggerPercent:  a.config.SummarizeTokenPercent,
				GrowthThreshold: a.config.SummarizeMessageThreshold,
				SystemTokens:    systemTokens,
			}, ref)
		},
		buildMessages: buildMessages,
		chat: func(ctx context.Context, req provider.ChatRequest, messageID string) (*provider.ChatResponse, error) {
			r, cerr := a.callLLM(ctx, req, msg, messageID)
			if cerr != nil && isContextLengthError(cerr) {
				before := tokenEstimateHeuristic(req.Messages)
				slog.Warn("context length exceeded, forcing compression")
				if a.summarizer != nil {
					// The same routine the gate uses: the client sees a divider and
					// the streamed summary instead of an unexplained retry.
					appliedSummary = a.summarizer.forceCompact(ctx, sess, ref)
				}
				req.Messages = buildMessages()
				// Compaction writes the floor now, so this retry is genuinely
				// smaller than the request the provider just rejected. Log both
				// sizes: a retry that did not shrink means the floor never took.
				slog.Warn("retrying after forced compression", "session", msg.SessionID,
					"tokens_before", before, "tokens_after", tokenEstimateHeuristic(req.Messages))
				return a.callLLM(ctx, req, msg, messageid.New())
			}
			return r, cerr
		},
		onUsage: func(u provider.Usage) {
			// Log the cache telemetry per session: it is the only local signal
			// that tells "the prefix changed" apart from "the cache missed".
			ratio := 0.0
			if u.PromptTokens > 0 {
				ratio = float64(u.CachedTokens) / float64(u.PromptTokens)
			}
			slog.Info("llm: turn usage",
				"session", sess.ID, "model", effectiveModel(spec, a.config.Model),
				"prompt_tokens", u.PromptTokens, "completion_tokens", u.CompletionTokens,
				"cached_tokens", u.CachedTokens, "cache_hit_ratio", ratio)
			a.emitUsage(msg, u)
			// Remember the provider's own prompt-token count: it is the ground
			// truth for the next compaction decision, where the char heuristic
			// can over-count (replayed reasoning, images, tool arguments).
			if u.PromptTokens > 0 {
				sess.SetMeta(lastPromptTokensMetaKey, strconv.Itoa(u.PromptTokens))
			}
		},
		onAssistant: func(messageID string, r *provider.ChatResponse) {
			if r.Thought != "" {
				sess.AddMessage(provider.Message{
					Role:             "thought",
					Content:          r.Thought,
					ThoughtElapsedMs: r.ThoughtElapsedMs,
				})
			}
			sess.AddMessage(provider.Message{
				Role:      "assistant",
				Content:   r.Content,
				ToolCalls: r.ToolCalls,
				Reasoning: r.Thought,
			})
		},
		onToolUse: func(messageID string, tc provider.ToolCall) {
			if err := a.busClient.PostOutboundChunk(nanoprotocol.OutboundChunk{
				MessageID: messageID,
				AgentID:   a.config.AgentID,
				UserID:    a.config.UserID,
				Channel:   msg.Channel,
				ChatID:    msg.ChatID,
				SessionID: msg.SessionID,
				Kind:      nanoprotocol.ChunkKindToolUse,
				Content:   fmt.Sprintf(`{"name":%q,"arguments":%q}`, tc.Name, tc.Arguments),
			}); err != nil {
				slog.Warn("failed to post tool_use chunk", "error", err)
			}
		},
		executeTool: func(ctx context.Context, messageID string, tc provider.ToolCall) (string, error) {
			slog.Info("executing tool", "name", tc.Name, "args", tc.Arguments)
			// Defense in depth: enforce the active definition's permissions even
			// if a denied tool slipped past the definition filter.
			var result string
			perm := permissionFor(tc.Name, spec.Permissions)
			switch {
			case perm == nanoprotocol.PermissionDeny:
				result = fmt.Sprintf("Error: %q is denied by permission policy.", tc.Name)
			case perm == nanoprotocol.PermissionAsk && !sessionToolGranted(sess, spec.ID, tc.Name) && !a.approveTool(ctx, sess, messageID, msg, tc.Name, spec.ID, tc.Arguments, spec.ChannelFallbacks):
				result = fmt.Sprintf("Error: %q was not approved.", tc.Name)
			default:
				execCtx := ctx
				isQuestion := tc.Name == "question"
				if isQuestion {
					qid := uuid.NewString()
					if req, ok := tool.ParseQuestions(qid, tc.Arguments); ok {
						if tool.ExceedsGuidanceCaps(req) {
							// Keep the call as the model wrote it: the answers come back one
							// per question, so trimming here would misalign them. The card
							// scrolls, so the user can still answer; this is a log nudge.
							slog.Warn("question call exceeds the guidance caps",
								"questions", len(req.Questions),
								"maxQuestions", tool.MaxQuestionsPerCall,
								"maxOptions", tool.MaxOptionsPerQuestion)
						}
						payload, _ := json.Marshal(req)
						a.beginQuestion(sess, messageID, msg, string(payload))
						execCtx = question.WithQuestionID(ctx, qid)
					}
				}
				var execErr error
				result, execErr = a.tools.Execute(execCtx, tc.Name, tc.Arguments)
				if execErr != nil {
					slog.Error("tool execution failed", "tool", tc.Name, "error", execErr)
					result = fmt.Sprintf("Error: %v", execErr)
				} else if tc.Name == "todowrite" {
					a.applyTodoResult(sess, messageID, msg, result)
				} else if tc.Name == "plan_write" {
					// Remember the newest plan artefact so a later mode switch
					// can point the build agent straight at it, and so a
					// same-turn plan_execute can close it out.
					if p := tool.PlanPathFromResult(result); p != "" {
						sess.SetMeta(lastPlanPathMetaKey, p)
						a.tools.SetPlanPath(p)
					}
				} else if tc.Name == "plan_execute" {
					// Closing the plan is what retires the mode note: it is
					// injected on every turn, so leaving it set kept pointing
					// the agent at finished work.
					if p := tool.PlanExecutedPath(result); p != "" {
						sess.SetMeta(planExecutedMetaKey, p)
						if p == sess.GetMeta(lastPlanPathMetaKey) {
							sess.DeleteMeta(sessionModeNoteMetaKey)
							sess.DeleteMeta(lastPlanPathMetaKey)
							a.tools.SetPlanPath("")
						}
						a.emitStatus("plan_execute", "applied")
					}
				}
				if isQuestion {
					a.endQuestion(sess)
				}
			}
			// Some tools (e.g. mkdir) produce no output on success; LLM APIs
			// require a non-empty tool message. Cap the result length too.
			if result == "" {
				result = "(no output)"
			}
			if maxChars := a.config.MaxToolResultChars; maxChars > 0 && len(result) > maxChars {
				truncated := len(result) - maxChars
				result = result[:maxChars] + fmt.Sprintf("\n\n[...truncated %d characters]", truncated)
			}
			sess.AddMessage(provider.Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    result,
			})
			return result, nil
		},
		onToolResult: func(messageID string, tc provider.ToolCall, result string) {
			if err := a.busClient.PostOutboundChunk(nanoprotocol.OutboundChunk{
				MessageID: messageID,
				AgentID:   a.config.AgentID,
				UserID:    a.config.UserID,
				Channel:   msg.Channel,
				ChatID:    msg.ChatID,
				SessionID: msg.SessionID,
				Kind:      nanoprotocol.ChunkKindToolResult,
				Content:   result,
			}); err != nil {
				slog.Warn("failed to post tool_result chunk", "error", err)
			}
		},
		recordToolResult: func(_ string, tc provider.ToolCall, result string) {
			// A call answered with an error because its arguments were unusable:
			// the answer belongs in the transcript, or the model would never see
			// what it wrote.
			sess.AddMessage(provider.Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    result,
			})
		},
		afterToolIteration: func(string) {
			if err := a.sessions.Save(sess); err != nil {
				slog.Warn("failed to save session", "error", err)
			}
		},
	}

	// The batch tool dispatches through the same function as a top-level call, so
	// an inner call is judged and executed exactly like a sequential one. It
	// never reaches into the registry on its own: a denied tool is still
	// registered, and that path would be a permission bypass.
	a.tools.SetParallelDispatch(tool.ParallelDispatch{
		Run: func(ctx context.Context, name, argumentsJSON string) (string, error) {
			return hooks.executeTool(ctx, "", provider.ToolCall{Name: name, Arguments: argumentsJSON})
		},
		Decision: func(name string) nanoprotocol.PermissionAction {
			return permissionFor(name, spec.Permissions)
		},
	})

	conversation := reactRequest{
		model:       effectiveModel(spec, a.config.Model),
		reasoning:   effectiveReasoning(spec, a.config.ThinkingEffort),
		temperature: effectiveTemp(spec, a.config.Temperature),
		topP:        effectiveTopP(spec, a.config.TopP),
		sessionID:   msg.SessionID,
		tools:       tools,
		maxIter:     maxIter,
	}
	content, messageID, resp, err := reactLoop(ctx, conversation, hooks)
	if err != nil {
		switch {
		case ctx.Err() != nil || errors.Is(err, context.Canceled):
			return a.stopTurn(sess, msg, messageID, resp)
		case errors.Is(err, errReactEmpty):
			return a.failTurn(sess, msg, messageID, resp,
				fmt.Errorf("empty completion from provider after retry"),
				nanoprotocol.TurnErrorProviderUnavailable)
		case errors.Is(err, errReactMaxIterations):
			return a.failTurn(sess, msg, messageID, nil,
				fmt.Errorf("max iterations (%d) reached", maxIter),
				nanoprotocol.TurnErrorMaxIterations)
		default:
			return a.failTurn(sess, msg, messageID, resp, err, "")
		}
	}
	slog.Info("final response", "content", content)

	// Save session (turn no longer running) and flush the coalesced DB snapshot
	// so the portal reflects this turn promptly.
	sess.SetTurnRunning(false)
	if err := a.sessions.Save(sess); err != nil {
		slog.Warn("failed to save session", "error", err)
	}
	a.sessions.SyncNow()
	// Surface a stalled snapshot path before the completion goes out: the chat
	// waiting on this reply is the one whose history is not landing.
	a.reportSyncHealth(ref)

	// Post the final assembled message to the bus (content was already streamed
	// as chunks); this completes the turn for streaming and batch channels. A
	// compaction that ran inside this turn rides along in Summary, so the client
	// can fill its divider even if the summary chunks were lost.
	if err := a.busClient.PostOutboundComplete(nanoprotocol.OutboundComplete{
		UserID:      a.config.UserID,
		AgentID:     a.config.AgentID,
		SessionID:   msg.SessionID,
		Channel:     msg.Channel,
		ChatID:      msg.ChatID,
		MessageID:   messageID,
		FullContent: content,
		Summary:     appliedSummary,
	}); err != nil {
		return fmt.Errorf("post outbound complete: %w", err)
	}
	slog.Debug("posted outbound complete to bus", "user", a.config.UserID, "session", msg.SessionID, "content_len", len(content))

	// Generate/refresh the conversation title AFTER the turn is delivered so it
	// never delays the user-visible answer.
	a.maybeTitle(ctx, sess)

	// Safety net: commit any memory flush whose immediate persist failed.
	a.persistMemoryFlush(sess)
	return nil
}

// compactKeepTextMessages is how many user/assistant text messages are retained
// after a compaction (tool calls and reasoning are not counted).
const compactKeepTextMessages = 6

// compactionFloor returns the index in msgs to keep from so that at least
// `keep` user/assistant text messages remain. The floor is backed up to the
// nearest preceding user message so no tool call/result is orphaned.
func compactionFloor(msgs []provider.Message, keep int) int {
	count := 0
	floor := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		if isTextMessage(msgs[i]) {
			count++
			if count == keep {
				floor = i
				break
			}
		}
	}
	for floor > 0 && msgs[floor].Role != "user" {
		floor--
	}
	return floor
}

// floorOfTurnStart returns the index of the last user message before upTo: the
// point the LLM context may resume from once [0, upTo) is covered by a summary.
// Resuming at the turn's own user message (rather than at upTo) keeps that
// message in context, and resuming on a user message keeps every tool call
// paired with its result — a retained result whose call was summarized away is
// rejected by the provider. Returns 0 (no floor) when the range is one turn.
func floorOfTurnStart(msgs []provider.Message, upTo int) int {
	if upTo > len(msgs) {
		upTo = len(msgs)
	}
	for i := upTo - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return i
		}
	}
	return 0
}

// isTextMessage reports whether a message contributes to the retained-tail
// count: a user message, or an assistant message with visible text and no tool
// calls.
func isTextMessage(m provider.Message) bool {
	if m.Role == "user" {
		return true
	}
	return m.Role == "assistant" && len(m.ToolCalls) == 0 && strings.TrimSpace(m.Content) != ""
}

// applyTodoResult persists a todowrite result to the session metadata and
// streams a todo chunk so the portal can render live progress.
func (a *Agent) applyTodoResult(sess *session.Session, messageID string, msg *nanoprotocol.InboundMessage, result string) {
	if sess != nil {
		sess.SetMeta("todos", result)
	}
	if err := a.busClient.PostOutboundChunk(nanoprotocol.OutboundChunk{
		MessageID: messageID,
		AgentID:   a.config.AgentID,
		UserID:    a.config.UserID,
		Channel:   msg.Channel,
		ChatID:    msg.ChatID,
		SessionID: msg.SessionID,
		Kind:      nanoprotocol.ChunkKindTodo,
		Content:   result,
	}); err != nil {
		slog.Warn("failed to post todo chunk", "error", err)
	}
}

// beginQuestion marks a question as pending (persisted in session metadata) and
// streams the question chunk so the portal can present it.
func (a *Agent) beginQuestion(sess *session.Session, messageID string, msg *nanoprotocol.InboundMessage, payload string) {
	if sess != nil {
		sess.SetMeta("_pending_question", payload)
	}
	if err := a.busClient.PostOutboundChunk(nanoprotocol.OutboundChunk{
		MessageID: messageID,
		AgentID:   a.config.AgentID,
		UserID:    a.config.UserID,
		Channel:   msg.Channel,
		ChatID:    msg.ChatID,
		SessionID: msg.SessionID,
		Kind:      nanoprotocol.ChunkKindQuestion,
		Content:   payload,
	}); err != nil {
		slog.Warn("failed to post question chunk", "error", err)
	}
}

// endQuestion clears the pending-question marker once the question has been
// answered (or the wait was cancelled).
func (a *Agent) endQuestion(sess *session.Session) {
	if sess != nil {
		sess.SetMeta("_pending_question", "")
	}
}

// isInteractiveChannel reports whether a channel can answer a blocking prompt.
func isInteractiveChannel(channel string) bool {
	switch channel {
	case "web", "cli":
		return true
	}
	return false
}

// beginApproval streams a structured permission-approval prompt and marks it
// pending in the session metadata, so the portal can render a dedicated
// approval card (with the tool call detail) instead of a generic question.
func (a *Agent) beginApproval(sess *session.Session, messageID string, msg *nanoprotocol.InboundMessage, payload string) {
	if sess != nil {
		sess.SetMeta("_pending_approval", payload)
	}
	if err := a.busClient.PostOutboundChunk(nanoprotocol.OutboundChunk{
		MessageID: messageID,
		AgentID:   a.config.AgentID,
		UserID:    a.config.UserID,
		Channel:   msg.Channel,
		ChatID:    msg.ChatID,
		SessionID: msg.SessionID,
		Kind:      nanoprotocol.ChunkKindApproval,
		Content:   payload,
	}); err != nil {
		slog.Warn("failed to post approval chunk", "error", err)
	}
}

// endApproval clears the pending-approval marker once the prompt is resolved.
func (a *Agent) endApproval(sess *session.Session) {
	if sess != nil {
		sess.SetMeta("_pending_approval", "")
	}
}

// permissionSummary produces a short, human-readable one-liner for a gated tool
// call so the approval card can show what will run (e.g. the bash command). It
// falls back to the flattened arguments when no specific summary applies.
func permissionSummary(toolName, arguments string) string {
	args := strings.TrimSpace(arguments)
	if args == "" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(args), &m); err == nil && toolName == "bash" {
		if cmd, ok := m["command"].(string); ok {
			return truncateSummary(strings.TrimSpace(cmd))
		}
	}
	return truncateSummary(strings.Join(strings.Fields(args), " "))
}

// truncateSummary caps a command/argument summary at the card's display limit.
func truncateSummary(s string) string {
	const max = 500
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// approveTool resolves an "ask" permission decision. It returns true when the
// tool may run: either the active definition's channel fallback allows it
// non-interactively, or an interactive user approved it. "Always allow" grants
// the tool for the rest of the session and persists an override on the active
// definition's type.
func (a *Agent) approveTool(ctx context.Context, sess *session.Session, messageID string, msg *nanoprotocol.InboundMessage, toolName, typeID, arguments string, fallbacks map[string]string) bool {
	if channelAllows(fallbacks, msg.Channel) {
		return true
	}
	if a.questions == nil || sess == nil || !isInteractiveChannel(msg.Channel) {
		return false
	}
	id := uuid.NewString()
	args := arguments
	if len(args) > 2000 {
		args = args[:2000]
	}
	approval := nanoprotocol.PermissionApproval{
		ID:        id,
		Tool:      toolName,
		TypeID:    typeID,
		Summary:   permissionSummary(toolName, arguments),
		Arguments: args,
	}
	payload, _ := json.Marshal(approval)
	a.beginApproval(sess, messageID, msg, string(payload))
	defer a.endApproval(sess)

	answers, err := a.questions.Wait(ctx, id, sess.ID)
	if err != nil || len(answers) == 0 {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(answers[0])) {
	case "always allow", "always", "allow_always", string(nanoprotocol.PermissionDecisionAlwaysAllow):
		// Session-scoped: grant the tool for the rest of this conversation only.
		// It is recorded in session metadata, never in the agent config, so a
		// new session asks again.
		a.grantSessionTool(sess, typeID, toolName)
		return true
	case "allow once", "allow", "allow_once", "yes":
		return true
	default:
		return false
	}
}
