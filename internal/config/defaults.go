package config

import "time"

// Default values for configuration.
const (
	DefaultBusURL = "ws://localhost:9000"

	DefaultProviderBaseURL = "https://api.openai.com/v1"
	DefaultProviderModel   = "gpt-4o"

	// DefaultDataRoot is the agent's data root. In the container it resolves to
	// the runtime user's home (/home/<username>) so agents/, sharedspace/ and
	// the profile files all live directly in the persisted user home (no dot
	// directory wrapper).
	DefaultDataRoot    = "~"
	DefaultTemplateDir = "/usr/share/nanosec/template" // image template used to seed per-agent homes

	DefaultMaxIterations             = 20
	DefaultContextWindow             = 128000
	DefaultMaxOutputTokens           = 4096
	DefaultMaxToolResultChars        = 16000
	DefaultSummarizeTokenPercent     = 75
	DefaultSummarizeMessageThreshold = 50
	DefaultTitleRefreshMessages      = 12
	DefaultMemoryRecentDays          = 3
	// DefaultScheduledTaskMode is what a scheduled task runs in when neither
	// the task nor the agent says otherwise: unattended work should be able to
	// act, and plan mode cannot.
	DefaultScheduledTaskMode = "build"

	// DefaultSessionSyncEnabled keeps async DB snapshot syncing on.
	DefaultSessionSyncEnabled = true
	// DefaultSessionSyncFlushInterval bounds the coalesced DB snapshot lag.
	DefaultSessionSyncFlushInterval = time.Second

	DefaultShellTimeout = 60 * time.Second

	// LLM resilience defaults: a stream that produces no event within
	// DefaultLLMTtfbTimeout (first token) is treated as a failed turn;
	// after the first token, a stream that produces no event within
	// DefaultLLMStreamIdleTimeout aborts; non-streaming calls cap at
	// DefaultLLMRequestTimeout; transient stream-open failures retry twice
	// (1s, 3s backoff).
	DefaultLLMTtfbTimeout       = 300 * time.Second
	DefaultLLMStreamIdleTimeout = 90 * time.Second
	DefaultLLMRequestTimeout    = 300 * time.Second
	DefaultLLMMaxRetries        = 2
)

// DefaultChannelCapabilities returns the per-channel messaging capability
// defaults. Values can be overridden via `channels.<name>.<cap>` config items
// under the "gateway" component (read by both the gateway and the agent).
func DefaultChannelCapabilities() map[string]ChannelCapabilities {
	return map[string]ChannelCapabilities{
		"web": {
			Streaming:             true,
			Markdown:              true,
			RichMedia:             true,
			Interactive:           true,
			AsyncReplies:          false,
			SessionResetSupported: true,
		},
		"wecom": {
			Streaming:             false,
			Markdown:              true, // WeCom supports markdown messages
			RichMedia:             true,
			Interactive:           false,
			AsyncReplies:          true,
			SessionResetSupported: true,
		},
		"wechatkf": {
			Streaming:             false,
			Markdown:              false,
			RichMedia:             true,
			Interactive:           false,
			AsyncReplies:          true,
			SessionResetSupported: false,
			MaxContentLen:         4000, // WeChat text limit
		},
		"feishu": {
			Streaming:             false,
			Markdown:              true,
			RichMedia:             true,
			Interactive:           false,
			AsyncReplies:          true,
			SessionResetSupported: true,
		},
		"cli": {
			Streaming:             true,
			Markdown:              true,
			RichMedia:             false,
			Interactive:           false,
			AsyncReplies:          false,
			SessionResetSupported: true,
		},
	}
}

// DefaultAgentConfig returns an AgentConfig with default values.
func DefaultAgentConfig() AgentConfig {
	return AgentConfig{
		Provider: ProviderConfig{
			BaseURL:      DefaultProviderBaseURL,
			DefaultModel: DefaultProviderModel,
		},
		Agent: AgentCoreConfig{
			MaxIterations:             DefaultMaxIterations,
			ContextWindow:             DefaultContextWindow,
			MaxOutputTokens:           DefaultMaxOutputTokens,
			MaxToolResultChars:        DefaultMaxToolResultChars,
			SummarizeTokenPercent:     DefaultSummarizeTokenPercent,
			SummarizeMessageThreshold: DefaultSummarizeMessageThreshold,
			TitleRefreshMessages:      DefaultTitleRefreshMessages,
			MemoryRecentDays:          DefaultMemoryRecentDays,
			ScheduledTaskMode:         DefaultScheduledTaskMode,
			LLMTtfbTimeout:            DefaultLLMTtfbTimeout,
			LLMStreamIdleTimeout:      DefaultLLMStreamIdleTimeout,
			LLMRequestTimeout:         DefaultLLMRequestTimeout,
			LLMMaxRetries:             DefaultLLMMaxRetries,
			SessionSyncEnabled:        DefaultSessionSyncEnabled,
			SessionSyncFlushInterval:  DefaultSessionSyncFlushInterval,
		},
		Tools: ToolsConfig{
			RestrictToWorkspace: true,
			Shell: ShellConfig{
				Timeout: DefaultShellTimeout,
			},
		},
		Paths:    DefaultPathsConfig(),
		Channels: DefaultChannelCapabilities(),
	}
}
