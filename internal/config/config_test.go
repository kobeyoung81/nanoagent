package config

import (
	"os"
	"testing"
	"time"
)

func TestDefaultAgentConfig(t *testing.T) {
	cfg := DefaultAgentConfig()

	if cfg.Provider.BaseURL != DefaultProviderBaseURL {
		t.Errorf("BaseURL = %q, want %q", cfg.Provider.BaseURL, DefaultProviderBaseURL)
	}
	if cfg.Provider.DefaultModel != DefaultProviderModel {
		t.Errorf("DefaultModel = %q, want %q", cfg.Provider.DefaultModel, DefaultProviderModel)
	}
	if cfg.Agent.MaxIterations != DefaultMaxIterations {
		t.Errorf("MaxIterations = %d, want %d", cfg.Agent.MaxIterations, DefaultMaxIterations)
	}
	if !cfg.Tools.RestrictToWorkspace {
		t.Error("RestrictToWorkspace should be true by default")
	}
	if cfg.Tools.Shell.Timeout != DefaultShellTimeout {
		t.Errorf("Shell.Timeout = %v, want %v", cfg.Tools.Shell.Timeout, DefaultShellTimeout)
	}
	if cfg.Paths.DataRoot != DefaultDataRoot {
		t.Errorf("Paths.DataRoot = %q, want %q", cfg.Paths.DataRoot, DefaultDataRoot)
	}
	if cfg.Paths.WorkspaceDir != DefaultTemplateDir {
		t.Errorf("Paths.WorkspaceDir = %q, want %q", cfg.Paths.WorkspaceDir, DefaultTemplateDir)
	}
}

func TestValidateAgentConfig_Valid(t *testing.T) {
	cfg := DefaultAgentConfig()
	cfg.Provider.APIKey = "sk-test"

	if err := ValidateAgentConfig(cfg); err != nil {
		t.Errorf("ValidateAgentConfig() unexpected error = %v", err)
	}
}

func TestValidateAgentConfig_MissingAPIKey(t *testing.T) {
	cfg := DefaultAgentConfig()

	if err := ValidateAgentConfig(cfg); err == nil {
		t.Error("ValidateAgentConfig() expected error for missing API key")
	}
}

func TestValidateAgentConfig_InvalidIterations(t *testing.T) {
	cfg := DefaultAgentConfig()
	cfg.Provider.APIKey = "sk-test"
	cfg.Agent.MaxIterations = 0

	if err := ValidateAgentConfig(cfg); err == nil {
		t.Error("ValidateAgentConfig() expected error for zero iterations")
	}
}

func TestResolveBusURL_FlagValue(t *testing.T) {
	url := ResolveBusURL("ws://custom:9000")
	if url != "ws://custom:9000" {
		t.Errorf("ResolveBusURL() = %q, want %q", url, "ws://custom:9000")
	}
}

func TestResolveBusURL_EnvVar(t *testing.T) {
	t.Setenv("NANOSEC_AGENT_BUS_URL", "ws://env:9000")

	url := ResolveBusURL("")
	if url != "ws://env:9000" {
		t.Errorf("ResolveBusURL() = %q, want %q", url, "ws://env:9000")
	}
}

func TestResolveBusURL_Default(t *testing.T) {
	// Ensure env var is not set
	os.Unsetenv("NANOSEC_AGENT_BUS_URL")

	url := ResolveBusURL("")
	if url != DefaultBusURL {
		t.Errorf("ResolveBusURL() = %q, want %q", url, DefaultBusURL)
	}
}

// TestResolveTunnelURL_FallsBackToTheBusURL pins the rolling-upgrade path: a
// deployment that does not set the tunnel address yet keeps dialing the bus.
func TestResolveTunnelURL_FallsBackToTheBusURL(t *testing.T) {
	os.Unsetenv("NANOSEC_AGENT_TUNNEL_URL")

	if got := ResolveTunnelURL("", "ws://nanobus:9000"); got != "ws://nanobus:9000" {
		t.Fatalf("ResolveTunnelURL() = %q, want the bus URL", got)
	}
}

func TestResolveTunnelURL_FlagWinsOverEnvAndBus(t *testing.T) {
	t.Setenv("NANOSEC_AGENT_TUNNEL_URL", "ws://env-tunnel:8080")

	if got := ResolveTunnelURL("ws://flag-tunnel:8080", "ws://nanobus:9000"); got != "ws://flag-tunnel:8080" {
		t.Fatalf("ResolveTunnelURL() = %q, want the flag value", got)
	}
	if got := ResolveTunnelURL("", "ws://nanobus:9000"); got != "ws://env-tunnel:8080" {
		t.Fatalf("ResolveTunnelURL() = %q, want the environment value", got)
	}
}

func TestDefaultAgentConfig_ShellTimeout(t *testing.T) {
	cfg := DefaultAgentConfig()
	want := 60 * time.Second
	if cfg.Tools.Shell.Timeout != want {
		t.Errorf("Shell.Timeout = %v, want %v", cfg.Tools.Shell.Timeout, want)
	}
}

func TestDefaultAgentConfig_LLMTimeouts(t *testing.T) {
	cfg := DefaultAgentConfig()
	if cfg.Agent.LLMTtfbTimeout != DefaultLLMTtfbTimeout {
		t.Errorf("LLMTtfbTimeout = %v, want %v", cfg.Agent.LLMTtfbTimeout, DefaultLLMTtfbTimeout)
	}
	if cfg.Agent.LLMStreamIdleTimeout != DefaultLLMStreamIdleTimeout {
		t.Errorf("LLMStreamIdleTimeout = %v, want %v", cfg.Agent.LLMStreamIdleTimeout, DefaultLLMStreamIdleTimeout)
	}
	if cfg.Agent.LLMRequestTimeout != DefaultLLMRequestTimeout {
		t.Errorf("LLMRequestTimeout = %v, want %v", cfg.Agent.LLMRequestTimeout, DefaultLLMRequestTimeout)
	}
	if DefaultLLMTtfbTimeout != 300*time.Second {
		t.Errorf("DefaultLLMTtfbTimeout = %v, want 300s", DefaultLLMTtfbTimeout)
	}
	if DefaultLLMRequestTimeout != 300*time.Second {
		t.Errorf("DefaultLLMRequestTimeout = %v, want 300s", DefaultLLMRequestTimeout)
	}
}

func TestApplyAgentConfig_LLMTtfbTimeout(t *testing.T) {
	cfg := DefaultAgentConfig()
	applyAgentConfig(&cfg, map[string]string{
		"agent.llmTtfbTimeout": "10m",
	})
	if cfg.Agent.LLMTtfbTimeout != 10*time.Minute {
		t.Errorf("LLMTtfbTimeout = %v, want 10m", cfg.Agent.LLMTtfbTimeout)
	}
}
