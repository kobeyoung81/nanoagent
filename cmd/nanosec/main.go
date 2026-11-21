// Package main provides the nanosec CLI binary (agent service).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanosec/internal/agent"
	"gitee.com/kobeyoung81/nanosec/internal/capability"
	"gitee.com/kobeyoung81/nanosec/internal/config"
	"gitee.com/kobeyoung81/nanosec/internal/cron"
	"gitee.com/kobeyoung81/nanosec/internal/gitsvc"
	"gitee.com/kobeyoung81/nanosec/internal/hypervisor"
	nanoseclog "gitee.com/kobeyoung81/nanosec/internal/log"
	"gitee.com/kobeyoung81/nanosec/internal/portalclient"
	"gitee.com/kobeyoung81/nanosec/internal/provider"
	"gitee.com/kobeyoung81/nanosec/internal/question"
	"gitee.com/kobeyoung81/nanosec/internal/skill"
	"gitee.com/kobeyoung81/nanosec/internal/tool"
	"gitee.com/kobeyoung81/nanosec/internal/workspace"
	"github.com/spf13/cobra"
)

// busWS decides whether agent processes run their requests on one WebSocket
// with the bus instead of one HTTP POST per poll. The socket is an optimisation:
// when it is off or down, the client falls back to the poll path unchanged.
var busWS = true

// Build-time variables set via -ldflags.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	setupLogger()

	rootCmd := &cobra.Command{
		Use:          "nanosec",
		Short:        "Nanosec - AI agent service",
		Long:         `Nanosec runs per-user agent containers. The hypervisor subcommand supervises per-agent processes; the agent subcommand runs a single agent.`,
		SilenceUsage: true,
	}
	rootCmd.PersistentFlags().BoolVar(&busWS, "bus-ws", true,
		"hold one WebSocket to the bus for requests and notifications (false: poll every 500ms)")
	rootCmd.AddCommand(newHypervisorCmd())
	rootCmd.AddCommand(newAgentCmd())
	rootCmd.AddCommand(newHomeCmd())
	rootCmd.AddCommand(newVersionCmd())

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

// newHypervisorCmd supervises per-agent processes inside a user container and
// exposes the external HTTP bridge (workspace browser, health, tunnels).
func newHypervisorCmd() *cobra.Command {
	var userID string
	var bindingID int64
	var busURL string
	var tunnelURL string
	var addr string

	cmd := &cobra.Command{
		Use:   "hypervisor",
		Short: "Run the container supervisor and external bridge",
		RunE: func(cmd *cobra.Command, args []string) error {
			if userID == "" {
				return fmt.Errorf("user ID is required (use --user flag)")
			}

			resolvedBusURL := config.ResolveBusURL(busURL)
			resolvedTunnelURL := config.ResolveTunnelURL(tunnelURL, resolvedBusURL)
			cfg, err := config.LoadAgentConfig(config.AdminURL())
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			paths, err := cfg.Paths.Resolve()
			if err != nil {
				return fmt.Errorf("resolve paths: %w", err)
			}

			client, err := connectBus(resolvedBusURL, nanoprotocol.AgentSocketRegister{
				UserID:     userID,
				Components: []string{nanoprotocol.SocketComponentAgents, nanoprotocol.SocketComponentConfig},
			})
			if err != nil {
				return fmt.Errorf("connect to bus: %w", err)
			}
			defer client.Close()

			// The agent rows and the conversation projection are the portal's;
			// the bus carries messages only.
			agents := portalclient.New(config.PortalURL(), config.InternalToken())

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
			go func() {
				sig := <-sigCh
				slog.Info("shutting down", "signal", sig)
				cancel()
			}()

			if addr != "" {
				hypervisor.Version = version
				hy := hypervisor.NewServer(addr).SetDataRoot(paths.DataRoot)
				terminals := hypervisor.NewTerminalManager(paths.DataRoot)
				homeResolver := newHomeResolver(agents, userID, paths.DataRoot)
				hy.SetHomeResolver(homeResolver)
				hy.SetGitService(gitsvc.New(paths.DataRoot, 55*time.Second))
				terminals.SetHomeResolver(homeResolver)
				hy.SetTerminalManager(terminals)
				go func() {
					if err := hy.Start(ctx); err != nil && err != context.Canceled {
						slog.Error("hypervisor bridge", "error", err)
						cancel()
					}
				}()

				// Register a WebSocket tunnel so the portal can reach this
				// container's HTTP bridge from any network topology. It dials the
				// provisioner where one is configured; the protocol is unchanged.
				tun := hypervisor.NewTunnel(userID, resolvedTunnelURL, addr, terminals)
				go tun.Run(ctx)
			}

			sup := hypervisor.New(userID, bindingID, resolvedBusURL, client, agents)
			sup.SetPaths(cfg.Paths, paths.DataRoot)
			return sup.Run(ctx)
		},
	}

	cmd.Flags().StringVar(&userID, "user", "", "User ID to supervise (required)")
	cmd.Flags().Int64Var(&bindingID, "binding", 0, "Binding ID")
	cmd.Flags().StringVar(&busURL, "bus-url", "", "bus URL")
	cmd.Flags().StringVar(&tunnelURL, "tunnel-url", "", "tunnel URL (default: $NANOSEC_AGENT_TUNNEL_URL, else the bus URL)")
	cmd.Flags().StringVar(&addr, "hypervisor-addr", ":9090", "external bridge listen address (empty disables)")
	_ = cmd.MarkFlagRequired("user")

	return cmd
}

