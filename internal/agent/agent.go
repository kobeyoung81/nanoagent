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
	"sync/atomic"
	"time"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanosec/internal/config"
	"gitee.com/kobeyoung81/nanosec/internal/messageid"
	"gitee.com/kobeyoung81/nanosec/internal/provider"
	"gitee.com/kobeyoung81/nanosec/internal/question"
	"gitee.com/kobeyoung81/nanosec/internal/session"
	"gitee.com/kobeyoung81/nanosec/internal/tool"
	"github.com/google/uuid"
)

// Polling cadence. The socket turns the poll into a safety net for a notice
// that never arrived; without it the poll is the only source of work.
const (
	socketPollInterval   = 5 * time.Second
	fallbackPollInterval = 500 * time.Millisecond
)

// BusClient defines the interface for bus communication.
// SessionSink writes the conversation projection.
type SessionSink interface {
	SaveSession(req nanoprotocol.SaveSessionRequest) error
}

type BusClient interface {
	PollInbound(userID string, bindingID int64, agentID string) (*nanoprotocol.InboundMessage, error)
	PostOutbound(msg nanoprotocol.OutboundMessage) error
	PostOutboundChunk(chunk nanoprotocol.OutboundChunk) error
	PostOutboundComplete(complete nanoprotocol.OutboundComplete) error
	ConfigChanges() <-chan nanoprotocol.ConfigChange
	SetAgentPermission(req nanoprotocol.SetAgentPermissionRequest) error
	Close() error
}

// Config holds the configuration for the agent.
type Config struct {
	UserID    string
	BindingID int64
	AgentID   string
	BusURL    string
	Model     string
	// TitleModel is the model used for conversation title generation (empty =
	// falls back to Model).
	TitleModel string
	// SummarizeModel is the model used for session summarization/compaction
	// (empty = falls back to Model).
	SummarizeModel string
	// RolePrompts overrides the built-in prompts for internal roles
	// (title, summarize), keyed by role. Empty entries use the constants.
	RolePrompts map[string]string
	// Language is the effective reply language for this conversation ("zh",
	// "en"), already resolved from the account's preference and the platform
	// default. Empty means no per-turn language note is emitted.
	Language string
	// Agents is the unified resolved definition registry (session agents,
	// subagents, utilities) keyed by definition id.
	Agents map[string]ResolvedAgent
	// DefaultAgent is the definition id a turn uses absent a per-message
	// metadata override.
	DefaultAgent string
	// SessionAgentIDs are the definition ids this agent can switch between
	// (default first). Empty falls back to attribute-derived session agents.
	SessionAgentIDs []string
	MaxIterations   int
	SessionDir      string
	// WorkspaceDir / SharedWorkspaceDir are the roots image references may
	// resolve within (uploaded via the file API → hypervisor workspace).
	WorkspaceDir       string
	SharedWorkspaceDir string
	// WorkspaceURL is the workspace module's base URL. The goal command creates
	// a goal there and goal_report posts each round's result back.
	WorkspaceURL string
	// VisionMaxImageBytes caps a single image inlined for the model (0 = the
	// package default). VisionMaxImagesPerMessage caps attachments per turn.
	VisionMaxImageBytes       int
	VisionMaxImagesPerMessage int
	ContextWindow             int
	MaxOutputTokens           int
	// ThinkingEffort is the main-loop reasoning effort used when a definition
	// resolves none of its own (see cmd/nanosec/models.go's
	// resolveThinkingEffort); "" sends no level at all.
	ThinkingEffort string
	// Temperature/TopP are optional per-model sampling params. Temperature is
	// nil when unset: a pointer to 0 asks for deterministic decoding.
	Temperature               *float32
	TopP                      float32
	MaxToolResultChars        int
	SummarizeTokenPercent     int
	SummarizeMessageThreshold int
	// TitleRefreshMessages is the minimum number of new messages before an
	// existing conversation title is regenerated (0 disables refresh).
	TitleRefreshMessages int
	MemoryRecentDays     int
	// SessionSyncEnabled controls async DB snapshot syncing (default on).
	SessionSyncEnabled bool
	// SessionSyncFlushInterval bounds the coalesced DB snapshot lag.
	SessionSyncFlushInterval time.Duration
	// LLMTtfbTimeout bounds the wait for the FIRST stream event
	// (time-to-first-byte); reasoning models can think for minutes. 0
	// disables the TTFB watchdog.
	LLMTtfbTimeout time.Duration
	// LLMStreamIdleTimeout aborts a streaming turn when no event arrives for
	// this long AFTER the first event (0 disables the watchdog).
	LLMStreamIdleTimeout time.Duration
	// LLMRequestTimeout bounds a non-session streamed call (subagent/utility)
	// as a total deadline (0 disables it).
	LLMRequestTimeout time.Duration
	// LLMMaxRetries bounds automatic retries for transient stream-open
	// failures (429 / 5xx / network).
	LLMMaxRetries int
	// LLMSummarizeReasoningEffort caps the summarizer's thinking (empty =
	// never send reasoning_effort; "none" is what actually turns it off).
	LLMSummarizeReasoningEffort string
	// ChannelCaps maps channel name → messaging capabilities, used to adapt
	// output formatting per channel.
	ChannelCaps map[string]config.ChannelCapabilities
}

