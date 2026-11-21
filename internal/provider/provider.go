package provider

import (
	"context"
	"encoding/json"
	"time"
)

// Provider defines the interface for LLM providers.
type Provider interface {
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
	// ChatStream streams a completion incrementally. The returned channel is
	// closed when the stream ends; the final event carries Done with
	// FullContent. Providers that cannot stream may synthesize a single Done
	// event from Chat.
	ChatStream(ctx context.Context, req ChatRequest) (<-chan ChatStreamEvent, error)
}

// ChatStreamEvent is a single event from a streaming Chat call.
type ChatStreamEvent struct {
	// Kind is "content" (answer text, default) or "thought" (reasoning).
	Kind string `json:"kind,omitempty"`
	// Delta is the incremental text produced since the previous event.
	Delta string `json:"delta,omitempty"`
	// ToolCalls is set on the final event when the turn produced tool calls.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// FullContent is the complete accumulated content, set on the Done event.
	FullContent string `json:"full_content,omitempty"`
	// Usage is the token usage, set on the Done event when the provider reports it.
	Usage Usage `json:"usage,omitempty"`
	// FinishReason is the upstream stop reason carried by the Done event
	// ("stop", "tool_calls", "length", ...). "length" means the completion was
	// cut at the output limit, so a tool call's arguments can be incomplete.
	FinishReason string `json:"finish_reason,omitempty"`
	// Done marks the final event of the stream.
	Done bool `json:"done,omitempty"`
	// Err reports a stream failure; the stream is closed after Err.
	Err error `json:"-"`
}

// ChatRequest represents a request to the LLM.
type ChatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Tools    []ToolDef `json:"tools,omitempty"`
	// SessionID is a stable, conversation-scoped identifier sent to the gateway
	// as the X-Session-Id header. Gateways use it to keep an upstream prompt
	// cache/routing affinity pinned to the same conversation even when the
	// request prefix changes (e.g. a rotating conversation summary). Callers
	// should namespace it per purpose ("<session>:summarize", ":title").
	SessionID string `json:"session_id,omitempty"`
	// ReasoningEffort caps the model's thinking for reasoning models
	// ("low"/"minimal"/...). Passed through verbatim as the OpenAI
	// reasoning_effort parameter; empty means the parameter is never sent
	// (safe for providers and models that do not support it).
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	// Temperature / TopP are optional per-model sampling parameters. A nil
	// temperature means the parameter is omitted (provider default); a pointer
	// to zero is an explicit "decode deterministically" and must reach the wire
	// as 0, which is why this one is a pointer and TopP is not.
	Temperature *float32 `json:"temperature,omitempty"`
	TopP        float32  `json:"top_p,omitempty"`
}

// Message represents a chat message.
type Message struct {
	Role       string     `json:"role"`              // "system", "user", "assistant", "tool", "thought"
	Content    string     `json:"content,omitempty"` // Text content
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"` // For tool role
	Timestamp  time.Time  `json:"timestamp,omitempty"`    // When the message was added
	// Reasoning is the model's private reasoning ("reasoning_content") for an
	// assistant turn. Reasoning models that operate in thinking mode require it
	// to be echoed back on subsequent requests whenever tools are present, or
	// the provider rejects the request with a 400. It is kept off the wire for
	// models that never emit it (empty → omitted).
	Reasoning string `json:"reasoning_content,omitempty"`
	// ThoughtElapsedMs is set on role "thought" rows: the wall-clock time the
	// model spent generating that reasoning block, used to render "Thought for
	// Xs" after a session restore.
	ThoughtElapsedMs int64 `json:"thought_elapsed_ms,omitempty"`
	// Detail is set on role "error" rows: a sanitized, human-readable
	// supplement to the failure category carried in Content.
	Detail string `json:"detail,omitempty"`
	// Images are local image attachments. Persisted as lightweight path
	// references only; the agent expands them to inline parts at request time
	// so the session JSON never carries base64 blobs.
	Images []ImageRef `json:"images,omitempty"`
	// Parts is the transient, wire-ready representation (text + image parts)
	// populated just before a provider call. Never persisted (json:"-").
	Parts []ContentPart `json:"-"`
}

// ImageRef is a local image attachment referenced by a session message. Path is
// relative to the agent/shared workspace root.
type ImageRef struct {
	Path     string `json:"path"`
	MimeType string `json:"mime_type,omitempty"`
	Bytes    int    `json:"bytes,omitempty"`
}

// ContentPart is a wire-ready message part. Type is "text" or "image_url";
// ImageURL holds an inline data: URL built at request time.
type ContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

// ToolDef represents a tool definition sent to the LLM.
type ToolDef struct {
	Name string `json:"name"`
	// Description is a human-readable description of what the tool does.
	Description string `json:"description"`
	// Parameters is the structured JSON-Schema for builtin tools.
	Parameters ParameterSchema `json:"parameters,omitempty"`
	// RawParameters is an optional pass-through JSON-Schema (e.g. MCP tool
	// input schemas, which are richer than ParameterSchema). When set it is
	// sent verbatim to the provider instead of Parameters.
	RawParameters json.RawMessage `json:"raw_parameters,omitempty"`
}

// ParameterSchema represents JSON schema for tool parameters.
// Currently supports top-level object with properties.
type ParameterSchema struct {
	Type       string                    `json:"type"` // "object"
	Properties map[string]PropertySchema `json:"properties"`
	Required   []string                  `json:"required,omitempty"`
}

// PropertySchema represents a property in the parameter schema.
type PropertySchema struct {
	Type        string `json:"type"` // "string", "number", "boolean", etc.
	Description string `json:"description"`
}

// ToolCall represents a request from the LLM to execute a tool.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON string
}

// Usage represents token usage from an LLM response.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// CachedTokens is how much of the prompt the upstream served from its prefix
	// cache (prompt_tokens_details.cached_tokens). It is the only ground truth for
	// whether a request kept the cached prefix, so it is worth reporting.
	CachedTokens int `json:"cached_tokens,omitempty"`
}

// ChatResponse represents the response from the LLM.
type ChatResponse struct {
	Content   string     `json:"content,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	Usage     Usage      `json:"usage,omitempty"`
	// Thought is the reasoning content the model emitted before this response
	// (empty when the model does not expose reasoning).
	Thought string `json:"thought,omitempty"`
	// ThoughtElapsedMs is how long the model spent generating Thought.
	ThoughtElapsedMs int64 `json:"thought_elapsed_ms,omitempty"`
	// FinishReason is the upstream stop reason (see ChatStreamEvent). "length"
	// means the upstream cut the completion at its output limit.
	FinishReason string `json:"finish_reason,omitempty"`
}
