// Package hypervisor is the in-container control plane: it supervises the
// per-agent nanosec processes and exposes the external HTTP bridge (health +
// workspace browser).
package hypervisor

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanosec/internal/agent"
	"gitee.com/kobeyoung81/nanosec/internal/capability"
	"gitee.com/kobeyoung81/nanosec/internal/config"
	"gitee.com/kobeyoung81/nanosec/internal/portalclient"
	"gitee.com/kobeyoung81/nanosec/internal/skillstore"
)

const (
	// maxRestarts caps how many times a crashed agent is restarted before it is
	// marked failed.
	maxRestarts = 3
	// readyFD is the file descriptor the agent child writes "READY" to once
	// it has finished initialization.
	readyFD = 3
)

// AgentDirectory is the agent list and status the supervisor works from. The
// portal owns those rows; the bus no longer serves them over the protocol.
type AgentDirectory interface {
	ListAgents(userID string) ([]portalclient.Agent, error)
	SetAgentStatus(agentID, status string) error
}

// Supervisor spawns and supervises one child process per active agent.
type Supervisor struct {
	userID    string
	bindingID int64
	busURL    string
	bus       *nanoprotocol.Client
	agents    AgentDirectory

	// paths/dataRoot locate each agent home so the supervisor can materialize
	// catalog skills before spawning the agent process.
	paths    config.PathsConfig
	dataRoot string

	mu       sync.Mutex
	children map[string]*child
}

type child struct {
	agentID  string
	cmd      *exec.Cmd
	ready    *os.File
	restarts atomic.Int32
}

// New creates a Supervisor for a user.
func New(userID string, bindingID int64, busURL string, bus *nanoprotocol.Client, agents AgentDirectory) *Supervisor {
	return &Supervisor{
		userID:    userID,
		bindingID: bindingID,
		busURL:    busURL,
		bus:       bus,
		agents:    agents,
		children:  make(map[string]*child),
	}
}

// SetPaths supplies the filesystem layout used to materialize skills into each
// agent's home. dataRoot is the resolved data root.
func (s *Supervisor) SetPaths(paths config.PathsConfig, dataRoot string) {
	s.paths = paths
	s.dataRoot = dataRoot
}

// prepare ensures the agent home exists and materializes its enabled skills
// from the platform catalog before the agent process starts. It is safe to
// call repeatedly (for example after a capability change).
func (s *Supervisor) prepare(agentID string) {
	if s.dataRoot == "" {
		return
	}
	snap, err := s.bus.GetAgentSnapshot(agentID)
	if err != nil {
		slog.Warn("prepare: fetch snapshot failed", "agent_id", agentID, "error", err)
		return
	}
	homeName, err := agent.EnsureAgentHome(s.dataRoot, agentID, snap.Agent.DisplayName)
	if err != nil {
		slog.Warn("prepare: ensure agent home failed", "agent_id", agentID, "error", err)
		return
	}
	paths, err := config.ResolveAgentPaths(homeName, s.paths)
	if err != nil {
		slog.Warn("prepare: resolve agent paths failed", "agent_id", agentID, "error", err)
		return
	}
	resolved := capability.Resolve(&snap.Capabilities)
	if err := skillstore.Materialize(paths.SkillsDir, resolved.Skills); err != nil {
		slog.Warn("prepare: materialize skills failed", "agent_id", agentID, "error", err)
	}
}

// reprepare re-materializes skills for every wanted agent after a capability
// change (skill, attachment, or agent-type edits).
func (s *Supervisor) reprepare() {
	agents, err := s.agents.ListAgents(s.userID)
	if err != nil {
		slog.Warn("reprepare: list agents failed", "error", err)
		return
	}
	for _, a := range agents {
		switch a.Status {
		case "deleted", "suspended", "failed":
			continue
		}
		s.prepare(a.ID)
	}
}

// capabilitiesChanged reports whether a config-change notification concerns the
// capability catalog (the bus emits keys ["capabilities", "capabilities.<kind>"]).
func capabilitiesChanged(keys []string) bool {
	for _, k := range keys {
		if k == "capabilities" || strings.HasPrefix(k, "capabilities.") {
			return true
		}
	}
	return false
}

// Run reconciles the agent set against the bus and reacts to agent/config
// changes until ctx is cancelled.
func (s *Supervisor) Run(ctx context.Context) error {
	s.reconcile()

	agentChanges := s.bus.AgentChanges()
	configChanges := s.bus.ConfigChanges()

	for {
		select {
		case <-ctx.Done():
			s.stopAll()
			return nil
		case change, ok := <-agentChanges:
			if !ok {
				return nil
			}
			if change.UserID != "" && change.UserID != s.userID {
				continue
			}
			slog.Info("agent change received", "action", change.Action, "agent_id", change.AgentID)
			switch change.Action {
			case "restart":
				// Explicit operator restart: respawn with a fresh config read.
				s.restart(change.AgentID)
			case "create", "delete", "restore":
				s.reconcile()
			default:
				// "update" and anything else: the running agent hot-reloads its
				// own record/config between turns. Do NOT kill the process —
				// that would tear down MCP connections and in-flight context.
			}
		case change, ok := <-configChanges:
			if !ok {
				continue
			}
			// Config items are hot-applied in-process (reconciler). Supervisors
			// must not restart agents on config_changed; if hot-apply truly
			// fails the agent process exits and supervise() respawns it.
			//
			// Capability changes are the exception: skill files are owned by the
			// hypervisor, so a skill/attachment/type edit must be re-materialized
			// on disk for the running agents to pick up.
			if capabilitiesChanged(change.Keys) {
				s.reprepare()
			}
		}
	}
}

