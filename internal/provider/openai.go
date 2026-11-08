package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	openai "github.com/sashabaranov/go-openai"
)

// OpenAIProvider implements the Provider interface for OpenAI-compatible APIs.
type OpenAIProvider struct {
	client       *openai.Client
	defaultModel string
	// requestTimeout bounds non-streaming Chat calls (summarizer, subagents).
	// Streaming calls are bounded by the agent's stream-idle watchdog instead,
	// because a wall-clock cap would cut off long-running generations.
	requestTimeout time.Duration
}

// NewOpenAI creates a new OpenAIProvider.
// apiKey: The API key.
// baseURL: Optional base URL (e.g. for compatible APIs like DeepSeek, Groq). If empty, uses default OpenAI URL.
// defaultModel: The model to use if none is specified in ChatRequest (e.g. "gpt-4o").
func NewOpenAI(apiKey, baseURL, defaultModel string, requestTimeout time.Duration) *OpenAIProvider {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: 10 * time.Second}).DialContext
	transport.TLSHandshakeTimeout = 10 * time.Second
	// Bounds only how long the HTTP layer waits for response headers. The
	// agent's own TTFB watchdog (LLMTtfbTimeout) makes the real decision and
	// would classify an abort as request_timeout; keep this generously above
	// it so the transport never 502s before the agent gets to decide.
	transport.ResponseHeaderTimeout = 10 * time.Minute
	config := openai.DefaultConfig(apiKey)
	if baseURL != "" {
		config.BaseURL = baseURL
	}
	config.HTTPClient = &http.Client{
		Transport: &loggingTransport{base: transport},
	}
	// Gateways commonly interleave SSE keep-alive/comment lines between data
	// frames. go-openai treats a run of empty events as a fatal error (default
	// limit 300), which aborts otherwise-healthy streams mid-generation. Raise
	// it well above any realistic heartbeat burst; a genuinely dead stream is
	// still caught by the agent's LLMStreamIdleTimeout watchdog.
	config.EmptyMessagesLimit = 100000
	return &OpenAIProvider{
		client:         openai.NewClientWithConfig(config),
		defaultModel:   defaultModel,
		requestTimeout: requestTimeout,
	}
}

// sessionIDKey carries the stable conversation identifier a provider attaches
// to an outgoing request context so the transport can emit it as a header.
type sessionIDKey struct{}

// withSessionID attaches the conversation-scoped session ID to ctx. A no-op for
// an empty ID.
func withSessionID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, sessionIDKey{}, id)
}

// sessionIDFromContext returns the session ID attached by withSessionID, or "".
func sessionIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(sessionIDKey{}).(string)
	return id
}

// loggingTransport wraps http.RoundTripper to log every LLM HTTP exchange.
// INFO level: method, URL, status, elapsed.
// DEBUG level: full request and response bodies.
type loggingTransport struct {
	base http.RoundTripper
}

func (t *loggingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()

	// go-openai v1.41.2 has no per-request header API, so the session ID is
	// carried on the request context and applied here. Gateways use X-Session-Id
	// to keep prompt-cache/routing affinity stable across a conversation.
	if sid := sessionIDFromContext(req.Context()); sid != "" {
		req.Header.Set("X-Session-Id", sid)
	}

	// Capture request body without consuming it.
	var reqBody string
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err == nil {
			req.Body = io.NopCloser(bytes.NewReader(b))
			reqBody = string(b)
		}
	}

	// Streaming completions must not be buffered here: reading the whole body
	// would consume the entire SSE stream before go-openai can read it,
	// destroying incremental token delivery (the client only ever sees a burst
	// after the LLM finishes). Only capture bodies for non-streaming requests.
	streaming := strings.Contains(reqBody, `"stream":true`)

	slog.Info("llm: →",
		"method", req.Method,
		"url", req.URL.String(),
	)
	slog.Debug("llm: → body", "body", reqBody)

	resp, err := t.base.RoundTrip(req)
	elapsed := time.Since(start)

	if err != nil {
		slog.Error("llm: request failed", "elapsed", elapsed, "error", err)
		return nil, err
	}

	// Capture response body without consuming it (non-streaming only).
	var respBody string
	if resp.Body != nil && !streaming {
		b, readErr := io.ReadAll(resp.Body)
		if readErr == nil {
			resp.Body = io.NopCloser(bytes.NewReader(b))
			respBody = string(b)
		}
	}

	slog.Info("llm: ←",
		"status", resp.StatusCode,
		"elapsed", elapsed,
	)
	slog.Debug("llm: ← body", "body", respBody)

	return resp, nil
}