// Agent represents a per-user agent instance.
type Agent struct {
	config     Config
	baseConfig config.AgentConfig
	busClient  BusClient
	provider   provider.Provider
	tools      *tool.Registry
	sessions   *session.Store
	instanceID string
	mu         sync.Mutex
	ctxBuilder *ContextBuilder // optional system prompt builder
	summarizer *summarizer     // async session summarization
	// capRebuild applies capability catalog changes (rebuild registry,
	// reconcile MCP servers, re-materialize skills). Invoked by the owning
	// AgentHandler when idle. Never nil after SetCapabilityRebuilder.
	capRebuild func(ctx context.Context) error

	msgMu sync.Mutex
	// lastMessage is the session context of the most recent message, used to
	// target status chunks (capability reload notifications).
	lastMessage *messageRef

	// reportedSyncFailures is the last SyncHealth.Failures value surfaced as a
	// status chunk. Edge-triggered on purpose: a store that keeps failing reports
	// once per turn instead of once per flush, and because the counter is
	// cumulative any later turn reports a failure that happened on a path nobody
	// was watching.
	reportedSyncFailures atomic.Int64

	// questions is the process-wide broker for blocking question tools.
	questions *question.Broker

	// sessMu guards sessLocks; each session gets a mutex so turns for the same
	// conversation are serialized while different sessions run concurrently.
	sessMu    sync.Mutex
	sessLocks map[string]*sync.Mutex

	// turnMu guards turns, the cancel funcs for in-flight per-session turns so
	// a /stop command can abort the running turn.
	turnMu sync.Mutex
	turns  map[string]context.CancelFunc

	// inFlight counts turns in progress; reloads are deferred until it is zero.
	inFlight      atomic.Int64
	reloadMu      sync.Mutex
	pendingChange *nanoprotocol.ConfigChange

	// permMu guards permissions, the resolved allow/ask/deny rules from the
	// agent type + per-agent overrides.
	permMu      sync.RWMutex
	permissions []nanoprotocol.PermissionRule

	// toolMetaMu guards the tool provenance sets used to gate tool definitions
	// per definition: which registered names are builtins, and which MCP server
	// each MCP tool came from.
	toolMetaMu      sync.RWMutex
	builtinNames    map[string]bool
	mcpServerByTool map[string]string

	// headMu guards heads: the frozen session-file snapshot of each live segment,
	// keyed by session id. The snapshot is what keeps the request head byte-stable
	// for the life of a segment (see sessionfiles.go).
	headMu sync.Mutex
	heads  map[string]headSnapshot
}

// New creates a new agent instance.
// Option configures the parts of an Agent that are not its message plane.
type Option func(*agentOptions)

type agentOptions struct {
	sessions SessionSink
}

// WithSessionSink sets where conversation snapshots are written. Without one the
// agent keeps its JSONL copy only, which is what the tests exercise.
func WithSessionSink(sink SessionSink) Option {
	return func(o *agentOptions) { o.sessions = sink }
}

func New(cfg Config, client BusClient, p provider.Provider, t *tool.Registry, opts ...Option) (*Agent, error) {
	var options agentOptions
	for _, opt := range opts {
		opt(&options)
	}
	store, err := session.NewStore(cfg.SessionDir)
	if err != nil {
		return nil, fmt.Errorf("create session store: %w", err)
	}

	if options.sessions != nil {
		store.SetSyncer(cfg.AgentID, newBusSessionSyncer(options.sessions, cfg.UserID))
	}
	if !cfg.SessionSyncEnabled {
		store.SetSyncEnabled(false)
	}
	if cfg.SessionSyncFlushInterval > 0 {
		store.SetSyncInterval(cfg.SessionSyncFlushInterval)
	}

	summarizeModel := cfg.SummarizeModel
	if summarizeModel == "" {
		summarizeModel = cfg.Model
	}

	ag := &Agent{
		config:     cfg,
		busClient:  client,
		provider:   p,
		tools:      t,
		sessions:   store,
		instanceID: uuid.New().String(),
		summarizer: newSummarizer(p, summarizeModel, cfg.LLMSummarizeReasoningEffort),
	}
	ag.summarizer.SetSystemPrompt(cfg.RolePrompts["summarize"])
	ag.summarizer.agent = ag
	ag.wireGoalJudge()
	return ag, nil
}

// wireGoalJudge gives the goal report tool the utility runner that judges a
// round. It is called again after every capability rebuild, because a rebuild
// replaces the tool instances.
func (a *Agent) wireGoalJudge() {
	if a.tools == nil {
		return
	}
	a.tools.SetGoalJudge(func(ctx context.Context, input string) (string, bool) {
		out, used, err := a.runUtility(ctx, "role_goal_judge", input, "goal-judge", nil)
		if err != nil {
			slog.Warn("goal judge failed", "error", err)
			return "", false
		}
		return out, used
	})
}

// ApplyTools atomically replaces the agent's tool set (capability reloads only
// run between turns, so no in-flight definition snapshot is torn).
func (a *Agent) ApplyTools(tools map[string]tool.Tool) {
	a.tools.ReplaceAll(tools)
	a.wireGoalJudge()
}

// SetToolProvenance records which registered tool names are builtins and which
// MCP server each MCP tool belongs to. It is applied on capability reloads so
// the per-definition filter can gate builtins and MCP tools independently.
func (a *Agent) SetToolProvenance(builtinNames []string, mcpServerByTool map[string]string) {
	builtins := make(map[string]bool, len(builtinNames))
	for _, n := range builtinNames {
		builtins[n] = true
	}
	mcp := make(map[string]string, len(mcpServerByTool))
	for k, v := range mcpServerByTool {
		mcp[k] = v
	}
	a.toolMetaMu.Lock()
	a.builtinNames = builtins
	a.mcpServerByTool = mcp
	a.toolMetaMu.Unlock()
}

// toolProvenance returns the current builtin-name set and MCP tool→server map.
func (a *Agent) toolProvenance() (map[string]bool, map[string]string) {
	a.toolMetaMu.RLock()
	defer a.toolMetaMu.RUnlock()
	return a.builtinNames, a.mcpServerByTool
}

