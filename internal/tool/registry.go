package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"gitee.com/kobeyoung81/nanosec/internal/provider"
)

// Registry manages the available tools for an agent.
type Registry struct {
	tools map[string]Tool
	mu    sync.RWMutex
}

// NewRegistry creates a new tool registry.
func NewRegistry() *Registry {
	return &Registry{
		tools: make(map[string]Tool),
	}
}

// Register adds a tool to the registry.
func (r *Registry) Register(t Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[t.Name()] = t
}

// ReplaceAll swaps the entire tool set in one atomic step (used when the
// agent's capability settings change between turns). Callers should build the
// full new map first so the swap is seamless.
func (r *Registry) ReplaceAll(tools map[string]Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools = tools
}

// Names returns the currently registered tool names, sorted by name.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Snapshot returns a copy of the current tool map (for atomic swaps on
// capability reload).
func (r *Registry) Snapshot() map[string]Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]Tool, len(r.tools))
	for k, v := range r.tools {
		out[k] = v
	}
	return out
}

// Get returns a tool by name.
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// Unregister drops a tool from the registry. It is used to keep delegation out
// of a subagent's registry: a model can only call what is registered, and a
// definition-level rule could be edited back on later.
func (r *Registry) Unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tools, name)
}

// Execute parses the JSON arguments and executes the tool.
func (r *Registry) Execute(ctx context.Context, name string, argumentsJSON string) (string, error) {
	t, ok := r.Get(name)
	if !ok {
		return "", fmt.Errorf("tool not found: %s", name)
	}

	var params map[string]any
	if argumentsJSON != "" {
		if err := json.Unmarshal([]byte(argumentsJSON), &params); err != nil {
			return "", fmt.Errorf("unmarshal arguments: %w", err)
		}
	} else {
		params = make(map[string]any)
	}

	return t.Execute(ctx, params)
}

// ContextualTool is implemented by tools that need to know the current
// session context (channel + chatID) before they execute.
type ContextualTool interface {
	SetContext(channel, chatID string)
}

// SetContext propagates the session context to every registered tool that
// implements ContextualTool (e.g. CronTool, SpawnTool).
func (r *Registry) SetContext(channel, chatID string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, t := range r.tools {
		if ct, ok := t.(ContextualTool); ok {
			ct.SetContext(channel, chatID)
		}
	}
}

// SessionContextTool is implemented by tools that need the current session id
// in addition to channel/chat (e.g. QuestionTool).
type SessionContextTool interface {
	SetSessionContext(sessionID, channel, chatID string)
}

// HeadlessTool is implemented by tools whose behaviour depends on whether a user
// is present for the turn. A scheduled task runs with no user to answer, so the
// question tool must refuse instead of blocking the turn.
type HeadlessTool interface {
	SetHeadless(headless bool)
}

// PlanContextTool is implemented by tools that act on the conversation's current
// plan artefact (e.g. PlanExecuteTool). The path itself lives in session
// metadata; the registry forwards it so the tool never has to be told by the
// model which plan it is closing.
type PlanContextTool interface {
	SetPlanPath(path string)
}

// SetPlanPath forwards the session's current plan artefact path to every
// registered tool that implements PlanContextTool.
func (r *Registry) SetPlanPath(path string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, t := range r.tools {
		if pt, ok := t.(PlanContextTool); ok {
			pt.SetPlanPath(path)
		}
	}
}

// GoalJudgeTool is implemented by tools that judge a goal round through the
// agent's utility path (e.g. GoalReportTool).
type GoalJudgeTool interface {
	SetGoalJudge(fn func(ctx context.Context, input string) (string, bool))
}

// SetGoalJudge forwards the utility runner to every registered tool that judges
// a goal round.
func (r *Registry) SetGoalJudge(fn func(ctx context.Context, input string) (string, bool)) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, t := range r.tools {
		if gt, ok := t.(GoalJudgeTool); ok {
			gt.SetGoalJudge(fn)
		}
	}
}

// PolicyContextTool is implemented by tools whose behaviour depends on what the
// calling agent may use (e.g. TaskTool, whose delegation is attenuated by the
// caller's own policy).
type PolicyContextTool interface {
	SetPolicy(policy CallerPolicy)
}

// SetPolicy forwards the turn's caller policy to every registered tool that
// implements PolicyContextTool.
func (r *Registry) SetPolicy(policy CallerPolicy) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, t := range r.tools {
		if pt, ok := t.(PolicyContextTool); ok {
			pt.SetPolicy(policy)
		}
	}
}

// SetParallelDispatch forwards the loop's dispatch to every registered tool that
// implements ParallelDispatchTool, so a batch runs its inner calls through the
// same path (and the same permission rules) as a top-level call.
func (r *Registry) SetParallelDispatch(d ParallelDispatch) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, t := range r.tools {
		if pt, ok := t.(ParallelDispatchTool); ok {
			pt.SetParallelDispatch(d)
		}
	}
}

// SetSessionContext propagates the full session context (session id + channel +
// chat) to every registered tool, covering both ContextualTool and
// SessionContextTool implementations.
func (r *Registry) SetSessionContext(sessionID, channel, chatID string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, t := range r.tools {
		if ct, ok := t.(ContextualTool); ok {
			ct.SetContext(channel, chatID)
		}
		if st, ok := t.(SessionContextTool); ok {
			st.SetSessionContext(sessionID, channel, chatID)
		}
	}
}

// SetHeadless tells every registered tool whether this turn has a user who can
// answer (false) or was started by the scheduler / another unattended source
// (true), so tools that would block on a human can refuse instead.
func (r *Registry) SetHeadless(headless bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, t := range r.tools {
		if ht, ok := t.(HeadlessTool); ok {
			ht.SetHeadless(headless)
		}
	}
}

// Definitions returns the list of tool definitions for the LLM, sorted by
// name.
//
// The order is part of the request prefix, and the upstream prompt cache is
// keyed on it: iterating the registry's map produced a different order on every
// turn, which made the first request of every turn a full cache miss (measured:
// swapping two adjacent tool definitions dropped cached tokens from 99.7% to 0).
// Sorting makes the definition block byte-stable for as long as the tool set
// does not change.
func (r *Registry) Definitions() []provider.ToolDef {
	r.mu.RLock()
	defer r.mu.RUnlock()

	defs := make([]provider.ToolDef, 0, len(r.tools))
	for _, t := range r.tools {
		def := provider.ToolDef{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters:  t.Parameters(),
		}
		if raw, ok := t.(interface{ RawParameters() []byte }); ok {
			if p := raw.RawParameters(); len(p) > 0 {
				def.RawParameters = p
			}
		}
		defs = append(defs, def)
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
	return defs
}