// reconcile ensures a child process exists for every non-deleted/non-suspended/
// non-failed agent and terminates children for agents that are gone.
func (s *Supervisor) reconcile() {
	agents, err := s.agents.ListAgents(s.userID)
	if err != nil {
		slog.Error("list agents", "error", err)
		return
	}

	wanted := make(map[string]bool, len(agents))
	for _, a := range agents {
		if a.Status == "deleted" || a.Status == "suspended" || a.Status == "failed" {
			continue
		}
		wanted[a.ID] = true
		if s.get(a.ID) == nil {
			s.prepare(a.ID)
			s.spawn(a.ID)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for id, c := range s.children {
		if !wanted[id] {
			slog.Info("stopping agent process", "agent_id", id)
			_ = c.cmd.Process.Kill()
			delete(s.children, id)
			_ = s.agents.SetAgentStatus(id, "stopped")
		}
	}
}

func (s *Supervisor) get(agentID string) *child {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.children[agentID]
}

// restart kills the agent process so supervise() respawns it with a fresh read
// of the agent record + config. Resets the restart counter so a deliberate
// restart is not counted toward the crash limit.
func (s *Supervisor) restart(agentID string) {
	s.mu.Lock()
	c := s.children[agentID]
	if c != nil {
		c.restarts.Store(0)
	}
	s.mu.Unlock()
	if c == nil {
		s.reconcile()
		return
	}
	slog.Info("restarting agent", "agent_id", agentID)
	_ = c.cmd.Process.Kill()
}

func (s *Supervisor) restartAll() {
	s.mu.Lock()
	ids := make([]string, 0, len(s.children))
	for id := range s.children {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.restart(id)
	}
}

// spawn starts a child process for an agent and supervises it.
func (s *Supervisor) spawn(agentID string) {
	exe, err := os.Executable()
	if err != nil {
		slog.Error("resolve executable", "error", err)
		_ = s.agents.SetAgentStatus(agentID, "failed")
		return
	}

	_ = s.agents.SetAgentStatus(agentID, "spawning")

	readyR, readyW, err := os.Pipe()
	if err != nil {
		slog.Error("create ready pipe", "error", err)
		_ = s.agents.SetAgentStatus(agentID, "failed")
		return
	}

	args := []string{"agent", "--user", s.userID, "--agent", agentID, "--bus-url", s.busURL, "--ready-fd", fmt.Sprintf("%d", readyFD)}
	if s.bindingID > 0 {
		args = append(args, "--binding", fmt.Sprintf("%d", s.bindingID))
	}

	cmd := exec.Command(exe, args...)
	cmd.ExtraFiles = []*os.File{readyW}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		slog.Error("start agent process", "agent_id", agentID, "error", err)
		readyR.Close()
		readyW.Close()
		_ = s.agents.SetAgentStatus(agentID, "failed")
		return
	}
	readyW.Close() // parent keeps only the read end

	c := &child{agentID: agentID, cmd: cmd, ready: readyR}
	s.mu.Lock()
	s.children[agentID] = c
	s.mu.Unlock()

	slog.Info("agent process spawned", "agent_id", agentID, "pid", cmd.Process.Pid)
	go s.supervise(c)
}

// supervise waits for readiness and exit, reporting status and restarting
// crashed agents with a backoff up to maxRestarts.
func (s *Supervisor) supervise(c *child) {
	go func() {
		buf := make([]byte, 64)
		n, _ := c.ready.Read(buf)
		if n > 0 {
			slog.Info("agent ready", "agent_id", c.agentID)
			_ = s.agents.SetAgentStatus(c.agentID, "running")
		}
	}()

	err := c.cmd.Wait()
	c.ready.Close()

	s.mu.Lock()
	current, ok := s.children[c.agentID]
	if ok && current == c {
		delete(s.children, c.agentID)
	}
	s.mu.Unlock()

	if err == nil {
		slog.Info("agent process exited cleanly", "agent_id", c.agentID)
		_ = s.bus.UpdateAgentStatus(s.userID, c.agentID, "stopped")
		return
	}

	slog.Warn("agent process exited", "agent_id", c.agentID, "error", err)

	if c.restarts.Load() >= maxRestarts {
		slog.Error("agent failed after restart attempts", "agent_id", c.agentID, "restarts", c.restarts.Load())
		_ = s.bus.UpdateAgentStatus(s.userID, c.agentID, "failed")
		return
	}

	delay := time.Duration(1<<uint(c.restarts.Load())) * time.Second
	c.restarts.Add(1)
	slog.Info("restarting agent", "agent_id", c.agentID, "attempt", c.restarts.Load(), "delay", delay)
	time.Sleep(delay)

	// Only respawn if the agent is still wanted and not already replaced. A
	// deleted agent must never be resurrected by a crash-backoff respawn.
	if s.get(c.agentID) == nil && s.stillWanted(c.agentID) {
		s.prepare(c.agentID)
		s.spawn(c.agentID)
	}
}

// stillWanted reports whether the agent still exists and is eligible to run
// (not deleted/suspended/failed). On a bus error it is permissive so a
// transient outage cannot permanently stop a wanted agent.
func (s *Supervisor) stillWanted(agentID string) bool {
	agents, err := s.agents.ListAgents(s.userID)
	if err != nil {
		return true
	}
	for _, a := range agents {
		if a.ID != agentID {
			continue
		}
		switch a.Status {
		case "deleted", "suspended", "failed":
			return false
		}
		return true
	}
	return false
}

func (s *Supervisor) stopAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, c := range s.children {
		_ = c.cmd.Process.Kill()
		_ = s.agents.SetAgentStatus(id, "stopped")
	}
	s.children = make(map[string]*child)
}