// SetCapabilityRebuilder installs the closure that applies capability catalog
// changes. It must be invoked with an idle agent.
func (a *Agent) SetCapabilityRebuilder(fn func(ctx context.Context) error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.capRebuild = fn
}

// SetProvider swaps the LLM provider (e.g. after a model-catalog change). Must
// be called between turns (the reload path is idle-deferred).
func (a *Agent) SetProvider(p provider.Provider) {
	a.mu.Lock()
	a.provider = p
	a.mu.Unlock()
}

// Provider returns the current LLM provider.
func (a *Agent) Provider() provider.Provider {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.provider
}

// RebuildCapabilities applies the installed capability rebuilder (if any).
func (a *Agent) RebuildCapabilities(ctx context.Context) error {
	a.mu.Lock()
	fn := a.capRebuild
	a.mu.Unlock()
	if fn == nil {
		return nil
	}
	return fn(ctx)
}

// SetQuestionBroker installs the process-wide question broker so the poll loop
// can route answers to blocked question tools.
func (a *Agent) SetQuestionBroker(b *question.Broker) {
	a.mu.Lock()
	a.questions = b
	a.mu.Unlock()
}

// InjectSystemMessage processes a synthetic message on the given session as if
// it had arrived from a user (used for background subagent results so the model
// can continue automatically). It runs asynchronously under the session lock.
func (a *Agent) InjectSystemMessage(sessionID, channel, chatID, content string) {
	msg := &nanoprotocol.InboundMessage{
		UserID:    a.config.UserID,
		AgentID:   a.config.AgentID,
		SessionID: sessionID,
		Channel:   channel,
		ChatID:    chatID,
		SenderID:  "system",
		MessageID: messageid.New(),
		Content:   content,
		Metadata:  map[string]any{"subagent_result": true},
		Timestamp: time.Now(),
	}
	go a.processMessageSerialized(context.Background(), msg)
}

// compactFromMetaKey records the index into ConversationMessages from which the
// LLM context should start (everything before it is covered by the summary).
const compactFromMetaKey = "compact_from"

// unattendedSources are the metadata source values that mean "no human is
// watching this turn": the scheduler ("cron") and the long-running goal loop
// ("goal"). A whitelist rather than a comparison, because the next unattended
// producer has to be named here on purpose — a default of "unattended" would
// silently give an ordinary user turn the scheduler's behaviour.
var unattendedSources = map[string]bool{
	"cron": true,
	"goal": true,
}

// isScheduledTurn reports whether a message was started by the scheduler or the
// goal loop rather than by a person, so the turn runs unattended.
func isScheduledTurn(msg *nanoprotocol.InboundMessage) bool {
	if msg == nil || msg.Metadata == nil {
		return false
	}
	src, _ := msg.Metadata[metadataSourceKey].(string)
	return unattendedSources[src]
}

// goalContextOf reads the goal binding a round carries. A message that is not a
// goal round gets the zero value, which is what makes goal_report refuse outside
// the loop.
func goalContextOf(msg *nanoprotocol.InboundMessage) tool.GoalContext {
	if msg == nil || msg.Metadata == nil {
		return tool.GoalContext{}
	}
	src, _ := msg.Metadata[metadataSourceKey].(string)
	if src != "goal" {
		return tool.GoalContext{}
	}
	id, _ := msg.Metadata["goal_id"].(string)
	round := 0
	switch v := msg.Metadata["round"].(type) {
	case float64:
		round = int(v)
	case int:
		round = v
	case int64:
		round = int(v)
	}
	verify, _ := msg.Metadata["verify"].(bool)
	return tool.GoalContext{GoalID: id, Round: round, Verify: verify, Instruction: msg.Content}
}

// scheduledTaskName returns the task name a scheduled run carries ("" for
// ordinary messages).
func scheduledTaskName(msg *nanoprotocol.InboundMessage) string {
	if msg == nil || msg.Metadata == nil {
		return ""
	}
	name, _ := msg.Metadata[metadataCronNameKey].(string)
	return name
}

// Session metadata keys backing context continuity across a session split and
// across a mode (plan <-> build) switch.
const (
	// metadataSourceKey marks who started a turn. Only the scheduler sets it
	// today (value "cron"): such a turn has no user behind it.
	metadataSourceKey = "source"
	// metadataCronNameKey carries the task name of a scheduled run, used to
	// name its conversation.
	metadataCronNameKey = "cron_name"
	// handoffSummaryMetaKey marks the summary carried across a session split, so
	// a new segment does not start from nothing.
	handoffSummaryMetaKey = "handoff_summary"
	// handoffFromMetaKey records the archived session a new segment started from.
	handoffFromMetaKey = "handoff_from"
	// sessionAgentMetaKey records the session agent id used by the last turn, so
	// a switch (plan -> build) can be detected.
	sessionAgentMetaKey = "_session_agent"
	// sessionModeNoteMetaKey holds the ephemeral mode note injected into the
	// LLM context; it is never persisted to the transcript.
	sessionModeNoteMetaKey = "_mode_note"
	// lastPromptTokensMetaKey caches the provider's own prompt-token count, the
	// ground truth for the next auto-compaction decision.
	lastPromptTokensMetaKey = "_last_prompt_tokens"
	// lastPlanPathMetaKey is the newest plan artefact written by plan_write.
	lastPlanPathMetaKey = "last_plan_path"
	// planExecutedMetaKey records the plan the build agent closed out with
	// plan_execute, so the mode note stops being injected for it.
	planExecutedMetaKey = "plan_executed"
	// splitPendingMetaKey marks a compacted segment: the next turn archives it
	// and continues in a new version, which is the only automatic session
	// boundary (a mid-turn split would leave the user message behind).
	splitPendingMetaKey = "_split_pending"
	// compactingMetaKey marks a compaction in flight, so a client that reads the
	// session (a reload, a reconnect) can show the "compacting" divider again:
	// the transcript's own divider is only written once the compaction finishes.
	// It is not a liveness signal — that is turn_running, which carries the
	// staleness check a crashed agent needs.
	compactingMetaKey = "_compacting"
)

