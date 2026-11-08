package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanosec/internal/messageid"
	"gitee.com/kobeyoung81/nanosec/internal/provider"
	"gitee.com/kobeyoung81/nanosec/internal/tool"
	"github.com/google/uuid"
)

// SubagentConfig holds configuration shared by spawned subagents.
type SubagentConfig struct {
	Model         string
	MaxIterations int
	Workspace     string
	ShellTimeout  time.Duration
	// AgentID identifies the agent in the progress chunks posted while a
	// subagent runs.
	AgentID string
	// MaxConcurrent caps simultaneously running subagents (foreground + background).
	MaxConcurrent int
	// BatchTimeout bounds one delegation end to end (0 = the default below),
	// whether it was started on its own or from a batch.
	BatchTimeout time.Duration
	// MaxToolResultChars caps a subagent tool result fed back to the model
	// (0 = uncapped). A profile's own setting overrides it.
	MaxToolResultChars int
	// ReasoningEffort caps the subagent's thinking ("low"/"minimal"/...);
	// empty = never send reasoning_effort, "none" = turn thinking off.
	ReasoningEffort string
	// RequestTimeout bounds a subagent's streamed completion (0 = no limit).
	RequestTimeout time.Duration
}

// defaultBatchTimeout bounds a concurrent delegation when the config leaves it
// unset: 15 iterations times the 300s request timeout is not a useful budget.
const defaultBatchTimeout = 10 * time.Minute

// taskProgressInterval throttles the streamed progress events of one subagent.
const taskProgressInterval = 2 * time.Second

// Progress states reported on the bus while a subagent runs.
const (
	taskStateStarted  = "started"
	taskStateThinking = "thinking"
	taskStateTool     = "tool"
	taskStateDone     = "done"
	taskStateError    = "error"
)

// SubagentProfile is the "newborn agent" blueprint for a subagent type: its own
// system prompt, tool permissions, and (lesser) turn limit. Derived from the
// unified agent registry (purpose=subagent).
type SubagentProfile struct {
	Tools         map[string]bool
	Prompt        string
	MaxIterations int
	// McpServers is the set of MCP server ids enabled for this subagent's
	// definition; the manager registers their tool adapters.
	McpServers map[string]bool
	// Model overrides the manager's default model for this profile.
	Model string
	// ReasoningEffort is the level already resolved for this subagent type
	// (agent_types.settings["thinking"], then the platform-wide default). Empty
	// means "fall back to the manager's default", which is the same platform
	// value, so an empty profile never widens thinking by itself.
	ReasoningEffort string
	// Permissions gate tool execution for this subagent (last-match-wins).
	Permissions []nanoprotocol.PermissionRule
	// MaxToolResultChars caps tool results for this profile (0 = inherit the
	// manager default).
	MaxToolResultChars int
	// ShellTimeout overrides the shell tool timeout for this profile (0 =
	// inherit the manager/global default).
	ShellTimeout time.Duration
	// RestrictToWorkspace overrides the manager's workspace restriction for
	// this profile's tools (nil = inherit).
	RestrictToWorkspace *bool
}

// subagentEffort picks the level one subagent call runs at: the profile's own
// (already resolved from the subagent type's platform default and the platform
// default), else the manager-wide platform default. Empty means "send no
// reasoning_effort at all".
func subagentEffort(profile SubagentProfile, cfg SubagentConfig) string {
	if profile.ReasoningEffort != "" {
		return profile.ReasoningEffort
	}
	return cfg.ReasoningEffort
}

// SubagentManager runs ephemeral subagents. Each invocation is a fresh agent
// with its own context, tool set and iteration cap; nothing is persisted and
// there is no resume.
type SubagentManager struct {
	cfg         SubagentConfig
	profiles    map[string]SubagentProfile
	toolFactory func(profile SubagentProfile, policy tool.CallerPolicy) *tool.Registry
	prov        provider.Provider
	busClient   BusClient
	userID      string
	sem         chan struct{}
}

// NewSubagentManager creates a SubagentManager.
func NewSubagentManager(
	cfg SubagentConfig,
	profiles map[string]SubagentProfile,
	toolFactory func(profile SubagentProfile, policy tool.CallerPolicy) *tool.Registry,
	prov provider.Provider,
	busClient BusClient,
	userID string,
) *SubagentManager {
	max := cfg.MaxConcurrent
	if max <= 0 {
		max = 4
	}
	return &SubagentManager{
		cfg:         cfg,
		profiles:    profiles,
		toolFactory: toolFactory,
		prov:        prov,
		busClient:   busClient,
		userID:      userID,
		sem:         make(chan struct{}, max),
	}
}

