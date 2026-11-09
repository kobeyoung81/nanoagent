package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"gitee.com/kobeyoung81/nanoprotocol"
)

// LoadAgentConfig fetches the "agent" and "gateway" config items from the admin
// module and reconstructs an AgentConfig from the flat key-value pairs (starting
// from defaults). Gateway items are read for `channels.<name>.<cap>` capability
// overrides.
//
// It retries briefly: the container is often started a moment before admin is
// reachable, and a hard failure there would leave the agent on defaults for its
// whole life. Running on defaults after ~30s is logged and reported, so a real
// outage is visible rather than silent.
func LoadAgentConfig(adminURL string) (AgentConfig, error) {
	cfg := DefaultAgentConfig()

	items, err := fetchWithRetry(adminURL, "agent")
	if err != nil {
		return AgentConfig{}, fmt.Errorf("get agent config items: %w", err)
	}
	applyAgentConfig(&cfg, configItemsToMap(items))

	gwItems, err := fetchWithRetry(adminURL, "gateway")
	if err != nil {
		slog.Warn("failed to load gateway config for channel capabilities", "error", err)
	} else {
		applyChannelCapabilities(&cfg, configItemsToMap(gwItems))
	}

	return cfg, nil
}

// fetchWithRetry retries 1s, 2s, 4s, 8s, 8s, 8s — about 31 seconds.
func fetchWithRetry(adminURL, component string) ([]nanoprotocol.ConfigItem, error) {
	delays := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second, 8 * time.Second}
	var lastErr error
	for attempt, delay := range append([]time.Duration{0}, delays...) {
		if delay > 0 {
			time.Sleep(delay)
		}
		items, err := fetchItemsFrom(adminURL, component, 5*time.Second)
		if err == nil {
			if attempt > 0 {
				slog.Info("admin config became reachable", "component", component, "attempts", attempt+1)
			}
			return items, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// applyChannelCapabilities merges `channels.<name>.<cap>` overrides into the
// default capability map.
func applyChannelCapabilities(cfg *AgentConfig, m map[string]string) {
	for name, def := range cfg.Channels {
		cap := def
		if v, ok := m["channels."+name+".streaming"]; ok && v != "" {
			cap.Streaming = v == "true"
		}
		if v, ok := m["channels."+name+".markdown"]; ok && v != "" {
			cap.Markdown = v == "true"
		}
		if v, ok := m["channels."+name+".richMedia"]; ok && v != "" {
			cap.RichMedia = v == "true"
		}
		if v, ok := m["channels."+name+".interactive"]; ok && v != "" {
			cap.Interactive = v == "true"
		}
		if v, ok := m["channels."+name+".asyncReplies"]; ok && v != "" {
			cap.AsyncReplies = v == "true"
		}
		if v, ok := m["channels."+name+".sessionResetSupported"]; ok && v != "" {
			cap.SessionResetSupported = v == "true"
		}
		if v, ok := m["channels."+name+".maxContentLen"]; ok && v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				cap.MaxContentLen = n
			}
		}
		cfg.Channels[name] = cap
	}
}

func configItemsToMap(items []nanoprotocol.ConfigItem) map[string]string {
	m := make(map[string]string, len(items))
	for _, item := range items {
		m[item.Key] = item.Value
	}
	return m
}