// contextMessages returns the LLM-facing messages with the compaction floor
// applied (the summarized prefix is dropped).
func (a *Agent) contextMessages(sess *session.Session) []provider.Message {
	msgs := sess.ConversationMessages()
	if from := compactFrom(sess); from > 0 && from < len(msgs) {
		msgs = msgs[from:]
	}
	return msgs
}

func compactFrom(sess *session.Session) int {
	raw := sess.GetMeta(compactFromMetaKey)
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// persistMemoryFlush commits a pending pre-compaction memory flush to the
// long-term memory file and clears the marker. Best-effort: on failure the
// marker is retained (and a later end-of-turn retry may commit it).
func (a *Agent) persistMemoryFlush(sess *session.Session) {
	content := sess.GetMeta("_pending_memory_flush")
	if content == "" {
		return
	}
	if a.ctxBuilder == nil {
		return
	}
	if err := a.ctxBuilder.AppendMemory(content); err != nil {
		slog.Error("failed to persist memory flush", "session", sess.ID, "error", err)
		return
	}
	sess.SetMeta("_pending_memory_flush", "")
	slog.Info("memory flush persisted", "session", sess.ID, "bytes", len(content))
}

// pinnedPrefix returns the request parts that sit ahead of the conversation and
// must never be truncated: the running summary (a role "user" row, which is what
// the model sees it as).
//
// The mode note used to live here too; it is now an ordinary persisted system row
// appended before each turn's user message (see injectSessionState), because a
// note pinned *ahead* of the conversation shifts the whole transcript whenever it
// changes and thereby throws away the cached prefix.
func pinnedPrefix(sess *session.Session) []provider.Message {
	var pinned []provider.Message
	if summary := sess.GetSummary(); summary != "" {
		pinned = append(pinned, provider.Message{Role: "user", Content: "[Previous conversation summary]\n" + summary})
	}
	return pinned
}

// promptTokens estimates the token cost of the next request the way
// buildRequestMessages assembles it: the conversation after the compaction
// floor, bounded to what is left of the budget once the pinned prefix is paid
// for, plus the pinned prefix and the system prompt. The compaction gate
// compares this against compactionPlan's threshold, so the two have to measure
// the same thing.
func promptTokens(sess *session.Session, budget, systemTokens int) int {
	pinned := pinnedPrefix(sess)
	view := sess.ConversationMessages()
	if from := compactFrom(sess); from > 0 && from < len(view) {
		view = view[from:]
	}
	view = provider.SanitizeMessages(view)
	pinnedTokens := tokenEstimateHeuristic(pinned)
	tail, _ := truncateToBudget(view, budget-pinnedTokens)
	return tokenEstimateHeuristic(tail) + pinnedTokens + systemTokens
}

// headMessages returns the request head for one turn: head 1 (the static system
// prompt) followed by the frozen per-file segments of the session's current
// segment. The head is byte-stable for the life of a segment.
func (a *Agent) headMessages(sess *session.Session, channel, chatID, prompt string) []provider.Message {
	if a.ctxBuilder == nil {
		return nil
	}
	var head []provider.Message
	if h1 := a.ctxBuilder.BuildSystemPrompt(channel, chatID, prompt); h1 != "" {
		head = append(head, provider.Message{Role: "system", Content: h1})
	}
	return append(head, a.headSnapshotFor(sess).messages...)
}

// estimateUsage approximates the prompt tokens of the next request (head +
// summary + compacted conversation) so the context indicator updates immediately
// after a compaction, before the next real LLM call.
func (a *Agent) estimateUsage(sess *session.Session, channel, chatID, prompt string) int {
	headTokens := tokenEstimateHeuristic(a.headMessages(sess, channel, chatID, prompt))
	return promptTokens(sess, budgetAvailable(a.config.ContextWindow, a.config.MaxOutputTokens), headTokens)
}

// emitUsage posts a status chunk carrying the latest token usage so the portal
// can show context usage.
func (a *Agent) emitUsage(msg *nanoprotocol.InboundMessage, u provider.Usage) {
	if u.PromptTokens == 0 && u.CompletionTokens == 0 {
		return
	}
	a.emitUsageValues(msg, u.PromptTokens+u.CompletionTokens, a.config.ContextWindow)
}

// emitUsageValues posts a usage status chunk for an explicit used/limit pair.
// budget is the prompt budget auto-compaction triggers against (the context
// window minus the output reservation) and threshold is the token count the
// gate actually fires at; the client draws the latter as its threshold marker,
// so the marker and the gate cannot drift apart (the client used to hard-code
// 75% of the budget while the gate read a configurable percentage).
func (a *Agent) emitUsageValues(msg *nanoprotocol.InboundMessage, used, limit int) {
	pct := 0.0
	if limit > 0 {
		pct = float64(used) / float64(limit) * 100
	}
	budget, threshold := compactionPlan(a.config.ContextWindow, a.config.MaxOutputTokens, a.config.SummarizeTokenPercent)
	payload, _ := json.Marshal(map[string]any{
		"event":     "usage",
		"state":     "",
		"used":      used,
		"limit":     limit,
		"pct":       pct,
		"budget":    budget,
		"threshold": threshold,
	})
	if err := a.busClient.PostOutboundChunk(nanoprotocol.OutboundChunk{
		MessageID: messageid.New(),
		AgentID:   a.config.AgentID,
		UserID:    a.config.UserID,
		Channel:   msg.Channel,
		ChatID:    msg.ChatID,
		SessionID: msg.SessionID,
		Kind:      nanoprotocol.ChunkKindStatus,
		Content:   string(payload),
	}); err != nil {
		slog.Warn("failed to post usage chunk", "error", err)
	}
}

// EmitStatus posts a status chunk (exported wrapper for the runtime wiring).
func (a *Agent) EmitStatus(event, state string) {
	a.emitStatus(event, state)
}

// postSummaryChunk posts one summary chunk — a streamed delta or a whole
// finished summary — to a specific chat, so a client that opened a "Compacting…"
// divider can fill it in. The turn must pass its own message's ref: the agent
// runs a few messages concurrently, so the shared lastMessage can belong to
// another conversation by the time a compaction reports.
func (a *Agent) postSummaryChunk(ref *messageRef, messageID, content string) {
	if ref == nil || content == "" {
		return
	}
	chunk := nanoprotocol.OutboundChunk{
		MessageID: messageID,
		AgentID:   a.config.AgentID,
		UserID:    a.config.UserID,
		Channel:   ref.channel,
		ChatID:    ref.chatID,
		SessionID: ref.sessionID,
		Kind:      nanoprotocol.ChunkKindSummary,
		Content:   content,
	}
	if err := a.busClient.PostOutboundChunk(chunk); err != nil {
		slog.Warn("failed to post summary chunk", "agent", a.config.AgentID, "error", err)
	}
}

// reloadConfig refetches the effective config and hot-applies it (single-agent
// mode; the router path uses AgentHandler.reloadConfig).
func (a *Agent) reloadConfig() {
	slog.Info("reloading config", "agent", a.config.AgentID)
	items, err := config.FetchItems("agent")
	if err != nil {
		slog.Warn("reload config: get items failed", "agent", a.config.AgentID, "error", err)
		return
	}
	cfg := config.BuildAgentConfigFor(a.baseConfig, items, a.config.AgentID)
	a.ApplySettings(cfg)
}

// rememberMessage records the session context of the latest inbound message.
func (a *Agent) rememberMessage(msg *nanoprotocol.InboundMessage) {
	a.msgMu.Lock()
	defer a.msgMu.Unlock()
	a.lastMessage = &messageRef{
		channel:   msg.Channel,
		chatID:    msg.ChatID,
		sessionID: msg.SessionID,
	}
}

// currentRef returns the session context of the most recent inbound message.
// Agent-wide events (a capability reload) use it; a turn must use its own
// message's ref instead (see messageRefOf).
func (a *Agent) currentRef() *messageRef {
	a.msgMu.Lock()
	defer a.msgMu.Unlock()
	return a.lastMessage
}

// emitStatus posts a status chunk to the last-known chat so the portal can
// surface lifecycle events. Best-effort: skipped without a known session.
func (a *Agent) emitStatus(event, state string) {
	a.emitStatusTo(a.currentRef(), event, state)
}

// reportSyncHealth surfaces a stalled external snapshot path to the chat once it
// starts failing.
//
// The JSONL file stays authoritative, so this warns about the *portal's* view and
// about the platform's datastore health, not about the agent's memory. It exists
// because the opposite failure mode is silent: a session snapshot that keeps
// failing looks exactly like a session nobody has touched.
func (a *Agent) reportSyncHealth(ref *messageRef) {
	health := a.sessions.SyncHealth()
	if health.Failures == 0 || health.Failures == a.reportedSyncFailures.Load() {
		return
	}
	a.reportedSyncFailures.Store(health.Failures)
	a.emitStatusTo(ref, "session_sync", "failed")
}

// emitStatusTo posts a status chunk to one specific chat. A turn passes its own
// message's ref, because the shared lastMessage can point at another
// conversation while a long command is still running.
func (a *Agent) emitStatusTo(ref *messageRef, event, state string) {
	if ref == nil {
		return
	}
	payload, _ := json.Marshal(map[string]string{"event": event, "state": state})
	chunk := nanoprotocol.OutboundChunk{
		MessageID: messageid.New(),
		AgentID:   a.config.AgentID,
		UserID:    a.config.UserID,
		Channel:   ref.channel,
		ChatID:    ref.chatID,
		SessionID: ref.sessionID,
		Kind:      nanoprotocol.ChunkKindStatus,
		Content:   string(payload),
	}
	if err := a.busClient.PostOutboundChunk(chunk); err != nil {
		slog.Warn("failed to post status chunk", "agent", a.config.AgentID, "error", err)
	}
}

// SessionSink applies one snapshot to the conversation projection. The portal
// owns it; the syncer does not care which service answers.
//
// It sends only the messages the bus has not acknowledged yet. Within a segment
// the conversation is append-only, so "what is new" is a suffix — and sending the
// suffix is the whole point: the payload stops being the whole conversation, and
// the bus stops rewriting it once a second (which is what filled the disk).
//
// The index it tracks is per session and held in memory: a segment that ends is
// never written to again (compaction starts a new session id), and losing the map
// to a restart costs one full snapshot.
type busSessionSyncer struct {
	client    SessionSink
	accountID string

	mu    sync.Mutex
	acked map[string]int
}

func newBusSessionSyncer(client SessionSink, accountID string) *busSessionSyncer {
	return &busSessionSyncer{client: client, accountID: accountID, acked: map[string]int{}}
}

func (s *busSessionSyncer) ackedIndex(sessionID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.acked[sessionID]
}

func (s *busSessionSyncer) setAcked(sessionID string, index int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acked[sessionID] = index
}

func (s *busSessionSyncer) SyncSession(snap session.SessionSnapshot) (int, error) {
	if s.client == nil {
		return 0, nil
	}
	baseKey, _ := session.ParseSessionKey(snap.SessionID)
	channel, chatID := splitBaseKey(baseKey)
	metaJSON := ""
	title := ""
	if len(snap.Meta) > 0 {
		if t := snap.Meta["_title"]; t != "" {
			title = t
		}
		// meta (including "_title") is forwarded verbatim so every sync carries
		// the LLM-derived title; without it the bus would re-derive a heuristic
		// title and overwrite the smarter one.
		if b, err := json.Marshal(snap.Meta); err == nil {
			metaJSON = string(b)
		}
	}
	status := "active"
	if snap.Meta["_archived"] == "true" {
		status = "archived"
	}
	req := nanoprotocol.SaveSessionRequest{
		SessionID:   snap.SessionID,
		AgentID:     snap.AgentID,
		AccountID:   s.accountID,
		Channel:     channel,
		ChatID:      chatID,
		Summary:     snap.Summary,
		Title:       title,
		Meta:        metaJSON,
		Status:      status,
		TurnRunning: snap.TurnRunning,
	}

	// A suffix only means something once the bus holds the prefix: the first
	// snapshot of a session is the whole conversation, and so is the one after a
	// resync. A segment that shrank cannot be a suffix of what was sent either.
	acked := s.ackedIndex(snap.SessionID)
	if acked > len(snap.Messages) {
		acked = 0
	}
	if acked == 0 {
		return s.sendFull(snap, req)
	}

	delta := snap.Messages[acked:]
	payload, err := json.Marshal(delta)
	if err != nil {
		return 0, fmt.Errorf("marshal session delta: %w", err)
	}
	req.Messages = string(payload)
	req.MessagesOmitted = true
	req.FromIndex = acked

	err = s.client.SaveSession(req)
	if errors.Is(err, nanoprotocol.ErrSessionResync) {
		// The bus is not holding the prefix this delta follows — a snapshot was
		// lost. The recovery is the whole conversation, which is also why the
		// store refuses the delta instead of leaving a hole in the history.
		s.setAcked(snap.SessionID, 0)
		return s.sendFull(snap, req)
	}
	if err != nil {
		return 0, err
	}
	s.setAcked(snap.SessionID, len(snap.Messages))
	return len(payload), nil
}

// sendFull sends the whole conversation, which is what establishes (or
// re-establishes) the prefix later deltas are measured against.
func (s *busSessionSyncer) sendFull(snap session.SessionSnapshot, req nanoprotocol.SaveSessionRequest) (int, error) {
	payload, err := json.Marshal(snap.Messages)
	if err != nil {
		return 0, fmt.Errorf("marshal session messages: %w", err)
	}
	req.Messages = string(payload)
	req.MessagesOmitted = false
	req.FromIndex = 0
	if err := s.client.SaveSession(req); err != nil {
		return 0, err
	}
	s.setAcked(snap.SessionID, len(snap.Messages))
	return len(payload), nil
}

// splitBaseKey splits a session base key "channel:chatID" into its parts.
// The chat ID itself may contain colons, so only the first segment is the channel.
func splitBaseKey(baseKey string) (channel, chatID string) {
	idx := strings.Index(baseKey, ":")
	if idx < 0 {
		return baseKey, ""
	}
	return baseKey[:idx], baseKey[idx+1:]
}

// SetContextBuilder attaches a system prompt builder to the agent.
func (a *Agent) SetContextBuilder(cb *ContextBuilder) {
	a.ctxBuilder = cb
}

// SetBaseConfig records the process-level config used as the base for
// per-agent config layering during hot reloads.
func (a *Agent) SetBaseConfig(cfg config.AgentConfig) {
	a.baseConfig = cfg
}

// SetAgents installs the unified resolved definition registry, the switchable
// session agent ids, and the default id.
func (a *Agent) SetAgents(agents map[string]ResolvedAgent, sessionAgentIDs []string, defaultID string) {
	a.mu.Lock()
	a.config.Agents = agents
	a.config.SessionAgentIDs = sessionAgentIDs
	a.config.DefaultAgent = defaultID
	a.mu.Unlock()
}

// SetRolePrompts installs the internal role prompt overrides.
func (a *Agent) SetRolePrompts(prompts map[string]string) {
	a.mu.Lock()
	a.config.RolePrompts = prompts
	a.mu.Unlock()
	if a.summarizer != nil {
		a.summarizer.SetSystemPrompt(prompts["summarize"])
	}
}

// ApplySettings hot-updates runtime agent settings (used on config reload).
func (a *Agent) ApplySettings(cfg config.AgentConfig) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.config.Model = cfg.Provider.DefaultModel
	// Title/summarize roles follow the resolved main model; per-agent overrides
	// are applied at construction and are not part of the reloaded config.
	a.config.TitleModel = cfg.Provider.DefaultModel
	a.config.SummarizeModel = cfg.Provider.DefaultModel
	a.config.MaxIterations = cfg.Agent.MaxIterations
	a.config.LLMRequestTimeout = cfg.Agent.LLMRequestTimeout
	a.config.ContextWindow = cfg.Agent.ContextWindow
	a.config.MaxOutputTokens = cfg.Agent.MaxOutputTokens
	a.config.ThinkingEffort = cfg.Agent.ThinkingEffort
	a.config.Temperature = cfg.Agent.Temperature
	a.config.TopP = cfg.Agent.TopP
	a.config.MaxToolResultChars = cfg.Agent.MaxToolResultChars
	// A trigger the budget-windowed request can never reach would silence the
	// guard; clamp it (and say so) instead of storing a value that does nothing.
	if percent, clamped := normalizeTriggerPercent(cfg.Agent.SummarizeTokenPercent); clamped {
		slog.Warn("summarize_token_percent out of range; clamped",
			"configured", cfg.Agent.SummarizeTokenPercent, "used", percent)
		cfg.Agent.SummarizeTokenPercent = percent
	}
	a.config.SummarizeTokenPercent = cfg.Agent.SummarizeTokenPercent
	a.config.SummarizeMessageThreshold = cfg.Agent.SummarizeMessageThreshold
	a.config.TitleRefreshMessages = cfg.Agent.TitleRefreshMessages
	a.config.MemoryRecentDays = cfg.Agent.MemoryRecentDays
	if a.ctxBuilder != nil {
		a.ctxBuilder.SetMemoryRecentDays(cfg.Agent.MemoryRecentDays)
	}
}