func (p *OpenAIProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	ctx = withSessionID(ctx, req.SessionID)
	if p.requestTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.requestTimeout)
		defer cancel()
	}
	openaiReq, err := buildChatRequest(req, p.defaultModel, false)
	if err != nil {
		return nil, err
	}

	resp, err := p.client.CreateChatCompletion(ctx, openaiReq)
	if err != nil {
		// A deadline we imposed ourselves is an agent-side timeout, not an
		// upstream failure — classify it as request_timeout (non-retryable).
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("openai chat: %w", ErrRequestTimeout{
				Phase:    "request",
				Duration: p.requestTimeout,
			})
		}
		return nil, fmt.Errorf("openai chat: %w", err)
	}

	if len(resp.Choices) == 0 {
		return &ChatResponse{}, nil
	}

	choice := resp.Choices[0].Message

	logUsage("llm: usage", resp.Model, string(resp.Choices[0].FinishReason), resp.Usage)

	res := &ChatResponse{
		Content:      choice.Content,
		FinishReason: string(resp.Choices[0].FinishReason),
		Usage:        usageFrom(resp.Usage),
	}

	if len(choice.ToolCalls) > 0 {
		tcs := make([]ToolCall, len(choice.ToolCalls))
		for i, tc := range choice.ToolCalls {
			tcs[i] = ToolCall{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			}
		}
		res.ToolCalls = tcs
	}

	return res, nil
}

// ChatStream streams a completion over the OpenAI SSE API. Each content delta
// is emitted as a ChatStreamEvent; the final event is marked Done and carries
// the full content, any accumulated tool calls, and usage.
func (p *OpenAIProvider) ChatStream(ctx context.Context, req ChatRequest) (<-chan ChatStreamEvent, error) {
	ctx = withSessionID(ctx, req.SessionID)
	openaiReq, err := buildChatRequest(req, p.defaultModel, true)
	if err != nil {
		return nil, err
	}

	stream, err := p.client.CreateChatCompletionStream(ctx, openaiReq)
	if err != nil {
		return nil, fmt.Errorf("openai chat stream: %w", err)
	}

	ch := make(chan ChatStreamEvent, 8)
	go func() {
		defer stream.Close()
		defer close(ch)

		var full strings.Builder
		var usage Usage
		var finishReason string
		var toolCalls []ToolCall
		var order []string
		accs := map[string]*toolCallAccumulator{}
		// idxKey maps a chunk index to the call currently open at it, so an
		// id-less continuation (the standard OpenAI shape: the id is sent only
		// on a call's first delta) joins the right call instead of opening a
		// second one.
		idxKey := map[int]string{}
		lastKey := ""

		for {
			recv, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				ch <- ChatStreamEvent{Err: fmt.Errorf("openai chat stream recv: %w", err)}
				return
			}

			if recv.Usage != nil {
				usage = usageFrom(*recv.Usage)
			}

			if len(recv.Choices) == 0 {
				continue
			}
			delta := recv.Choices[0].Delta
			if fr := recv.Choices[0].FinishReason; fr != "" {
				finishReason = string(fr)
			}

			// Reasoning models (e.g. deepseek-reasoner) emit thoughts through
			// reasoning_content. Forward them as thought events so the agent can
			// stream them to clients that render intermediate reasoning.
			//
			// The thought goes first when a single boundary chunk carries both
			// fields: a client renders the reasoning block above the answer, so a
			// thought arriving after the answer would open a second block holding
			// only the tail of the reasoning.
			if delta.ReasoningContent != "" {
				ch <- ChatStreamEvent{Kind: "thought", Delta: delta.ReasoningContent}
			}

			if delta.Content != "" {
				full.WriteString(delta.Content)
				ch <- ChatStreamEvent{Kind: "content", Delta: delta.Content}
			}

			for i := range delta.ToolCalls {
				dc := &delta.ToolCalls[i]
				idx := 0
				hasIndex := dc.Index != nil
				if hasIndex {
					idx = *dc.Index
				}
				key := accumKey(dc.ID, hasIndex, idx, accs, idxKey, lastKey)
				acc, seen := accs[key]
				if !seen {
					acc = &toolCallAccumulator{}
					accs[key] = acc
					order = append(order, key)
				}
				if dc.ID != "" {
					acc.id = dc.ID
				}
				if dc.Function.Name != "" {
					acc.name = dc.Function.Name
				}
				if dc.Function.Arguments != "" {
					acc.args.WriteString(dc.Function.Arguments)
				}
				lastKey = key
			}
		}

		// Assemble in first-seen order. Indexing by position (0..len-1) used to
		// silently drop every call whose index was not dense from zero.
		for _, key := range order {
			acc := accs[key]
			id := acc.id
			if id == "" {
				// A call without an id cannot be answered: the provider requires
				// the tool message to carry the matching tool_call_id, and the
				// request sanitizer drops id-less calls (and their results).
				id = uuid.NewString()
			}
			toolCalls = append(toolCalls, ToolCall{
				ID:        id,
				Name:      acc.name,
				Arguments: acc.args.String(),
			})
		}

		ch <- ChatStreamEvent{
			Done:         true,
			FullContent:  full.String(),
			ToolCalls:    toolCalls,
			Usage:        usage,
			FinishReason: finishReason,
		}
	}()

	return ch, nil
}

