package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanosec/internal/provider"
)

// chatStreamCollect runs one streamed completion and returns the assembled
// response without posting anything to the bus. Subagents and utility roles use
// it so every LLM-backed definition shares the streaming transport. A positive
// timeout bounds the whole call.
func chatStreamCollect(ctx context.Context, prov provider.Provider, req provider.ChatRequest, timeout time.Duration) (*provider.ChatResponse, error) {
	return chatStreamCollectWith(ctx, prov, req, timeout, nil)
}

// chatStreamCollectWith is chatStreamCollect with an optional delta sink that
// receives each content delta as it streams (nil = collect only).
func chatStreamCollectWith(ctx context.Context, prov provider.Provider, req provider.ChatRequest, timeout time.Duration, onDelta func(string)) (*provider.ChatResponse, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	stream, err := prov.ChatStream(ctx, req)
	if err != nil {
		return nil, err
	}
	var full, thoughts strings.Builder
	var toolCalls []provider.ToolCall
	var usage provider.Usage
	var finishReason string
	sawDone := false
	for ev := range stream {
		if ev.Err != nil {
			return nil, ev.Err
		}
		if ev.Delta != "" {
			if ev.Kind == nanoprotocol.ChunkKindThought {
				thoughts.WriteString(ev.Delta)
			} else {
				full.WriteString(ev.Delta)
				if onDelta != nil {
					onDelta(ev.Delta)
				}
			}
		}
		if ev.Done {
			sawDone = true
			full.Reset()
			full.WriteString(ev.FullContent)
			toolCalls = ev.ToolCalls
			usage = ev.Usage
			finishReason = ev.FinishReason
		}
	}
	if !sawDone {
		return nil, fmt.Errorf("llm stream closed without completion")
	}
	resp := &provider.ChatResponse{
		Content:      full.String(),
		Thought:      thoughts.String(),
		ToolCalls:    toolCalls,
		Usage:        usage,
		FinishReason: finishReason,
	}
	// Unusable arguments are not a transport failure: the response (and its
	// calls) goes back to the caller so the shared loop can answer the call and
	// the model can re-emit it.
	if bad := provider.NormalizeToolCalls(resp.ToolCalls); len(bad) > 0 {
		logIncompleteToolCalls(req.SessionID, req.Model, resp.ToolCalls, bad, finishReason)
		return resp, &provider.ErrToolCallIncomplete{Calls: bad, FinishReason: finishReason}
	}
	return resp, nil
}

// logIncompleteToolCalls records what the model wrote. The raw bytes are the
// only way to tell a cut-off document from a malformed one after the fact (and
// to see whether the relay mangled the stream), and they are never persisted,
// so log a bounded tail here.
func logIncompleteToolCalls(sessionID, model string, calls []provider.ToolCall, bad []provider.IncompleteToolCall, finishReason string) {
	for _, c := range bad {
		id := ""
		if c.Index >= 0 && c.Index < len(calls) {
			id = calls[c.Index].ID
		}
		args := ""
		if c.Index >= 0 && c.Index < len(calls) {
			args = calls[c.Index].Arguments
		}
		slog.Warn("llm tool call arguments unusable; answering the call with an error",
			"session", sessionID, "model", model, "tool", c.Tool,
			"argument_bytes", c.Bytes, "kind", c.Kind, "finish_reason", finishReason,
			"call_index", c.Index, "call_id", id, "calls_total", len(calls),
			"tail", tailOf(args, 120))
	}
}

// tailOf returns the last n bytes of s, quoted so a log line stays one line.
func tailOf(s string, n int) string {
	if len(s) > n {
		s = s[len(s)-n:]
	}
	return fmt.Sprintf("%q", s)
}

// ttfbC returns the channel to select on for the TTFB watchdog, or nil when
// there is no TTFB timer. A nil channel in a select blocks forever, allowing
// the other cases (stream/idle) to proceed.
func ttfbC(t *time.Timer) <-chan time.Time {
	if t == nil {
		return nil
	}
	return t.C
}

// idleC returns the channel to select on for the idle watchdog, or nil when
// the idle timer has not been armed yet (still awaiting first byte) or is
// disabled.
func idleC(t *time.Timer) <-chan time.Time {
	if t == nil {
		return nil
	}
	return t.C
}

// Thresholds above which the numbers are worth a line in the log: a bus
// post that took this long held the read loop, and a gap this long is the
// shape of a stream the proxies in front of the gateway may cut.
var (
	chunkPostWarnThreshold = 2 * time.Second
	streamGapWarnThreshold = 30 * time.Second
)

// streamStats is what one streamed completion looked like from the agent's
// side, for the post-mortem of a cut: how much streamed, how long the biggest
// quiet stretch was, and how long the bus held the read loop.
type streamStats struct {
	session   string
	model     string
	chunks    int
	bytes     int
	duration  time.Duration
	maxPost   time.Duration
	totalPost time.Duration
	maxGap    time.Duration
	streamErr error
}