// applyModeSettings overlays the active mode's runtime policy settings onto the
// agent config so per-(agent, mode) overrides take effect for the turn. Model,
// thinking, prompts, tools and permissions are already per-def (the spec).
func (a *Agent) applyModeSettings(spec ResolvedAgent) {
	if len(spec.Settings) == 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if v, ok := spec.Settings["summarize_token_percent"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			if percent, clamped := normalizeTriggerPercent(n); clamped {
				slog.Warn("summarize_token_percent out of range; clamped",
					"configured", n, "used", percent, "definition", spec.ID)
				n = percent
			}
			a.config.SummarizeTokenPercent = n
		}
	}
	if v, ok := spec.Settings["summarize_message_threshold"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			a.config.SummarizeMessageThreshold = n
		}
	}
	if v, ok := spec.Settings["title_refresh_messages"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			a.config.TitleRefreshMessages = n
		}
	}
	if v, ok := spec.Settings["memory_recent_days"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			a.config.MemoryRecentDays = n
			if a.ctxBuilder != nil {
				a.ctxBuilder.SetMemoryRecentDays(n)
			}
		}
	}
	if v, ok := spec.Settings["max_tool_result_chars"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			a.config.MaxToolResultChars = n
		}
	}
}

// pollInterval is how long to wait between polls when no push arrives: the
// socket makes polling a safety net, so it can be slow; without one it is the
// only source of work.
func (a *Agent) pollInterval() time.Duration {
	if s, ok := a.busClient.(interface{ SocketConnected() bool }); ok && s.SocketConnected() {
		return socketPollInterval
	}
	return fallbackPollInterval
}