func applyAgentConfig(cfg *AgentConfig, m map[string]string) {
	if v := m["agent.maxIterations"]; v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Agent.MaxIterations = n
		}
	}
	if v := m["agent.maxToolResultChars"]; v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Agent.MaxToolResultChars = n
		}
	}
	if v := m["agent.summarizeTokenPercent"]; v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Agent.SummarizeTokenPercent = n
		}
	}
	if v := m["agent.summarizeMessageThreshold"]; v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Agent.SummarizeMessageThreshold = n
		}
	}
	if v := m["agent.titleRefreshMessages"]; v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Agent.TitleRefreshMessages = n
		}
	}
	if v := m["agent.scheduledTaskMode"]; v != "" {
		cfg.Agent.ScheduledTaskMode = v
	}
	if v := m["agent.language.default"]; v != "" {
		cfg.Agent.LanguageDefault = v
	}
	if v := m["agent.memoryRecentDays"]; v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Agent.MemoryRecentDays = n
		}
	}
	if v := m["agent.llmStreamIdleTimeout"]; v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Agent.LLMStreamIdleTimeout = d
		}
	}
	if v := m["agent.llmTtfbTimeout"]; v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Agent.LLMTtfbTimeout = d
		}
	}
	if v := m["agent.llmRequestTimeout"]; v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Agent.LLMRequestTimeout = d
		}
	}
	if v := m["agent.llmMaxRetries"]; v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Agent.LLMMaxRetries = n
		}
	}
	if v := m["agent.llmSummarizeReasoningEffort"]; v != "" {
		cfg.Agent.LLMSummarizeReasoningEffort = v
	}
	if v := m["agent.llmSubagentReasoningEffort"]; v != "" {
		cfg.Agent.LLMSubagentReasoningEffort = v
	}
	if v := m["agent.sessionSyncEnabled"]; v != "" {
		cfg.Agent.SessionSyncEnabled = v == "true"
	}
	if v := m["agent.sessionSyncFlushInterval"]; v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Agent.SessionSyncFlushInterval = d
		}
	}
	if v := m["tools.restrictToWorkspace"]; v != "" {
		cfg.Tools.RestrictToWorkspace = v == "true"
	}
	if v := m["tools.shell.timeout"]; v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Tools.Shell.Timeout = d
		}
	}
	if v := m["media.corpID"]; v != "" {
		cfg.Media.CorpID = v
	}
	if v := m["media.corpSecret"]; v != "" {
		cfg.Media.CorpSecret = v
	}
	if v := m["media.agentID"]; v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.Media.AgentID = n
		}
	}
	if v := m["paths.dataRoot"]; v != "" {
		cfg.Paths.DataRoot = v
	}
	if v := m["paths.sessionDir"]; v != "" {
		cfg.Paths.SessionDir = v
	}
	if v := m["paths.cronFile"]; v != "" {
		cfg.Paths.CronFile = v
	}
	if v := m["paths.mediaBaseDir"]; v != "" {
		cfg.Paths.MediaBaseDir = v
	}
	if v := m["paths.workspaceDir"]; v != "" {
		cfg.Paths.WorkspaceDir = v
	}
	if v := m["paths.identityFile"]; v != "" {
		cfg.Paths.IdentityFile = v
	}
	if v := m["paths.bootstrapDir"]; v != "" {
		cfg.Paths.BootstrapDir = v
	}
	if v := m["paths.memoryDir"]; v != "" {
		cfg.Paths.MemoryDir = v
	}
	if v := m["paths.skillsDir"]; v != "" {
		cfg.Paths.SkillsDir = v
	}
}

// ResolveAgentSettings merges config items for a specific agent using layering:
//
//	agent.default.<setting>  → base for all agents
//	agent.<setting>          → direct (legacy) override
//	agent.<id>.<setting>     → per-agent override (highest priority)
//
// The returned map uses the flat "agent.<setting>" key format consumed by
// applyAgentConfig.
func ResolveAgentSettings(items []nanoprotocol.ConfigItem, agentID string) map[string]string {
	merged := make(map[string]string)
	direct := make(map[string]string)
	for _, item := range items {
		switch {
		case strings.HasPrefix(item.Key, "agent.default."):
			merged["agent."+strings.TrimPrefix(item.Key, "agent.default.")] = item.Value
		case strings.HasPrefix(item.Key, "agent."+agentID+"."):
			direct["agent."+strings.TrimPrefix(item.Key, "agent."+agentID+".")] = item.Value
		default:
			direct[item.Key] = item.Value
		}
	}
	for k, v := range direct {
		merged[k] = v
	}
	return merged
}

// BuildAgentConfigFor returns the effective AgentConfig for a specific agent,
// layered over the given base config.
func BuildAgentConfigFor(base AgentConfig, items []nanoprotocol.ConfigItem, agentID string) AgentConfig {
	cfg := base
	applyAgentConfig(&cfg, ResolveAgentSettings(items, agentID))
	return cfg
}

// ResolveBusURL returns the bus URL from the flag value, the
// NANOSEC_AGENT_BUS_URL environment variable, or DefaultBusURL.
func ResolveBusURL(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if env := os.Getenv("NANOSEC_AGENT_BUS_URL"); env != "" {
		return env
	}
	return DefaultBusURL
}

// InternalToken returns the shared token the bus requires on its internal
// endpoints. It comes from the environment rather than the config items: the
// agent needs it before it can reach the bus at all, and every process of one
// platform holds the same value.
func InternalToken() string {
	return strings.TrimSpace(os.Getenv("NANOSEC_BUS_INTERNAL_TOKEN"))
}

// ValidateAgentConfig checks that required agent configuration fields are set.
func ValidateAgentConfig(cfg AgentConfig) error {
	if cfg.Provider.APIKey == "" {
		return fmt.Errorf("provider API key is required")
	}
	if cfg.Agent.MaxIterations <= 0 {
		return fmt.Errorf("max iterations must be positive")
	}
	return nil
}