// reportStreamObservation logs a stream that was held up (slow bus posts), went
// quiet long enough to risk a proxy timeout, or ended in an error. A fast,
// quiet, successful stream logs nothing: this exists to attribute a cut to the
// read loop, so the next occurrence is not guesswork.
func (a *Agent) reportStreamObservation(st streamStats, finishReason string) {
	slowPost := st.maxPost >= chunkPostWarnThreshold
	longGap := st.maxGap >= streamGapWarnThreshold
	if !slowPost && !longGap && st.streamErr == nil {
		return
	}
	outcome := "completed"
	if st.streamErr != nil {
		outcome = "interrupted"
		if provider.IsRequestTimeout(st.streamErr) {
			outcome = "agent_timeout"
		}
	}
	args := []any{
		"session", st.session,
		"model", st.model,
		"chunks", st.chunks,
		"bytes", st.bytes,
		"duration_ms", st.duration.Milliseconds(),
		"max_post_ms", st.maxPost.Milliseconds(),
		"total_post_ms", st.totalPost.Milliseconds(),
		"max_gap_ms", st.maxGap.Milliseconds(),
		"finish_reason", finishReason,
		"outcome", outcome,
	}
	if st.streamErr != nil {
		args = append(args, "error", st.streamErr)
	}
	slog.Warn("llm stream held up or cut", args...)
}