// toolCallAccumulator collects the argument deltas of one tool call.
type toolCallAccumulator struct {
	id   string
	name string
	args strings.Builder
}

// accumKey picks the accumulator a delta belongs to, and records the call
// currently open at the chunk index so a later id-less continuation finds it.
//
// The id, when present, is authoritative: two calls that share a chunk index
// (some relays always report index 0) must not be concatenated, which is what
// produced arguments like `{...}{...}`. The catch is that OpenAI sends the id
// only on a call's FIRST delta and repeats just the index on every
// continuation — so an id-less chunk must join the call already open at its
// index, not start a new one. Opening a new accumulator there split a single
// call into a name-only call and an arguments-only call (the arguments-only one
// with an empty name, which surfaced as "tool not found: ").
func accumKey(id string, hasIndex bool, idx int, accs map[string]*toolCallAccumulator, idxKey map[int]string, lastKey string) string {
	if id != "" {
		key := "id:" + id
		idxKey[idx] = key
		return key
	}
	if hasIndex {
		if key, ok := idxKey[idx]; ok {
			return key
		}
		byIdx := "idx:" + strconv.Itoa(idx)
		if _, seen := accs[byIdx]; seen {
			idxKey[idx] = byIdx
			return byIdx
		}
		// No id anywhere and no call recorded at this index: a relay that
		// omits the index reports the zero value, so an open call is the one it
		// continues.
		if lastKey != "" && idx == 0 {
			idxKey[idx] = lastKey
			return lastKey
		}
		idxKey[idx] = byIdx
		return byIdx
	}
	// Neither id nor index: a continuation of whatever call is open.
	if lastKey != "" {
		return lastKey
	}
	byIdx := "idx:0"
	idxKey[0] = byIdx
	return byIdx
}