// inboundReady is the bus's "work arrived" signal, or nil when the client has
// no socket (selecting on nil blocks forever, which is what we want).
func (a *Agent) inboundReady() <-chan struct{} {
	if s, ok := a.busClient.(interface{ InboundReady() <-chan struct{} }); ok {
		return s.InboundReady()
	}
	return nil
}

// Run starts the agent loop.
func (a *Agent) Run(ctx context.Context) error {
	slog.Info("agent started",
		"user", a.config.UserID,
		"instance", a.instanceID,
		"bus", a.config.BusURL,
	)
	defer a.busClient.Close()
	defer a.sessions.Close()

	// With an agent socket the bus says when work arrives, so the fallback poll
	// only has to catch a notice that never made it. Without one, the poll is the
	// only way to learn about work and keeps the old cadence.
	ticker := time.NewTicker(a.pollInterval())
	defer ticker.Stop()
	ready := a.inboundReady()

	changeCh := a.busClient.ConfigChanges()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ready:
			// A push says a message is waiting: claim it now instead of at the
			// next tick. The tick keeps its period, so this cannot starve it.
			ticker.Reset(time.Millisecond)
		case change, ok := <-changeCh:
			if !ok {
				changeCh = nil
				continue
			}
			// This agent is single-purpose: forward changes that affect it
			// (AgentID empty = global default change).
			if change.AgentID != "" && change.AgentID != a.config.AgentID {
				continue
			}
			a.handleChange(ctx, change)
		case <-ticker.C:
			ticker.Reset(a.pollInterval())
			// Poll for inbound message routed to this agent.
			msg, err := a.busClient.PollInbound(a.config.UserID, a.config.BindingID, a.config.AgentID)
			if err != nil {
				slog.Error("poll inbound failed", "error", err)
				time.Sleep(1 * time.Second) // Backoff on error
				continue
			}

			if msg == nil {
				continue // No message available
			}

			slog.Info("received message",
				"session", msg.SessionID,
				"content", msg.Content,
			)

			// /stop aborts the in-flight turn for this session and must never be
			// queued or turned into a turn itself.
			if strings.TrimSpace(msg.Content) == "/stop" {
				if a.cancelTurn(msg.SessionID) {
					slog.Info("turn stopped by user", "session", msg.SessionID)
				}
				continue
			}

			// /goal starts a long-running goal from one line; it is a command,
			// not a turn, so the transcript does not grow a user message the
			// model would then have to answer.
			if desc, ok := parseGoalCommand(msg.Content); ok {
				go a.handleGoalCommand(ctx, msg, desc)
				continue
			}

			// Route an answer to a waiting question instead of starting a turn.
			if a.questions != nil {
				if qid, _ := msg.Metadata["question_id"].(string); qid != "" {
					if a.questions.Deliver(qid, extractAnswers(msg)) {
						continue
					}
				}
				// A permission-approval answer carries approval_id + decision
				// and resolves the same broker waiter by id.
				if aid, _ := msg.Metadata[nanoprotocol.MetadataApprovalID].(string); aid != "" {
					if a.questions.Deliver(aid, extractApprovalDecision(msg)) {
						continue
					}
				}
				// While a question is pending for this session, hold normal
				// messages in a durable queue instead of starting a new turn.
				if a.questions.IsWaiting(msg.SessionID) {
					a.enqueueQuestionMessage(msg)
					continue
				}
			}

			// Turns run concurrently (one goroutine per message) but are
			// serialized per session so a blocked question cannot interleave
			// with a newer turn for the same conversation.
			go a.processMessageSerialized(ctx, msg)
		}
	}
}