// newHomeResolver builds the agentID -> home folder name resolver used by the
// hypervisor bridge and terminal manager (they only see agent IDs, not agent
// records). The registry is the authority; the display name used to derive a
// folder for an unregistered agent is fetched through the bus and cached.
func newHomeResolver(agents *portalclient.Client, userID, dataRoot string) func(string) string {
	index := workspace.NewHomesIndex(dataRoot)
	var mu sync.Mutex
	names := map[string]string{} // agentID -> displayName
	return func(agentID string) string {
		mu.Lock()
		displayName := names[agentID]
		mu.Unlock()
		if displayName == "" {
			if recs, err := agents.ListAgents(userID); err == nil {
				mu.Lock()
				for _, r := range recs {
					names[r.ID] = r.DisplayName
				}
				displayName = names[agentID]
				mu.Unlock()
			}
		}
		return index.Lookup(agentID, displayName)
	}
}

// newAgentCmd runs a single agent process, polling only its own inbound queue.
func newAgentCmd() *cobra.Command {
	var userID string
	var bindingID int64
	var agentID string
	var busURL string
	var readyFD int

	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Run a single agent",
		RunE: func(cmd *cobra.Command, args []string) error {
			if userID == "" {
				return fmt.Errorf("user ID is required (use --user flag)")
			}
			if agentID == "" {
				return fmt.Errorf("agent ID is required (use --agent flag)")
			}
			return runAgent(userID, bindingID, agentID, busURL, readyFD)
		},
	}

	cmd.Flags().StringVar(&userID, "user", "", "User ID (required)")
	cmd.Flags().Int64Var(&bindingID, "binding", 0, "Binding ID")
	cmd.Flags().StringVar(&agentID, "agent", "", "Agent ID (required)")
	cmd.Flags().StringVar(&busURL, "bus-url", "", "bus URL")
	cmd.Flags().IntVar(&readyFD, "ready-fd", -1, "write READY to this fd once initialized")
	_ = cmd.MarkFlagRequired("user")
	_ = cmd.MarkFlagRequired("agent")

	return cmd
}

// connectBus opens the bus connection: a registered agent socket when that is
// enabled, the plain HTTP client otherwise. A socket that cannot be opened or
// registered is not an error — the caller gets the client it would have had.
func connectBus(busURL string, reg nanoprotocol.AgentSocketRegister) (*nanoprotocol.Client, error) {
	token := config.InternalToken()
	if !busWS {
		return nanoprotocol.NewClient(busURL, nanoprotocol.WithToken(token))
	}
	client, err := nanoprotocol.DialWS(busURL, nanoprotocol.WithToken(token))
	if err != nil {
		slog.Warn("agent socket unavailable; polling the bus", "url", busURL, "error", err)
		return nanoprotocol.NewClient(busURL, nanoprotocol.WithToken(token))
	}
	if err := client.RegisterAgentSocket(reg); err != nil {
		slog.Warn("agent socket refused; polling the bus", "url", busURL, "error", err)
		_ = client.Close()
		return nanoprotocol.NewClient(busURL, nanoprotocol.WithToken(token))
	}
	slog.Info("agent socket connected", "url", busURL, "agent_id", reg.AgentID)
	return client, nil
}