// chatStreamTurn runs a single LLM turn via the streaming API and returns the
// assembled response. Content deltas are forwarded to the bus as outbound
// chunks in real time so streaming channels render tokens as they arrive;
// batch channels aggregate them and flush on the completion event. Tool calls
// are accumulated from the stream and returned like a regular Chat response.
//
// A stream that produces no events is governed by two independent watchdogs:
//   - LLMTtfbTimeout bounds the wait for the FIRST event (time-to-first-byte).
//     Reasoning models can think for minutes before their first token, so this
//     must be generous and is NOT confused with a stall mid-generation.
//   - LLMStreamIdleTimeout bounds the gap BETWEEN events once the stream has
//     started, so a provider that stalls mid-generation can never wedge the
//     agent loop.
//
// Either expiry aborts the turn (the child context is canceled so the provider
// goroutine unblocks). On failure the caller receives the partial response
// (content streamed so far) plus the error; posting the terminal complete —
// success or failure — is owned by the caller.
func (a *Agent) chatStreamTurn(ctx context.Context, req provider.ChatRequest, msg *nanoprotocol.InboundMessage, messageID string) (*provider.ChatResponse, error) {
	turnCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := a.Provider().ChatStream(turnCtx, req)
	if err != nil {
		return nil, err
	}

	var full strings.Builder
	var thoughts strings.Builder
	var toolCalls []provider.ToolCall
	var usage provider.Usage
	var finishReason string
	var streamErr error
	var thoughtStart time.Time
	var thoughtElapsed time.Duration
	sawDone := false

	// Stream observation. Deltas are posted to the bus synchronously on this
	// goroutine, so a slow bus stops the provider being read — and a consumer
	// that stops reading for around a minute has its SSE connection torn down
	// by the proxies in front of the gateway (measured 2026-10-19), which
	// surfaces here as a truncated stream. The counters below are reported,
	// never acted on: they are how a later cut is attributed to this loop
	// instead of guessed at.
	started := time.Now()
	var (
		chunkCount  int
		streamBytes int
		totalPost   time.Duration
		maxPost     time.Duration
		maxGap      time.Duration
	)
	lastEvent := started
	noteGap := func() {
		if gap := time.Since(lastEvent); gap > maxGap {
			maxGap = gap
		}
		lastEvent = time.Now()
	}
	markThoughtEnd := func() {
		if !thoughtStart.IsZero() && thoughtElapsed == 0 {
			thoughtElapsed = time.Since(thoughtStart)
		}
	}

	ttfb := a.config.LLMTtfbTimeout
	idle := a.config.LLMStreamIdleTimeout

	// TTFB watchdog: armed at stream open, stopped on the first event.
	var ttfbTimer *time.Timer
	if ttfb > 0 {
		ttfbTimer = time.NewTimer(ttfb)
		defer ttfbTimer.Stop()
	}

	// Idle watchdog: armed only AFTER the first event (see arming inside the
	// loop). Until then, a nil idleTimer means "still waiting for first byte".
	var idleTimer *time.Timer
	defer func() {
		if idleTimer != nil {
			idleTimer.Stop()
		}
	}()
	resetIdle := func() {
		if idleTimer == nil {
			return
		}
		if !idleTimer.Stop() {
			select {
			case <-idleTimer.C:
			default:
			}
		}
		idleTimer.Reset(idle)
	}
	armIdle := func() {
		if idle > 0 {
			idleTimer = time.NewTimer(idle)
		}
	}

loop:
	for {
		var ev provider.ChatStreamEvent
		var ok bool
		select {
		case ev, ok = <-stream:
		case <-ttfbC(ttfbTimer):
			cancel()
			streamErr = provider.ErrRequestTimeout{Phase: "ttfb", Duration: ttfb}
			slog.Warn("llm stream ttfb timeout", "session", msg.SessionID, "ttfb", ttfb)
			break loop
		case <-idleC(idleTimer):
			cancel()
			streamErr = provider.ErrRequestTimeout{Phase: "idle", Duration: idle}
			slog.Warn("llm stream idle timeout", "session", msg.SessionID, "idle", idle)
			break loop
		}
		if !ok {
			break
		}
		if ev.Err != nil {
			streamErr = ev.Err
			break
		}
		noteGap()

		// First non-empty event: the stream is producing. Stop the TTFB
		// watchdog and start the idle watchdog.
		started := ev.Delta != "" || ev.Done
		if started && ttfbTimer != nil {
			if !ttfbTimer.Stop() {
				select {
				case <-ttfbTimer.C:
				default:
				}
			}
			ttfbTimer = nil
			if idleTimer == nil {
				armIdle()
			}
		}

		if ev.Delta != "" {
			kind := ev.Kind
			if kind == "" {
				kind = nanoprotocol.ChunkKindContent
			}
			if kind == nanoprotocol.ChunkKindContent {
				markThoughtEnd()
				full.WriteString(ev.Delta)
			} else if kind == nanoprotocol.ChunkKindThought {
				if thoughtStart.IsZero() {
					thoughtStart = time.Now()
				}
				thoughts.WriteString(ev.Delta)
			}
			postStart := time.Now()
			err := a.busClient.PostOutboundChunk(nanoprotocol.OutboundChunk{
				MessageID: messageID,
				AgentID:   a.config.AgentID,
				UserID:    a.config.UserID,
				Channel:   msg.Channel,
				ChatID:    msg.ChatID,
				SessionID: msg.SessionID,
				Kind:      kind,
				Content:   ev.Delta,
			})
			if post := time.Since(postStart); post > maxPost {
				maxPost = post
			}
			totalPost += time.Since(postStart)
			chunkCount++
			streamBytes += len(ev.Delta)
			if err != nil {
				slog.Warn("failed to post outbound chunk", "error", err)
			}
			resetIdle()
		}
		if ev.Done {
			markThoughtEnd()
			sawDone = true
			full.Reset()
			full.WriteString(ev.FullContent)
			toolCalls = ev.ToolCalls
			usage = ev.Usage
			finishReason = ev.FinishReason
		}
	}

	// The stream ended without the terminal Done event and without an error
	// (e.g. the provider closed the connection mid-generation).
	if streamErr == nil && !sawDone {
		streamErr = fmt.Errorf("llm stream closed without completion")
	}
	// A Done event can still carry tool calls whose arguments are not usable.
	// Persisting those poisons the session: the provider rejects every future
	// request because function.arguments is not valid JSON. They are never
	// written down as the model wrote them — the loop answers each one with an
	// error result (like any failing tool) and the model may re-emit it.
	if streamErr == nil {
		if bad := provider.NormalizeToolCalls(toolCalls); len(bad) > 0 {
			streamErr = &provider.ErrToolCallIncomplete{Calls: bad, FinishReason: finishReason}
			logIncompleteToolCalls(msg.SessionID, req.Model, toolCalls, bad, finishReason)
		}
	}

	markThoughtEnd()
	a.reportStreamObservation(streamStats{
		session:   msg.SessionID,
		model:     req.Model,
		chunks:    chunkCount,
		bytes:     streamBytes,
		duration:  time.Since(started),
		maxPost:   maxPost,
		totalPost: totalPost,
		maxGap:    maxGap,
		streamErr: streamErr,
	}, finishReason)
	if streamErr != nil {
		// Return the partial content — and any tool calls — so the caller can
		// finalize the turn with it or answer the calls; posting the (error)
		// completion is the caller's job.
		return &provider.ChatResponse{
			Content:          full.String(),
			Thought:          thoughts.String(),
			ThoughtElapsedMs: thoughtElapsed.Milliseconds(),
			ToolCalls:        toolCalls,
			Usage:            usage,
			FinishReason:     finishReason,
		}, streamErr
	}

	return &provider.ChatResponse{
		Content:          full.String(),
		Thought:          thoughts.String(),
		ThoughtElapsedMs: thoughtElapsed.Milliseconds(),
		ToolCalls:        toolCalls,
		Usage:            usage,
		FinishReason:     finishReason,
	}, nil
}