// SetProfiles replaces the subagent profile map (on capability reload).
func (m *SubagentManager) SetProfiles(profiles map[string]SubagentProfile) {
	m.profiles = profiles
}

// Profile reports whether a subagent type exists.
func (m *SubagentManager) Profile(subagentType string) (SubagentProfile, bool) {
	p, ok := m.profiles[subagentType]
	return p, ok
}

// batchTimeout is the wall-clock budget of one delegated batch.
func (m *SubagentManager) batchTimeout() time.Duration {
	if m.cfg.BatchTimeout > 0 {
		return m.cfg.BatchTimeout
	}
	return defaultBatchTimeout
}

func (m *SubagentManager) release() { <-m.sem }

// acquireN reserves n slots, waiting for in-flight subagents to finish. Callers
// must have checked n against cap(m.sem) first, so this cannot deadlock.
func (m *SubagentManager) acquireN(ctx context.Context, n int) error {
	for i := 0; i < n; i++ {
		select {
		case m.sem <- struct{}{}:
		case <-ctx.Done():
			for j := 0; j < i; j++ {
				m.release()
			}
			return ctx.Err()
		}
	}
	return nil
}

func (m *SubagentManager) releaseN(n int) {
	for i := 0; i < n; i++ {
		m.release()
	}
}

// resolve looks up a subagent type and attenuates it by the caller's policy: the
// tools the caller cannot use are dropped, and a read-only caller never receives
// a write-capable type. This is the delegation half of the permission model —
// the subagent's own rules still apply on top.
func (m *SubagentManager) resolve(policy tool.CallerPolicy, subagentType string) (SubagentProfile, error) {
	profile, ok := m.profiles[subagentType]
	if !ok {
		return SubagentProfile{}, fmt.Errorf("unknown subagent type %q", subagentType)
	}
	if policy.DeniesWrite && !profileIsReadOnly(profile) {
		return SubagentProfile{}, fmt.Errorf(
			"subagent type %q can modify files and is not available from a read-only mode; read-only types are: %s",
			subagentType, strings.Join(ReadOnlyProfileIDs(m.profiles), ", "))
	}
	profile.Tools = tool.AttenuateEnabled(profile.Tools, policy.AllowedTools)
	return profile, nil
}

// Run executes a subagent synchronously and returns its final response, blocking
// the calling turn (foreground mode). channel/chatID are where its progress
// events go.
func (m *SubagentManager) Run(ctx context.Context, policy tool.CallerPolicy, req tool.TaskRequest, channel, chatID string) (string, error) {
	profile, err := m.resolve(policy, req.SubagentType)
	if err != nil {
		return "", err
	}
	// Wait for a slot rather than failing fast: batches of delegations are now
	// started by parallel_execution, and queueing behind the limit is the useful
	// behaviour there — the tasks that did not fit must not be dropped.
	if err := m.acquireN(ctx, 1); err != nil {
		return "", fmt.Errorf("subagent start cancelled: %w", err)
	}
	defer m.release()

	// A foreground delegation used to run without any overall bound — one call
	// was measured taking 49 minutes while the caller waited. It now shares the
	// batch budget.
	ctx, cancel := context.WithTimeout(ctx, m.batchTimeout())
	defer cancel()

	id := uuid.NewString()[:8]
	out, err := m.execute(ctx, profile, policy, req.Prompt, m.progressReporter(id, req.Description, channel, chatID))
	if errors.Is(err, errSubagentTimedOut) {
		return out, fmt.Errorf("subagent exceeded its time budget (%s)", m.batchTimeout())
	}
	return out, err
}