// buildChatRequest converts a provider ChatRequest into a go-openai request.
// streaming must be true for requests sent via CreateChatCompletionStream and
// false for CreateChatCompletion, because stream_options is only valid when
// stream is true: some gateways reject the field otherwise.
func buildChatRequest(req ChatRequest, defaultModel string, streaming bool) (openai.ChatCompletionRequest, error) {
	model := req.Model
	if model == "" {
		model = defaultModel
	}

	req.Messages = SanitizeMessages(req.Messages)
	msgs := make([]openai.ChatCompletionMessage, len(req.Messages))
	for i, m := range req.Messages {
		msg := openai.ChatCompletionMessage{
			Role:             m.Role,
			Content:          m.Content,
			ReasoningContent: m.Reasoning,
		}

		// A message with image parts is sent as multimodal content. go-openai
		// rejects Content and MultiContent together, so Content is cleared.
		if parts := multiContent(m.Parts); len(parts) > 0 {
			msg.Content = ""
			msg.MultiContent = parts
		}

		if m.ToolCallID != "" {
			msg.ToolCallID = m.ToolCallID
		}

		if len(m.ToolCalls) > 0 {
			tcs := make([]openai.ToolCall, len(m.ToolCalls))
			for j, tc := range m.ToolCalls {
				tcs[j] = openai.ToolCall{
					ID:   tc.ID,
					Type: openai.ToolTypeFunction,
					Function: openai.FunctionCall{
						Name:      tc.Name,
						Arguments: tc.Arguments,
					},
				}
			}
			msg.ToolCalls = tcs
		}

		msgs[i] = msg
	}

	var tools []openai.Tool
	if len(req.Tools) > 0 {
		tools = make([]openai.Tool, len(req.Tools))
		for i, t := range req.Tools {
			tools[i] = openai.Tool{
				Type: openai.ToolTypeFunction,
				Function: &openai.FunctionDefinition{
					Name:        t.Name,
					Description: t.Description,
					Parameters:  rawSchema(t),
				},
			}
		}
	}

	result := openai.ChatCompletionRequest{
		Model:           model,
		Messages:        msgs,
		Tools:           tools,
		ReasoningEffort: req.ReasoningEffort,
		Temperature:     req.Temperature,
		TopP:            req.TopP,
	}
	// stream_options is only valid alongside stream=true. Streaming requests
	// need include_usage to receive the final usage chunk; non-streaming
	// responses report usage directly, and sending stream_options without
	// stream is rejected by some OpenAI-compatible gateways.
	if streaming {
		result.StreamOptions = &openai.StreamOptions{IncludeUsage: true}
	}
	return result, nil
}

// usageFrom maps the SDK's usage onto ours, keeping the prompt-cache telemetry
// (prompt_tokens_details.cached_tokens) that used to be dropped.
func usageFrom(u openai.Usage) Usage {
	out := Usage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
	}
	if u.PromptTokensDetails != nil {
		out.CachedTokens = u.PromptTokensDetails.CachedTokens
	}
	return out
}

// logUsage reports one completion's usage, including how much of the prompt came
// from the upstream's prefix cache. cache_hit_ratio is what tells a prefix change
// apart from a cache that simply missed.
func logUsage(msg, model, finish string, u openai.Usage) {
	usage := usageFrom(u)
	ratio := 0.0
	if usage.PromptTokens > 0 {
		ratio = float64(usage.CachedTokens) / float64(usage.PromptTokens)
	}
	slog.Info(msg,
		"model", model,
		"finish_reason", finish,
		"prompt_tokens", usage.PromptTokens,
		"completion_tokens", usage.CompletionTokens,
		"total_tokens", usage.TotalTokens,
		"cached_tokens", usage.CachedTokens,
		"cache_hit_ratio", ratio,
	)
}

// multiContent maps provider content parts to go-openai parts. It returns nil
// when there are no usable parts, so callers fall back to plain text Content.
func multiContent(parts []ContentPart) []openai.ChatMessagePart {
	if len(parts) == 0 {
		return nil
	}
	out := make([]openai.ChatMessagePart, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "image_url":
			if p.ImageURL == "" {
				continue
			}
			out = append(out, openai.ChatMessagePart{
				Type:     openai.ChatMessagePartTypeImageURL,
				ImageURL: &openai.ChatMessageImageURL{URL: p.ImageURL},
			})
		default:
			if p.Text == "" {
				continue
			}
			out = append(out, openai.ChatMessagePart{
				Type: openai.ChatMessagePartTypeText,
				Text: p.Text,
			})
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// rawSchema returns the JSON-Schema bytes for a tool definition: the passed
// through RawParameters when set (MCP tool schemas), else the marshaled
// structured Parameters.
func rawSchema(t ToolDef) json.RawMessage {
	if len(t.RawParameters) > 0 {
		return t.RawParameters
	}
	if params, err := json.Marshal(t.Parameters); err == nil {
		return params
	}
	return json.RawMessage(`{"type":"object","properties":{}}`)
}