// runAgent builds and runs a single agent, polling its own inbound queue.
func runAgent(userID string, bindingID int64, agentID, busURL string, readyFD int) error {
	resolvedBusURL := config.ResolveBusURL(busURL)
	cfg, err := config.LoadAgentConfig(config.AdminURL())
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	rootPaths, err := cfg.Paths.Resolve()
	if err != nil {
		return fmt.Errorf("resolve root paths: %w", err)
	}

	client, err := connectBus(resolvedBusURL, nanoprotocol.AgentSocketRegister{
		UserID:     userID,
		AgentID:    agentID,
		Components: []string{nanoprotocol.SocketComponentConfig},
	})
	if err != nil {
		return fmt.Errorf("connect to bus: %w", err)
	}
	defer client.Close()

	// Session snapshots go to the portal, which owns the conversation projection.
	sessions := portalclient.New(config.PortalURL(), config.InternalToken())

	// Fetch the full reconciler input in one round trip: the agent record
	// (catalog model reference, thinking, MCP toggle, model roles, mode),
	// config items, and the resolved capabilities (which include the visible
	// model catalog with provider credentials).
	snap, err := client.GetAgentSnapshot(agentID)
	if err != nil {
		return fmt.Errorf("get agent snapshot: %w", err)
	}
	record := snap.Agent

	// Effective per-agent config and filesystem layout: identity, bootstrap,
	// memory, skills, sessions, cron, media live under agents/<homeName>/agent
	// and working files under agents/<homeName>/workspace (see ResolveAgentPaths).
	merged := config.BuildAgentConfigFor(cfg, snap.Config, record.ID)

	// ---- model catalog resolution ----
	// Select the agent's model from the catalog (platform + owner models),
	// overriding the flat provider config with the selected model's provider
	// connection and per-model parameters.
	defaultModelID, defaultThinking := defaultModeModel(snap)
	applyModelCatalog(&snap.Capabilities, defaultModelID, record.Model, defaultThinking, merged.Provider.DefaultModel, &merged)

	// Say what is actually missing. Without a model the next check only sees an
	// empty API key and reports "provider API key is required", which sends an
	// administrator looking at the wrong setting: on a fresh installation the
	// model catalog exists but no agent type has been given a model yet.
	if merged.Provider.DefaultModel == "" {
		typeID := record.TypeID
		if typeID == "" {
			typeID = "unknown"
		}
		return fmt.Errorf("no model is assigned to agent type %q: assign one in the admin console (Models → apply to built-in agent types)", typeID)
	}

	// Validate after catalog resolution: the provider connection (and API key)
	// now comes from the selected catalog model, not the flat config items.
	if err := config.ValidateAgentConfig(merged); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}

	// The LLM client now carries the resolved provider connection/model.
	llm := provider.NewOpenAI(
		merged.Provider.APIKey,
		merged.Provider.BaseURL,
		merged.Provider.DefaultModel,
		merged.Agent.LLMRequestTimeout,
	)

	// Assign/seed the agent home first: the folder name is derived from the
	// display name (frozen at creation) and recorded in agents/.homes.json, so
	// ResolveAgentPaths and the workspace manager must use the returned name.
	homeName, err := agent.EnsureAgentHome(rootPaths.DataRoot, record.ID, record.DisplayName)
	if err != nil {
		return fmt.Errorf("ensure agent home: %w", err)
	}

	// Model roles: resolved main model; platform role AgentTypes override.
	model := merged.Provider.DefaultModel
	maxIter := record.MaxIterations
	if maxIter <= 0 {
		maxIter = merged.Agent.MaxIterations
	}
	titleModel := model
	summarizeModel := model
	subModel := model

	// Platform role AgentTypes override the built-in role prompts/models, and
	// the unified registry provides every definition (session/subagent/utility).
	rolePrompts, roleModels, roleMax := resolveRoleConfig(snap)
	// The platform-wide default for subagent calls (the service key). It is the
	// last layer before "send nothing"; subagents have no user-level setting.
	subagentEffort := normalizeThinkingLevel(merged.Agent.LLMSubagentReasoningEffort)
	agentsMap, defaultAgent, specPrompts, specModels := resolveAgentRegistry(snap, llm, subagentEffort)
	for k, v := range specPrompts {
		rolePrompts[k] = v
	}
	for k, v := range specModels {
		roleModels[k] = v
	}
	if m := roleModels["title"]; m != "" {
		titleModel = m
	}
	if m := roleModels["summarize"]; m != "" {
		summarizeModel = m
	}
	if m := roleModels["subagent"]; m != "" {
		subModel = m
	} else if m := roleModels["general"]; m != "" {
		subModel = m
	}

	paths, err := config.ResolveAgentPaths(homeName, merged.Paths)
	if err != nil {
		return fmt.Errorf("resolve agent paths: %w", err)
	}
	// Resolve "~" into the persistent, writable agent home so tool caches and
	// config land there instead of the (ephemeral, root-owned) /root.
	_ = os.Setenv("HOME", paths.AgentRoot(homeName))

	shellTimeout := time.Duration(merged.Tools.Shell.Timeout)

	// A scheduled task runs unattended, so its mode is resolved from the task
	// itself, then the agent's own setting, then the platform default — never
	// from the conversation-oriented default agent (an agent whose default is
	// plan would otherwise be unable to run a task at all).
	cronSvc := cron.New(paths.CronFile, cron.Injector(client, userID, record.ID, func(job *cron.Job) string {
		return cron.TaskMode(job, record.Settings["scheduled_task_mode"], merged.Agent.ScheduledTaskMode)
	}))

	subCfg := agent.SubagentConfig{
		Model:              subModel,
		MaxIterations:      merged.Agent.MaxIterations,
		Workspace:          paths.AgentRoot(homeName),
		ShellTimeout:       shellTimeout,
		AgentID:            record.ID,
		MaxToolResultChars: merged.Agent.MaxToolResultChars,
		ReasoningEffort:    subagentEffort,
		RequestTimeout:     merged.Agent.LLMRequestTimeout,
	}
	// Subagent profiles are resolved before the tool registry is built: the
	// read-only delegation tool (readonly_task) may only start subagent types
	// that cannot modify anything (no write/edit/full shell, no MCP servers).
	subProfiles := agent.SubagentProfilesFrom(agentsMap)
	readOnlySubagentIDs := agent.ReadOnlyProfileIDs(subProfiles)

	// subMgr is created after agentHome/sharedWS are resolved (it builds
	// subagent tool registries via buildTools); the closure below only runs
	// later, once it is assigned.
	var subMgr *agent.SubagentManager

	// Skills are materialized into paths.SkillsDir by the hypervisor from the
	// platform catalog; the agent process only reads them.
	skillLoader := skill.NewLoader(paths.SkillsDir)
	skillReg := skill.NewRegistry(skillLoader)

	// Process-wide broker for the blocking question tool. Shared between the
	// poll loop (which routes answers) and the tool (which waits).
	qBroker := question.NewBroker()

	// The read tool's window is derived from the tool-result budget (3/4 of it)
	// so that its paging footer — the line naming the next offset — survives the
	// global truncation, which keeps the head of a result. 0 means "tool
	// default".
	readWindowBytes := 0
	if n := merged.Agent.MaxToolResultChars; n > 0 {
		readWindowBytes = n * 3 / 4
	}

	// buildToolsWith builds a tool registry with optional per-profile overrides
	// (shell timeout, workspace restriction). nil/zero means inherit the agent's
	// effective settings.
	buildToolsWith := func(fsWorkspace, shared string, enabled map[string]bool, shellTimeout time.Duration, restrict *bool) *tool.Registry {
		ws, sh := fsWorkspace, shared
		restrictToWS := merged.Tools.RestrictToWorkspace
		if restrict != nil {
			restrictToWS = *restrict
		}
		if !restrictToWS {
			ws, sh = "", ""
		}
		// Read the shell timeout at call time so a capability rebuild picks up
		// live changes to the agent's effective settings.
		shTimeout := time.Duration(merged.Tools.Shell.Timeout)
		if shellTimeout > 0 {
			shTimeout = shellTimeout
		}
		registry := tool.NewRegistry()

		keep := func(name string) bool {
			return enabled == nil || enabled[name]
		}

		if keep("bash") {
			shellTool := tool.NewShellTool(shTimeout, ws)
			if sh != "" {
				shellTool = shellTool.WithSharedWorkspace(sh)
			}
			registry.Register(shellTool)
		}
		if keep("readonly_bash") {
			registry.Register(tool.NewReadOnlyShellTool(shTimeout, ws))
		}
		if keep("read") {
			registry.Register(&tool.ReadFileTool{Workspace: ws, SharedWorkspace: sh, MaxBytes: readWindowBytes})
		}
		if keep("write") {
			registry.Register(&tool.WriteFileTool{Workspace: ws, SharedWorkspace: sh})
		}
		if keep("edit") {
			registry.Register(&tool.EditFileTool{Workspace: ws, SharedWorkspace: sh})
		}
		if keep("list") {
			registry.Register(&tool.ListDirTool{Workspace: ws, SharedWorkspace: sh})
		}
		if keep("append_note") {
			registry.Register(&tool.AppendNoteTool{MemoryDir: paths.MemoryDir})
		}
		if keep("webfetch") {
			registry.Register(&tool.WebFetchTool{})
		}
		if keep("grep") {
			registry.Register(tool.NewGrepTool(ws, sh))
		}
		if keep("glob") {
			registry.Register(tool.NewGlobTool(ws, sh))
		}
		if keep("load_skill") {
			registry.Register(tool.NewSkillTool(skillReg))
		}
		if keep("manage_skill") {
			registry.Register(tool.NewManageSkillTool(client, userID, record.ID, ws, sh, paths.SkillsDir))
		}
		if keep("todowrite") {
			registry.Register(tool.NewTodoWriteTool())
		}
		if keep("plan_write") {
			registry.Register(tool.NewPlanWriteTool(ws, sh))
		}
		if keep("plan_execute") {
			registry.Register(tool.NewPlanExecuteTool(ws, sh))
		}
		if keep("question") {
			registry.Register(tool.NewQuestionTool(qBroker))
		}
		// The report that ends a long-running goal's round. Registered like any
		// other tool so the model sees one surface; it refuses outside a goal
		// round, which is every turn until the scheduler ships.
		if keep("goal_report") {
			registry.Register(tool.NewGoalReportTool())
		}
		if keep("download_media") && cfg.Media.CorpID != "" && cfg.Media.CorpSecret != "" {
			registry.Register(tool.NewMediaDownloadTool(
				paths.MediaBaseDir,
				cfg.Media.CorpID,
				cfg.Media.CorpSecret,
				cfg.Media.AgentID,
			))
		}
		if keep("upload_media") && cfg.Media.CorpID != "" && cfg.Media.CorpSecret != "" {
			registry.Register(tool.NewMediaUploadTool(
				paths.MediaBaseDir,
				fsWorkspace,
				cfg.Media.CorpID,
				cfg.Media.CorpSecret,
				cfg.Media.AgentID,
				client,
				userID,
			))
		}
		if keep("cron") {
			registry.Register(tool.NewCronTool(cronSvc))
		}
		if keep("task") && subMgr != nil {
			registry.Register(tool.NewTaskTool(subMgr))
		}
		if keep("readonly_task") && subMgr != nil && len(readOnlySubagentIDs) > 0 {
			registry.Register(tool.NewReadOnlyTaskTool(subMgr, readOnlySubagentIDs))
		}
		// The single concurrency entry point: several calls written in one
		// assistant message run one after another, so a caller that wants them at
		// the same time asks here. The loop injects its dispatch per turn.
		if keep("parallel_execution") {
			registry.Register(tool.NewParallelTool())
		}

		return registry
	}

	// buildTools builds a registry with the agent's own effective settings (no
	// per-profile overrides) for session agents and capability probes.
	buildTools := func(fsWorkspace, shared string, enabled map[string]bool) *tool.Registry {
		return buildToolsWith(fsWorkspace, shared, enabled, 0, nil)
	}

	newContextBuilder := func(fsWorkspace string) *agent.ContextBuilder {
		return agent.NewContextBuilder(
			fsWorkspace,
			paths.IdentityFile,
			paths.BootstrapDir,
			paths.MemoryDir,
			skillReg,
			merged.Agent.MemoryRecentDays,
		)
	}

	wsMgr := workspace.NewNamed(record.ID, homeName, paths.DataRoot)
	if err := wsMgr.EnsureDirs(); err != nil {
		return fmt.Errorf("create agent workspace: %w", err)
	}
	agentHome := wsMgr.AgentWorkspace()
	sharedWS := wsMgr.SharedWorkspace()

	// mcpToolsByServer holds the connected MCP tool adapters grouped by server
	// id, refreshed on every capability rebuild (idle-deferred, so reads by the
	// subagent factory never race a write).
	var mcpToolsByServer map[string][]tool.Tool

	// Subagent runner: each subagent is a fresh "newborn agent" built from a
	// type profile (own prompt, tool permissions, lesser turn limit).
	subCfg.Workspace = agentHome
	for role, p := range subProfiles {
		if pr := rolePrompts[role]; pr != "" {
			p.Prompt = pr
		}
		if mr := roleMax[role]; mr > 0 {
			p.MaxIterations = mr
		}
		subProfiles[role] = p
	}
	subMgr = agent.NewSubagentManager(
		subCfg,
		subProfiles,
		func(p agent.SubagentProfile, policy tool.CallerPolicy) *tool.Registry {
			// The caller's own tool set bounds what the subagent may use: a
			// delegation never hands out a capability the caller lacks.
			enabled := tool.AttenuateEnabled(p.Tools, policy.AllowedTools)
			reg := buildToolsWith(agentHome, sharedWS, enabled, p.ShellTimeout, p.RestrictToWorkspace)
			// A subagent never delegates again: delegation depth stays 1, so a
			// runaway fan-out cannot hide behind nested subagents. The registry is
			// the enforcement point — what is not registered cannot be called, and
			// a capability row edited back on later cannot re-open it.
			reg.Unregister("task")
			reg.Unregister("readonly_task")
			for srv := range p.McpServers {
				for _, t := range mcpToolsByServer[srv] {
					if policy.AllowedTools == nil || policy.AllowedTools[t.Name()] {
						reg.Register(t)
					}
				}
			}
			return reg
		},
		llm,
		client,
		userID,
	)

	agentCfg := agent.Config{
		UserID:                      userID,
		BindingID:                   bindingID,
		AgentID:                     record.ID,
		BusURL:                      resolvedBusURL,
		Model:                       model,
		TitleModel:                  titleModel,
		SummarizeModel:              summarizeModel,
		RolePrompts:                 rolePrompts,
		Language:                    agent.EffectiveLanguage(record.Settings["language"], merged.Agent.LanguageDefault),
		Agents:                      agentsMap,
		DefaultAgent:                defaultAgent,
		SessionAgentIDs:             snap.Agent.SessionAgents,
		MaxIterations:               maxIter,
		SessionDir:                  paths.SessionDir,
		WorkspaceDir:                agentHome,
		SharedWorkspaceDir:          sharedWS,
		WorkspaceURL:                config.WorkspaceURL(),
		ContextWindow:               merged.Agent.ContextWindow,
		MaxOutputTokens:             merged.Agent.MaxOutputTokens,
		ThinkingEffort:              merged.Agent.ThinkingEffort,
		Temperature:                 merged.Agent.Temperature,
		TopP:                        merged.Agent.TopP,
		MaxToolResultChars:          merged.Agent.MaxToolResultChars,
		SummarizeTokenPercent:       merged.Agent.SummarizeTokenPercent,
		SummarizeMessageThreshold:   merged.Agent.SummarizeMessageThreshold,
		TitleRefreshMessages:        merged.Agent.TitleRefreshMessages,
		MemoryRecentDays:            merged.Agent.MemoryRecentDays,
		SessionSyncEnabled:          merged.Agent.SessionSyncEnabled,
		SessionSyncFlushInterval:    merged.Agent.SessionSyncFlushInterval,
		LLMTtfbTimeout:              merged.Agent.LLMTtfbTimeout,
		LLMStreamIdleTimeout:        merged.Agent.LLMStreamIdleTimeout,
		LLMRequestTimeout:           merged.Agent.LLMRequestTimeout,
		LLMMaxRetries:               merged.Agent.LLMMaxRetries,
		LLMSummarizeReasoningEffort: merged.Agent.LLMSummarizeReasoningEffort,
		ChannelCaps:                 merged.Channels,
	}

	ag, err := agent.New(agentCfg, client, llm, buildTools(agentHome, sharedWS, nil), agent.WithSessionSink(sessions))
	if err != nil {
		return fmt.Errorf("create agent %s: %w", record.ID, err)
	}
	ag.SetQuestionBroker(qBroker)

	ctxBuilder := newContextBuilder(agentHome)
	ctxBuilder.SetChannelCapabilities(merged.Channels)
	ag.SetContextBuilder(ctxBuilder)
	ag.SetBaseConfig(cfg)

	// ===== capability catalog wiring =====

	// Self-heal the bus catalog with this binary's builtin tool definitions
	// (descriptions + parameter schemas); admin state is preserved bus-side.
	probe := buildTools(agentHome, sharedWS, nil)
	defs := make([]nanoprotocol.BuiltinToolDef, 0, len(probe.Names()))
	for _, d := range probe.Definitions() {
		raw, _ := json.Marshal(d.Parameters)
		defs = append(defs, nanoprotocol.BuiltinToolDef{
			ID:             d.Name,
			Description:    d.Description,
			Parameters:     raw,
			DefaultEnabled: builtinDefaultEnabled(d.Name),
		})
	}
	if len(defs) > 0 {
		if err := client.SyncBuiltinTools(record.ID, defs); err != nil {
			slog.Warn("sync builtin tools failed", "agent", record.ID, "error", err)
		}
	}

	// MCP bridge owns stdio/http server connections for this agent.
	mcpBridge := agent.NewMcpBridge(record.ID, agentHome, sharedWS)

	// rebuildCapabilities fetches a consolidated snapshot (agent record +
	// config + capability catalog + model catalog) in one round trip and
	// atomically applies it: model/provider swap, settings, registry swap + MCP
	// reconcile + skill materialization. Never invoked mid-turn (idle-deferred).
	rebuildCapabilities := func(ctx context.Context) error {
		snap, err := client.GetAgentSnapshot(record.ID)
		if err != nil {
			return fmt.Errorf("fetch agent snapshot: %w", err)
		}
		// Refresh config + model/provider + per-agent settings (thinking).
		merged = config.BuildAgentConfigFor(cfg, snap.Config, record.ID)
		modeModelID, modeThinking := defaultModeModel(snap)
		applyModelCatalog(&snap.Capabilities, modeModelID, snap.Agent.Model, modeThinking, merged.Provider.DefaultModel, &merged)
		newProvider := provider.NewOpenAI(merged.Provider.APIKey, merged.Provider.BaseURL, merged.Provider.DefaultModel, merged.Agent.LLMRequestTimeout)
		ag.SetProvider(newProvider)
		ag.ApplySettings(merged)
		// Permission baseline (type) + per-agent overrides, resolved by the bus.
		ag.SetPermissions(snap.Capabilities.Permissions)
		// channel_fallbacks are carried per resolved definition, so no
		// agent-level install is needed here.

		// Refresh the unified definition registry (session/subagent/utility) and
		// derive subagent profiles + role prompts from it.
		regMap, defID, specPrompts, _ := resolveAgentRegistry(snap, newProvider,
			normalizeThinkingLevel(merged.Agent.LLMSubagentReasoningEffort))
		agentsMap, defaultAgent = regMap, defID
		ag.SetAgents(agentsMap, snap.Agent.SessionAgents, defaultAgent)
		if subMgr != nil {
			subMgr.SetProfiles(agent.SubagentProfilesFrom(agentsMap))
		}
		if rolePrompts == nil {
			rolePrompts = map[string]string{}
		}
		for k, v := range specPrompts {
			rolePrompts[k] = v
		}
		ag.SetRolePrompts(rolePrompts)

		resolved := capability.Resolve(&snap.Capabilities)

		// MCP servers are resolved per definition (session agents + subagents)
		// under the same capability rule as builtin tools. Connect the union
		// once, then let each definition gate its own server set.
		union := map[string]bool{}
		for _, spec := range agentsMap {
			for srv := range spec.McpServers {
				union[srv] = true
			}
		}
		unionDefs := make([]nanoprotocol.McpServerDef, 0, len(union))
		for _, m := range snap.Capabilities.McpServers {
			if union[m.ID] {
				unionDefs = append(unionDefs, m)
			}
		}
		nextMcpTools := map[string][]tool.Tool{}
		for _, mcpTool := range mcpBridge.Sync(ctx, unionDefs) {
			if mt, ok := mcpTool.(*tool.McpTool); ok {
				nextMcpTools[mt.ServerID] = append(nextMcpTools[mt.ServerID], mcpTool)
			}
		}
		mcpToolsByServer = nextMcpTools

		// Register every builtin tool; per-definition enablement filters what
		// each definition may use (builtin allowlist + MCP server set).
		reg := buildTools(agentHome, sharedWS, nil)
		builtinNames := reg.Names()
		mcpServerByTool := map[string]string{}
		for srv, srvTools := range mcpToolsByServer {
			for _, t := range srvTools {
				mcpServerByTool[t.Name()] = srv
				reg.Register(t)
			}
		}
		ag.ApplyTools(reg.Snapshot())
		ag.SetToolProvenance(builtinNames, mcpServerByTool)

		// Surface per-server MCP connection state to the portal.
		if st, merr := json.Marshal(mcpBridge.Status()); merr == nil {
			ag.EmitStatus("mcp_status", string(st))
		}
		mcpToolCount := 0
		for _, tools := range mcpToolsByServer {
			mcpToolCount += len(tools)
		}
		slog.Info("capabilities applied",
			"agent", record.ID,
			"builtin_tools", len(resolved.BuiltinEnabled),
			"mcp_servers", len(union),
			"mcp_tools", mcpToolCount,
			"skills", len(resolved.Skills),
			"model", merged.Provider.DefaultModel,
		)
		return nil
	}
	ag.SetCapabilityRebuilder(rebuildCapabilities)

	// Apply initial capability state before the loop starts.
	{
		ictx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := rebuildCapabilities(ictx); err != nil {
			slog.Warn("initial capabilities apply failed (using defaults)", "agent", record.ID, "error", err)
		}
		cancel()
	}
	defer mcpBridge.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		slog.Info("shutting down", "signal", sig)
		cancel()
	}()

	if err := cronSvc.Start(ctx); err != nil {
		return fmt.Errorf("start cron service: %w", err)
	}
	defer cronSvc.Stop()

	signalReady(readyFD)
	slog.Info("agent process running", "user", userID, "agent", agentID, "home", agentHome)

	return ag.Run(ctx)
}