// processMessageSerialized runs a turn under the session lock, then drains any
// queued messages now that a pending question (if any) has resolved.
func (a *Agent) processMessageSerialized(ctx context.Context, msg *nanoprotocol.InboundMessage) {
	lock := a.sessionLock(msg.SessionID)
	lock.Lock()
	defer lock.Unlock()

	turnCtx, cancel := context.WithCancel(ctx)
	a.registerTurn(msg.SessionID, cancel)
	defer a.unregisterTurn(msg.SessionID)

	a.inFlight.Add(1)
	err := a.processMessage(turnCtx, msg)
	a.inFlight.Add(-1)
	if err != nil {
		slog.Error("process message failed", "session", msg.SessionID, "error", err)
	}
	a.tryApplyPending(ctx)
	a.drainQuestionQueue(turnCtx, msg.SessionID)
}

// registerTurn records the cancel func for a session's in-flight turn.
func (a *Agent) registerTurn(sessionID string, cancel context.CancelFunc) {
	a.turnMu.Lock()
	if a.turns == nil {
		a.turns = make(map[string]context.CancelFunc)
	}
	a.turns[sessionID] = cancel
	a.turnMu.Unlock()
}

func (a *Agent) unregisterTurn(sessionID string) {
	a.turnMu.Lock()
	delete(a.turns, sessionID)
	a.turnMu.Unlock()
}

