package agent

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"gitee.com/kobeyoung81/nanoprotocol"
)

type AgentRouter struct {
	userID    string
	busClient BusClient
	agents    map[string]*AgentHandler
	defaultID string
	mu        sync.RWMutex
}

// AgentBuilder constructs a fully-wired Agent for a given agent record.
type AgentBuilder func(nanoprotocol.AgentRecord) (*Agent, error)

func NewAgentRouter(userID string, busClient BusClient, buildAgent AgentBuilder) (*AgentRouter, error) {
	agents, err := busClient.ListAgents(userID)
	if err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}

	if len(agents) == 0 {
		return nil, fmt.Errorf("no agents found for user %s", userID)
	}

	r := &AgentRouter{
		userID:    userID,
		busClient: busClient,
		agents:    make(map[string]*AgentHandler),
	}

	for _, a := range agents {
		if a.Status != "active" {
			continue
		}
		ag, err := buildAgent(a)
		if err != nil {
			slog.Error("build agent failed", "agent_id", a.ID, "user_id", userID, "error", err)
			continue
		}
		handler := NewAgentHandler(a, ag)
		r.agents[a.ID] = handler
		if r.defaultID == "" {
			r.defaultID = a.ID
		}
		slog.Info("agent loaded", "agent_id", a.ID, "user_id", userID)
	}

	if len(r.agents) == 0 {
		return nil, fmt.Errorf("no active agents for user %s", userID)
	}

	return r, nil
}

func (r *AgentRouter) Run(ctx context.Context) error {
	go r.watchConfig(ctx)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			msg, err := r.busClient.PollInbound(r.userID, 0, "")
			if err != nil {
				slog.Error("poll inbound", "error", err)
				time.Sleep(500 * time.Millisecond)
				continue
			}
			if msg == nil {
				time.Sleep(100 * time.Millisecond)
				continue
			}

			handler := r.resolveAgent(msg.AgentID)
			go handler.processMessageWithLimit(ctx, msg)
		}
	}
}

func (r *AgentRouter) resolveAgent(agentID string) *AgentHandler {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if agentID == "" {
		return r.agents[r.defaultID]
	}
	if h, ok := r.agents[agentID]; ok {
		return h
	}
	return r.agents[r.defaultID]
}

func (r *AgentRouter) watchConfig(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case change := <-r.busClient.ConfigChanges():
			r.mu.RLock()
			var affected []*AgentHandler
			if change.AgentID == "" {
				// a default change affects every agent
				for _, handler := range r.agents {
					affected = append(affected, handler)
				}
			} else if handler, ok := r.agents[change.AgentID]; ok {
				affected = append(affected, handler)
			}
			r.mu.RUnlock()

			for _, handler := range affected {
				// Any config change re-pulls the agent snapshot on the
				// idle-deferred rebuild path, so settings, prompt and tools all
				// refresh without a restart.
				handler.RequestCapabilityReload()
			}
		}
	}
}