// signalReady writes "READY" to the given file descriptor so the supervisor can
// transition the agent to running. fd < 0 disables the signal.
func signalReady(fd int) {
	if fd < 0 {
		return
	}
	f := os.NewFile(uintptr(fd), "ready")
	if f == nil {
		return
	}
	_, _ = fmt.Fprintln(f, "READY")
	_ = f.Close()
}

// resolveRoleConfig extracts prompt and model overrides for internal role
// AgentTypes (title, summarize, subagent, and the subagent profiles). The role
// model is the type's locked/default allowed model resolved to its API id.
func resolveRoleConfig(snap *nanoprotocol.AgentSnapshotResponse) (prompts, models map[string]string, maxRounds map[string]int) {
	prompts = map[string]string{}
	models = map[string]string{}
	maxRounds = map[string]int{}
	if snap == nil {
		return
	}
	byID := map[int64]nanoprotocol.ModelDef{}
	for _, m := range snap.Capabilities.Models {
		byID[m.ID] = m
	}
	for _, rt := range snap.Agents {
		if rt.Role == "" {
			continue
		}
		if rt.Prompt != "" {
			prompts[rt.Role] = rt.Prompt
		}
		if rt.MaxRounds > 0 {
			maxRounds[rt.Role] = rt.MaxRounds
		}
		var defaultID int64
		for _, b := range rt.Models {
			if b.IsDefault {
				defaultID = b.ModelID
			}
		}
		if defaultID == 0 && len(rt.Models) > 0 {
			defaultID = rt.Models[0].ModelID
		}
		if md, ok := byID[defaultID]; ok && md.Model != "" {
			models[rt.Role] = md.Model
		}
	}
	return
}

func setupLogger() {
	level := slog.LevelInfo
	if env := os.Getenv("LOG_LEVEL"); env != "" {
		switch strings.ToUpper(env) {
		case "VERBOSE":
			level = nanoseclog.LevelVerbose
		case "DEBUG":
			level = slog.LevelDebug
		case "WARN":
			level = slog.LevelWarn
		case "ERROR":
			level = slog.LevelError
		}
	}

	opts := &slog.HandlerOptions{
		Level: level,
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, opts))
	slog.SetDefault(logger)
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("nanosec %s (commit: %s, built: %s)\n", version, commit, date)
			fmt.Printf("os: %s\n", hypervisor.Platform())
		},
	}
}