// cancelTurn aborts the in-flight turn for a session, returning whether one was
// running.
func (a *Agent) cancelTurn(sessionID string) bool {
	a.turnMu.Lock()
	cancel, ok := a.turns[sessionID]
	a.turnMu.Unlock()
	if ok {
		cancel()
	}
	return ok
}

func (a *Agent) sessionLock(sessionID string) *sync.Mutex {
	a.sessMu.Lock()
	defer a.sessMu.Unlock()
	if a.sessLocks == nil {
		a.sessLocks = make(map[string]*sync.Mutex)
	}
	l, ok := a.sessLocks[sessionID]
	if !ok {
		l = &sync.Mutex{}
		a.sessLocks[sessionID] = l
	}
	return l
}

// handleChange applies a config/capability change, deferring it until no turns
// are in flight (a rebuild mid-turn would tear the tool set).
func (a *Agent) handleChange(ctx context.Context, change nanoprotocol.ConfigChange) {
	a.reloadMu.Lock()
	if a.inFlight.Load() > 0 {
		c := change
		a.pendingChange = &c
		a.reloadMu.Unlock()
		return
	}
	a.reloadMu.Unlock()
	a.applyChange(ctx, change)
}

func (a *Agent) tryApplyPending(ctx context.Context) {
	a.reloadMu.Lock()
	c := a.pendingChange
	if c == nil || a.inFlight.Load() > 0 {
		a.reloadMu.Unlock()
		return
	}
	a.pendingChange = nil
	a.reloadMu.Unlock()
	a.applyChange(ctx, *c)
}

func (a *Agent) applyChange(ctx context.Context, change nanoprotocol.ConfigChange) {
	// Any config change re-pulls the agent snapshot on the idle-deferred
	// rebuild path, so settings, prompt and tools all refresh without a restart.
	_ = change
	a.emitStatus("capabilities_reload", "pending")
	if err := a.RebuildCapabilities(ctx); err != nil {
		slog.Error("capability reload failed", "agent", a.config.AgentID, "error", err)
	}
	a.emitStatus("capabilities_reload", "applied")
}

// enqueueQuestionMessage appends a message to the session's durable queue while
// a question is pending.
func (a *Agent) enqueueQuestionMessage(msg *nanoprotocol.InboundMessage) {
	sess := a.sessions.GetOrCreate(msg.SessionID)
	var queue []nanoprotocol.InboundMessage
	if raw := sess.GetMeta("_question_queue"); raw != "" {
		_ = json.Unmarshal([]byte(raw), &queue)
	}
	queue = append(queue, *msg)
	if b, err := json.Marshal(queue); err == nil {
		sess.SetMeta("_question_queue", string(b))
	}
	if err := a.sessions.Save(sess); err != nil {
		slog.Warn("failed to persist question queue", "session", msg.SessionID, "error", err)
	}
}

// drainQuestionQueue processes messages queued while a question was pending.
func (a *Agent) drainQuestionQueue(ctx context.Context, sessionID string) {
	sess := a.sessions.GetOrCreate(sessionID)
	raw := sess.GetMeta("_question_queue")
	if raw == "" {
		return
	}
	sess.SetMeta("_question_queue", "")
	if err := a.sessions.Save(sess); err != nil {
		slog.Warn("failed to clear question queue", "session", sessionID, "error", err)
	}
	var queue []nanoprotocol.InboundMessage
	if err := json.Unmarshal([]byte(raw), &queue); err != nil {
		return
	}
	for i := range queue {
		if err := a.processMessage(ctx, &queue[i]); err != nil {
			slog.Error("queued message failed", "session", sessionID, "error", err)
		}
	}
}

// extractAnswers pulls the answer list from an inbound message's metadata.
func extractAnswers(msg *nanoprotocol.InboundMessage) []string {
	if msg.Metadata == nil {
		return nil
	}
	raw, ok := msg.Metadata["answers"]
	if !ok {
		return nil
	}
	switch v := raw.(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, it := range v {
			if s, ok := it.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return v
	case string:
		return []string{v}
	}
	return nil
}

// extractApprovalDecision pulls the decision from a permission-approval answer
// (metadata.decision), falling back to the answers list.
func extractApprovalDecision(msg *nanoprotocol.InboundMessage) []string {
	if msg.Metadata == nil {
		return nil
	}
	if d, ok := msg.Metadata[nanoprotocol.MetadataDecision].(string); ok && d != "" {
		return []string{d}
	}
	return extractAnswers(msg)
}