// execute runs the shared ReAct loop for a subagent with an isolated tool
// registry. It uses the same executor as the main session agent.
func (m *SubagentManager) execute(ctx context.Context, profile SubagentProfile, policy tool.CallerPolicy, prompt string, progress func(state, detail string)) (out string, err error) {
	defer func() {
		if err != nil {
			progress(taskStateError, tailText(err.Error(), 200))
			return
		}
		progress(taskStateDone, "")
	}()

	var reg *tool.Registry
	if m.toolFactory != nil {
		reg = m.toolFactory(profile, policy)
	} else {
		reg = tool.NewRegistry()
	}

	systemPrompt := profile.Prompt
	if systemPrompt == "" {
		systemPrompt = defaultSubagentPrompt(m.cfg.Workspace)
	}

	messages := []provider.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: prompt},
	}

	maxIter := profile.MaxIterations
	if maxIter <= 0 {
		maxIter = m.cfg.MaxIterations
	}
	if maxIter <= 0 {
		maxIter = 15
	}

	model := m.cfg.Model
	if profile.Model != "" {
		model = profile.Model
	}

	progress(taskStateStarted, model)

	var streamed strings.Builder
	hooks := reactHooks{
		buildMessages: func() []provider.Message { return messages },
		chat: func(ctx context.Context, req provider.ChatRequest, _ string) (*provider.ChatResponse, error) {
			progress(taskStateThinking, "")
			var lastErr error
			for attempt := 0; attempt < subagentChatAttempts; attempt++ {
				if attempt > 0 {
					// A gateway blip (502, an HTML error page where JSON was
					// expected, a stream cut mid-flight) used to throw away
					// everything the subagent had done — including the nine
					// minutes it had just spent. One retry is cheap next to that.
					progress(taskStateThinking, "retrying: "+tailText(lastErr.Error(), 80))
					select {
					case <-time.After(subagentRetryDelay):
					case <-ctx.Done():
						return nil, fmt.Errorf("llm: %w", ctx.Err())
					}
				}
				resp, err := chatStreamCollectWith(ctx, m.prov, req, m.cfg.RequestTimeout, func(delta string) {
					streamed.WriteString(delta)
					progress(taskStateThinking, tailText(streamed.String(), 120))
				})
				if err == nil {
					return resp, nil
				}
				lastErr = err
				// Unusable tool-call arguments are not a transport failure: hand
				// the response back so the shared loop can answer the call with
				// an error result and let the model re-emit it.
				var incomplete *provider.ErrToolCallIncomplete
				if resp != nil && errors.As(err, &incomplete) {
					return resp, err
				}
				if !retryableSubagentError(err) {
					break
				}
			}
			return nil, fmt.Errorf("llm: %w", lastErr)
		},
		onAssistant: func(_ string, resp *provider.ChatResponse) {
			messages = append(messages, provider.Message{
				Role:      "assistant",
				Content:   resp.Content,
				ToolCalls: resp.ToolCalls,
			})
		},
		executeTool: func(ctx context.Context, _ string, tc provider.ToolCall) (string, error) {
			progress(taskStateTool, tc.Name)
			var res string
			if permissionFor(tc.Name, profile.Permissions) == nanoprotocol.PermissionDeny {
				res = fmt.Sprintf("Error: %q is denied by permission policy.", tc.Name)
			} else {
				var execErr error
				res, execErr = reg.Execute(ctx, tc.Name, tc.Arguments)
				if execErr != nil {
					res = fmt.Sprintf("Error: %v", execErr)
				}
			}
			// Some tools produce no output on success; LLM APIs require a
			// non-empty tool message.
			if res == "" {
				res = "(no output)"
			}
			// Cap the result fed back to the model (the profile's own setting
			// overrides the manager default), mirroring the session loop.
			maxChars := profile.MaxToolResultChars
			if maxChars <= 0 {
				maxChars = m.cfg.MaxToolResultChars
			}
			if maxChars > 0 && len(res) > maxChars {
				truncated := len(res) - maxChars
				res = res[:maxChars] + fmt.Sprintf("\n\n[...truncated %d characters]", truncated)
			}
			// Always append the result — including for denied/failed tools — so
			// every assistant tool call is answered. Otherwise the next request
			// is rejected with "tool_calls must be followed by tool messages".
			messages = append(messages, provider.Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    res,
			})
			return res, nil
		},
		recordToolResult: func(_ string, tc provider.ToolCall, result string) {
			// The answer to a call whose arguments were unusable, stored like any
			// tool result so the subagent's next request carries it.
			messages = append(messages, provider.Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    result,
			})
		},
	}

	// A subagent may batch its own calls (parallel reads, for instance) but the
	// dispatch still goes through the code above, so its rules apply, and its
	// dispatch never hands out a tool the profile lacks.
	reg.SetParallelDispatch(tool.ParallelDispatch{
		Run: func(ctx context.Context, name, argumentsJSON string) (string, error) {
			return hooks.executeTool(ctx, "", provider.ToolCall{Name: name, Arguments: argumentsJSON})
		},
		Decision: func(name string) nanoprotocol.PermissionAction {
			return permissionFor(name, profile.Permissions)
		},
	})

	content, _, _, err := reactLoop(ctx, reactRequest{
		model:     model,
		reasoning: subagentEffort(profile, m.cfg),
		tools:     reg.Definitions(),
		maxIter:   maxIter,
	}, hooks)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", errSubagentTimedOut
		}
		switch {
		case errors.Is(err, errReactEmpty):
			return "", fmt.Errorf("llm: empty completion from provider after retry")
		case errors.Is(err, errReactMaxIterations):
			return "", fmt.Errorf("max iterations (%d) reached", maxIter)
		default:
			return "", err
		}
	}
	return content, nil
}

