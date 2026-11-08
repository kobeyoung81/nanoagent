// Package config defines the configuration structures for the nanosec agent.
package config

import "time"

// AgentConfig holds configuration for the agent service.
type AgentConfig struct {
	Provider ProviderConfig  `json:"provider" mapstructure:"provider"`
	Agent    AgentCoreConfig `json:"agent"    mapstructure:"agent"`
	Tools    ToolsConfig     `json:"tools"    mapstructure:"tools"`
	Media    MediaConfig     `json:"media"    mapstructure:"media"`
	Paths    PathsConfig     `json:"paths"    mapstructure:"paths"`
	// Channels maps a channel name ("web", "wecom", "wechatkf", "feishu",
	// "cli") to the messaging capabilities the agent must adapt its output to.
	Channels map[string]ChannelCapabilities `json:"channels" mapstructure:"channels"`
}

// ChannelCapabilities describes what a channel can and cannot do. The agent
// adapts its output (markdown vs plain text, length limits, interactive
// elements) and the gateway adapts delivery (streaming vs batch) to these.
type ChannelCapabilities struct {
	// Streaming indicates the channel renders tokens incrementally (web).
	Streaming bool `json:"streaming" mapstructure:"streaming"`
	// Markdown indicates the channel renders markdown (web, wecom, feishu).
	Markdown bool `json:"markdown" mapstructure:"markdown"`
	// RichMedia indicates the channel supports media attachments.
	RichMedia bool `json:"richMedia" mapstructure:"richMedia"`
	// Interactive indicates the channel supports interactive elements (buttons/menus).
	Interactive bool `json:"interactive" mapstructure:"interactive"`
	// AsyncReplies indicates replies are delivered asynchronously, not in a live session.
	AsyncReplies bool `json:"asyncReplies" mapstructure:"asyncReplies"`
	// SessionResetSupported indicates a /new-style reset command can be sent.
	SessionResetSupported bool `json:"sessionResetSupported" mapstructure:"sessionResetSupported"`
	// MaxContentLen caps the content length per message (0 = unlimited).
	MaxContentLen int `json:"maxContentLen" mapstructure:"maxContentLen"`
}

// ProviderConfig holds LLM provider settings. Platform LLM configuration is
// owned by the model catalog (model_providers/models); the runtime folds the
// selected catalog model's provider connection into these fields.
type ProviderConfig struct {
	APIKey       string `json:"apiKey"        mapstructure:"apiKey"`
	BaseURL      string `json:"baseUrl"       mapstructure:"baseUrl"`
	DefaultModel string `json:"defaultModel"  mapstructure:"defaultModel"`
}

