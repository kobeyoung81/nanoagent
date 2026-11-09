package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanosec/internal/config"
	"gitee.com/kobeyoung81/nanosec/internal/messageid"
)

// AgentHandler wraps a fully-constructed Agent with per-agent concurrency limits
// and configuration reload support.
type AgentHandler struct {
	ID         string
	Config     AgentConfig
	baseConfig config.AgentConfig
	busClient  BusClient
	agent      *Agent
	semaphore  chan struct{}

	// inFlight counts turns currently being processed; capability changes are
	// applied only when it reaches zero (the agent is idle).
	inFlight atomic.Int64
	// pendingCapReload indicates a deferred capability reload is queued.
	pendingCapReload atomic.Bool

	reloadMu sync.Mutex

	msgMu       sync.Mutex
	lastMessage *messageRef
}

// messageRef is the session context of the most recent message, used to target
// status chunks (e.g. capability reload notifications) to the right chat.
type messageRef struct {
	channel   string
	chatID    string
	sessionID string
}

// messageRefOf is the same context for one specific message. A turn holds its
// own ref for the whole turn: the agent processes several messages at once, so
// the shared lastMessage can point at another conversation by the time a long
// command (a compaction) reports.
func messageRefOf(msg *nanoprotocol.InboundMessage) *messageRef {
	if msg == nil {
		return nil
	}
	return &messageRef{channel: msg.Channel, chatID: msg.ChatID, sessionID: msg.SessionID}
}

// AgentConfig holds per-agent runtime settings surfaced for management.
type AgentConfig struct {
	Model                 string
	MaxIterations         int
	MaxConcurrentMessages int
	Workspace             string
	SharedWorkspace       string
}

// NewAgentHandler wraps a built Agent for the given record.
func NewAgentHandler(record nanoprotocol.AgentRecord, ag *Agent) *AgentHandler {
	maxConc := record.MaxConcurrentMessages
	if maxConc <= 0 {
		maxConc = 4
	}

	return &AgentHandler{
		ID: record.ID,
		Config: AgentConfig{
			Model:                 ag.config.Model,
			MaxIterations:         ag.config.MaxIterations,
			MaxConcurrentMessages: maxConc,
			Workspace:             fmt.Sprintf("agents/%s", record.ID),
		},
		baseConfig: ag.baseConfig,
		busClient:  ag.busClient,
		agent:      ag,
		semaphore:  make(chan struct{}, maxConc),
	}
}

func (h *AgentHandler) processMessageWithLimit(ctx context.Context, msg *nanoprotocol.InboundMessage) {
	select {
	case h.semaphore <- struct{}{}:
		defer func() { <-h.semaphore }()
		h.inFlight.Add(1)
		defer func() {
			h.inFlight.Add(-1)
			h.tryApplyCapabilities()
		}()
		if err := h.processMessage(ctx, msg); err != nil {
			slog.Error("process message failed", "agent_id", h.ID, "message_id", msg.MessageID, "error", err)
		}
	case <-ctx.Done():
		return
	}
}

func (h *AgentHandler) processMessage(ctx context.Context, msg *nanoprotocol.InboundMessage) error {
	slog.Info("processing message", "agent_id", h.ID, "message_id", msg.MessageID)
	h.rememberMessage(msg)
	return h.agent.processMessage(ctx, msg)
}

func (h *AgentHandler) rememberMessage(msg *nanoprotocol.InboundMessage) {
	h.msgMu.Lock()
	defer h.msgMu.Unlock()
	h.lastMessage = &messageRef{
		channel:   msg.Channel,
		chatID:    msg.ChatID,
		sessionID: msg.SessionID,
	}
}

// reloadConfig refetches the agent's effective configuration and applies it.
func (h *AgentHandler) reloadConfig(keys []string) {
	slog.Info("reloading config", "agent_id", h.ID, "keys", keys)

	items, err := config.FetchItems("agent")
	if err != nil {
		slog.Warn("reload config: get items failed", "agent_id", h.ID, "error", err)
		return
	}

	cfg := config.BuildAgentConfigFor(h.baseConfig, items, h.ID)
	h.agent.ApplySettings(cfg)
	slog.Info("config applied", "agent_id", h.ID, "model", cfg.Provider.DefaultModel, "max_iterations", cfg.Agent.MaxIterations)
}

// ======== capability reload (idle-deferred) ========

// RequestCapabilityReload schedules a capability catalog reload. If the agent
// is idle the new tool set is applied immediately; otherwise it is applied the
// moment the agent finishes its in-flight turn (LLM watchdogs bound turn
// duration, so this can never stall forever).
func (h *AgentHandler) RequestCapabilityReload() {
	h.pendingCapReload.Store(true)
	if h.inFlight.Load() > 0 {
		h.emitStatus("capabilities_reload", "pending")
		return
	}
	h.tryApplyCapabilities()
}

// tryApplyCapabilities applies a deferred reload if the agent is idle and a
// reload is pending. Exactly one goroutine applies at a time.
func (h *AgentHandler) tryApplyCapabilities() {
	if !h.pendingCapReload.CompareAndSwap(true, false) {
		return
	}
	if h.inFlight.Load() != 0 {
		h.pendingCapReload.Store(true) // raced with a new turn; retry on completion
		return
	}
	go func() {
		h.reloadMu.Lock()
		defer h.reloadMu.Unlock()
		if err := h.agent.RebuildCapabilities(context.Background()); err != nil {
			slog.Error("capability reload failed", "agent_id", h.ID, "error", err)
		}
		h.emitStatus("capabilities_reload", "applied")
	}()
}

// emitStatus posts a status chunk to the agent's last-known chat so the portal
// can surface lifecycle events (e.g. capability reloads). Best-effort: skipped
// when no session context has been seen yet.
func (h *AgentHandler) emitStatus(event, state string) {
	h.msgMu.Lock()
	ref := h.lastMessage
	h.msgMu.Unlock()
	if ref == nil {
		return
	}
	payload, _ := json.Marshal(map[string]string{"event": event, "state": state})
	chunk := nanoprotocol.OutboundChunk{
		MessageID: messageid.New(),
		AgentID:   h.agent.config.AgentID,
		UserID:    h.agent.config.UserID,
		Channel:   ref.channel,
		ChatID:    ref.chatID,
		SessionID: ref.sessionID,
		Kind:      nanoprotocol.ChunkKindStatus,
		Content:   string(payload),
	}
	if err := h.busClient.PostOutboundChunk(chunk); err != nil {
		slog.Warn("failed to post status chunk", "agent_id", h.ID, "error", err)
	}
}