// subagentChatAttempts is how many times one subagent completion is attempted:
// the initial call plus a single retry of a gateway-class failure.
const subagentChatAttempts = 2

// subagentRetryDelay is the pause before that retry. It is a variable so tests
// can run the retry path without waiting.
var subagentRetryDelay = 2 * time.Second

// errSubagentTimedOut marks a subagent that ran out of its time budget instead
// of failing on its own, so the caller can say which of the two happened.
var errSubagentTimedOut = errors.New("subagent timed out before finishing")

// retryableSubagentError reports whether an LLM failure is the kind a retry can
// plausibly fix: a gateway hiccup. A maxed-out iteration count, a permission or
// type refusal and a context-length overflow are all decisions, not blips, and
// retrying them only spends time.
func retryableSubagentError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, marker := range []string{
		"502", "503", "504",
		"invalid character", // an HTML error page where JSON was expected
		"empty completion",  // the provider closed the stream without content
		"context canceled",  // the stream was cut mid-flight
		"connection reset",
		"unexpected EOF",
		"EOF",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// taskProgressPayload is the JSON body of a ChunkKindStatus event emitted while
// a subagent runs, so the client can show one live progress line instead of a
// silent wait.
type taskProgressPayload struct {
	Event       string `json:"event"`
	ID          string `json:"id"`
	Description string `json:"description"`
	State       string `json:"state"`
	Detail      string `json:"detail,omitempty"`
}

// progressReporter builds the progress sink for one subagent. It is a no-op when
// the origin chat is unknown (e.g. a cron-triggered turn), and it throttles the
// per-token text stream while always letting state changes through.
func (m *SubagentManager) progressReporter(id, description, channel, chatID string) func(state, detail string) {
	if channel == "" || chatID == "" || m.busClient == nil {
		return func(string, string) {}
	}
	var mu sync.Mutex
	last := time.Time{}
	return func(state, detail string) {
		mu.Lock()
		if state == taskStateThinking && time.Since(last) < taskProgressInterval {
			mu.Unlock()
			return
		}
		last = time.Now()
		mu.Unlock()

		payload, err := json.Marshal(taskProgressPayload{
			Event:       "task_progress",
			ID:          id,
			Description: description,
			State:       state,
			Detail:      detail,
		})
		if err != nil {
			return
		}
		chunk := nanoprotocol.OutboundChunk{
			MessageID: messageid.New(),
			AgentID:   m.cfg.AgentID,
			UserID:    m.userID,
			Channel:   channel,
			ChatID:    chatID,
			SessionID: channel + ":" + chatID,
			Kind:      nanoprotocol.ChunkKindStatus,
			Content:   string(payload),
		}
		if err := m.busClient.PostOutboundChunk(chunk); err != nil {
			slog.Debug("subagent: failed to post progress", "error", err)
		}
	}
}

// announce posts the subagent result to the outbound bus queue (user-visible).
func (m *SubagentManager) announce(description, result, originChannel, originChatID string) {
	err := m.busClient.PostOutbound(nanoprotocol.OutboundMessage{
		UserID:    m.userID,
		SessionID: originChannel + ":" + originChatID,
		Channel:   originChannel,
		ChatID:    originChatID,
		MessageID: messageid.New(),
		Content:   fmt.Sprintf("**[Task '%s' completed]**\n\n%s", description, result),
	})
	if err != nil {
		slog.Error("subagent: failed to announce result", "error", err)
	}
}

// tailText returns the last n runes of s on a single line, for a progress line.
func tailText(s string, n int) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	joined := strings.Join(fields, " ")
	r := []rune(joined)
	if len(r) > n {
		return "…" + string(r[len(r)-n:])
	}
	return joined
}

func defaultSubagentPrompt(workspace string) string {
	return fmt.Sprintf(`# Subagent

You are a focused subagent spawned to complete a specific task.

## Rules
1. Stay focused — complete only the assigned task.
2. Your final response will be delivered directly to the user.
3. Be concise but complete in your findings.

## Workspace
%s`, workspace)
}