// AgentCoreConfig holds agent behavior settings.
type AgentCoreConfig struct {
	MaxIterations   int `json:"maxIterations"             mapstructure:"maxIterations"`
	ContextWindow   int `json:"contextWindow"             mapstructure:"contextWindow"`
	MaxOutputTokens int `json:"maxOutputTokens"           mapstructure:"maxOutputTokens"`
	// ThinkingEffort is the resolved reasoning effort for the MAIN loop, layered
	// most specific first: the user's per-(agent, mode) choice, the model's
	// capability, the AgentType's `thinking` setting, then the model catalog
	// entry. Passed through as reasoning_effort; empty sends no level at all.
	// See cmd/nanosec/models.go (resolveThinkingEffort).
	ThinkingEffort string `json:"thinkingEffort" mapstructure:"thinkingEffort"`
	// Temperature/TopP are optional per-model sampling parameters (0 = unset).
	Temperature               float32 `json:"temperature" mapstructure:"temperature"`
	TopP                      float32 `json:"topP" mapstructure:"topP"`
	MaxToolResultChars        int     `json:"maxToolResultChars"        mapstructure:"maxToolResultChars"`
	SummarizeTokenPercent     int     `json:"summarizeTokenPercent"     mapstructure:"summarizeTokenPercent"`
	SummarizeMessageThreshold int     `json:"summarizeMessageThreshold" mapstructure:"summarizeMessageThreshold"`
	// TitleRefreshMessages is the minimum number of new messages before an
	// existing conversation title is regenerated (0 disables refresh).
	TitleRefreshMessages int `json:"titleRefreshMessages" mapstructure:"titleRefreshMessages"`
	MemoryRecentDays     int `json:"memoryRecentDays"          mapstructure:"memoryRecentDays"`
	// ScheduledTaskMode is the platform default mode a scheduled task's turns
	// run in ("build" | "plan"). A task's own mode and the agent's
	// agents.settings override both win over it (see cron.TaskMode).
	ScheduledTaskMode string `json:"scheduledTaskMode" mapstructure:"scheduledTaskMode"`
	// LanguageDefault is the platform default for the per-turn language note
	// ("zh", "en", …). A user's own choice overrides it; there is no
	// follow-the-message mode.
	LanguageDefault string `json:"languageDefault" mapstructure:"languageDefault"`
	// LLMTtfbTimeout bounds how long the agent waits for the FIRST event of a
	// streaming LLM turn (time-to-first-byte). Reasoning models can think for
	// minutes before their first token, so this must be generous. 0 disables
	// the TTFB watchdog (the transport ResponseHeaderTimeout still applies).
	// Default 300s.
	LLMTtfbTimeout time.Duration `json:"llmTtfbTimeout" mapstructure:"llmTtfbTimeout"`
	// LLMStreamIdleTimeout aborts a streaming LLM turn when no event arrives
	// for this long AFTER the first event, so a provider that stalls
	// mid-generation can never wedge the agent loop (long thinking before the
	// first token is governed by LLMTtfbTimeout instead). 0 disables the
	// watchdog (not recommended). Default 90s.
	LLMStreamIdleTimeout time.Duration `json:"llmStreamIdleTimeout" mapstructure:"llmStreamIdleTimeout"`
	// LLMRequestTimeout bounds non-streaming LLM calls (summarizer,
	// subagents). 0 disables. Default 300s.
	LLMRequestTimeout time.Duration `json:"llmRequestTimeout" mapstructure:"llmRequestTimeout"`
	// LLMMaxRetries is the number of automatic retries for transient failures
	// (429 / 5xx / network) when opening a stream. 0 disables retries.
	// Default 2.
	LLMMaxRetries int `json:"llmMaxRetries" mapstructure:"llmMaxRetries"`
	// LLMSummarizeReasoningEffort caps the model's thinking for the
	// non-streaming summarizer calls (session summarization + pre-compaction
	// memory flush). Passed through verbatim as reasoning_effort; empty means
	// the parameter is never sent. A reasoning model still thinks without it,
	// so use "none" to turn thinking off; "low" caps it.
	LLMSummarizeReasoningEffort string `json:"llmSummarizeReasoningEffort" mapstructure:"llmSummarizeReasoningEffort"`
	// LLMSubagentReasoningEffort does the same for spawned subagent calls.
	LLMSubagentReasoningEffort string `json:"llmSubagentReasoningEffort" mapstructure:"llmSubagentReasoningEffort"`
	// SessionSyncEnabled controls async DB snapshot syncing (JSONL stays
	// synchronous and authoritative). Default true.
	SessionSyncEnabled bool `json:"sessionSyncEnabled" mapstructure:"sessionSyncEnabled"`
	// SessionSyncFlushInterval bounds how long the coalesced DB snapshot can lag
	// the JSONL write. Default 1s.
	SessionSyncFlushInterval time.Duration `json:"sessionSyncFlushInterval" mapstructure:"sessionSyncFlushInterval"`
}

// ToolsConfig holds tool system settings.
type ToolsConfig struct {
	RestrictToWorkspace bool        `json:"restrictToWorkspace" mapstructure:"restrictToWorkspace"`
	Shell               ShellConfig `json:"shell"              mapstructure:"shell"`
}

// ShellConfig holds shell tool settings.
type ShellConfig struct {
	Timeout time.Duration `json:"timeout" mapstructure:"timeout"`
}

// MediaConfig holds WeCom media API settings for file upload/download.
type MediaConfig struct {
	CorpID     string `json:"corpID"     mapstructure:"corpID"`
	CorpSecret string `json:"corpSecret" mapstructure:"corpSecret"`
	AgentID    int64  `json:"agentID"    mapstructure:"agentID"`
}
