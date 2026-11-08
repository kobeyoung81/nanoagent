package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// httpClient talks to a remote streamable-HTTP (or plain SSE/JSON) MCP server.
// A single connection-level HTTP client is reused; Mcp-Session-Id is honoured
// when the server issues one on initialize.
type httpClient struct {
	server Server

	mu        sync.Mutex
	state     State
	lastErr   error
	client    *http.Client
	sessionID string
}

func (c *httpClient) State() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

func (c *httpClient) LastError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastErr
}

func (c *httpClient) setState(s State, err error) {
	c.mu.Lock()
	c.state = s
	if err != nil {
		c.lastErr = err
	} else if s == StateConnected {
		c.lastErr = nil
	}
	c.mu.Unlock()
}

func (c *httpClient) ensure(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == StateConnected {
		return nil
	}
	if c.server.URL == "" {
		return fmt.Errorf("mcp server %q: http transport requires a url", c.server.ID)
	}
	c.client = &http.Client{Timeout: 90 * time.Second}
	c.state = StateConnecting
	return nil
}

func (c *httpClient) send(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := nextID()
	raw, err := json.Marshal(jsonrpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.server.URL, strings.NewReader(string(raw)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range c.server.Headers {
		req.Header.Set(k, v)
	}

	c.mu.Lock()
	sessionID := c.sessionID
	client := c.client
	c.mu.Unlock()
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	if client == nil {
		return nil, fmt.Errorf("mcp %s: not started", c.server.ID)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mcp %s: request: %w", c.server.ID, err)
	}
	defer resp.Body.Close()

	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.mu.Lock()
		c.sessionID = sid
		c.mu.Unlock()
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("mcp %s: read body: %w", c.server.ID, err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("mcp %s: HTTP %d: %s", c.server.ID, resp.StatusCode, trimBody(body))
	}

	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "text/event-stream") {
		return parseSSEResponse(body, id)
	}
	return parseJSONResponse(body, id)
}

// parseJSONResponse extracts the response with the matching id.
func parseJSONResponse(body []byte, id int64) (json.RawMessage, error) {
	var resp jsonrpcResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("mcp: invalid json-rpc response: %w", err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("mcp: %s", resp.Error.Message)
	}
	return resp.Result, nil
}

// parseSSEResponse scans an SSE stream for the event whose data carries the
// matching json-rpc id.
func parseSSEResponse(body []byte, id int64) (json.RawMessage, error) {
	events := splitSSE(body)
	for _, data := range events {
		if strings.TrimSpace(data) == "" {
			continue
		}
		var resp jsonrpcResponse
		if err := json.Unmarshal([]byte(data), &resp); err != nil {
			continue
		}
		if resp.ID != id {
			continue
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("mcp: %s", resp.Error.Message)
		}
		return resp.Result, nil
	}
	return nil, fmt.Errorf("mcp: no matching event for request %d", id)
}

// splitSSE returns the joined data payload of each SSE event.
func splitSSE(body []byte) []string {
	var out []string
	var lines []string
	for _, rawLine := range strings.Split(string(body), "\n") {
		line := strings.TrimSuffix(strings.TrimRight(rawLine, "\r"), "\n")
		if line == "" {
			if len(lines) > 0 {
				out = append(out, strings.Join(lines, "\n"))
				lines = nil
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			lines = append(lines, strings.TrimPrefix(line, "data:"))
		}
	}
	if len(lines) > 0 {
		out = append(out, strings.Join(lines, "\n"))
	}
	return out
}

func trimBody(body []byte) string {
	s := strings.TrimSpace(string(body))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

func (c *httpClient) ListTools(ctx context.Context) ([]Tool, error) {
	if err := c.ensure(ctx); err != nil {
		c.setState(StateFailed, err)
		return nil, err
	}
	// initialize handshake first
	if c.sessionID == "" {
		for _, version := range supportedProtocolVersions {
			if _, err := c.send(ctx, "initialize", map[string]any{
				"protocolVersion": version,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"clientInfo":      map[string]any{"name": "nanosec", "version": "0.1"},
			}); err == nil {
				_, _ = c.send(ctx, "notifications/initialized", nil)
				break
			}
		}
	}
	result, err := c.send(ctx, "tools/list", nil)
	if err != nil {
		c.setState(StateFailed, err)
		return nil, err
	}
	c.setState(StateConnected, nil)
	var list struct {
		Tools []Tool `json:"tools"`
	}
	if err := json.Unmarshal(result, &list); err != nil {
		return nil, fmt.Errorf("mcp %s: parse tools/list: %w", c.server.ID, err)
	}
	if list.Tools == nil {
		list.Tools = []Tool{}
	}
	return list.Tools, nil
}

func (c *httpClient) CallTool(ctx context.Context, toolName string, arguments json.RawMessage) (string, error) {
	if err := c.ensure(ctx); err != nil {
		c.setState(StateFailed, err)
		return "", err
	}
	var args any
	if len(arguments) > 0 && string(arguments) != "null" {
		args = json.RawMessage(arguments)
	}
	result, err := c.send(ctx, "tools/call", map[string]any{"name": toolName, "arguments": args})
	if err != nil {
		c.setState(StateFailed, err)
		return "", err
	}
	c.setState(StateConnected, nil)
	text, isErr, err := renderCallResult(result)
	if err != nil {
		return "", err
	}
	if isErr {
		return "", fmt.Errorf("mcp tool %s/%s failed: %s", c.server.ID, toolName, text)
	}
	return text, nil
}

// UploadBlob sends a file argument's bytes beside the JSON-RPC channel.
//
// It is a plain POST, not a JSON-RPC request: the whole point is that the body
// never becomes a JSON value. The server headers (authorization, and the agent
// identity the bridge adds) go along unchanged, so the byte path is authorized
// exactly like the call that follows it.
func (c *httpClient) UploadBlob(ctx context.Context, body io.Reader, size int64, endpoint string) (*BlobRef, error) {
	if err := c.ensure(ctx); err != nil {
		c.setState(StateFailed, err)
		return nil, err
	}
	if strings.TrimSpace(endpoint) == "" {
		endpoint = "/mcp/blobs"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpointURL(endpoint), body)
	if err != nil {
		return nil, fmt.Errorf("mcp %s: upload request: %w", c.server.ID, err)
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "text/html")
	for k, v := range c.server.Headers {
		req.Header.Set(k, v)
	}

	c.mu.Lock()
	client := c.client
	c.mu.Unlock()
	if client == nil {
		return nil, fmt.Errorf("mcp %s: not started", c.server.ID)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mcp %s: upload: %w", c.server.ID, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("mcp %s: read upload response: %w", c.server.ID, err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("mcp %s: upload failed: HTTP %d: %s", c.server.ID, resp.StatusCode, trimBody(raw))
	}
	var ref BlobRef
	if err := json.Unmarshal(raw, &ref); err != nil {
		return nil, fmt.Errorf("mcp %s: decode upload response: %w", c.server.ID, err)
	}
	if ref.URI == "" {
		return nil, fmt.Errorf("mcp %s: upload response carried no uri", c.server.ID)
	}
	return &ref, nil
}

// OpenBlob streams a server-relative path (a download descriptor's raw_path).
func (c *httpClient) OpenBlob(ctx context.Context, path string) (io.ReadCloser, int64, error) {
	if err := c.ensure(ctx); err != nil {
		c.setState(StateFailed, err)
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpointURL(path), nil)
	if err != nil {
		return nil, 0, fmt.Errorf("mcp %s: download request: %w", c.server.ID, err)
	}
	for k, v := range c.server.Headers {
		req.Header.Set(k, v)
	}

	c.mu.Lock()
	client := c.client
	c.mu.Unlock()
	if client == nil {
		return nil, 0, fmt.Errorf("mcp %s: not started", c.server.ID)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("mcp %s: download: %w", c.server.ID, err)
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		return nil, 0, fmt.Errorf("mcp %s: download failed: HTTP %d: %s", c.server.ID, resp.StatusCode, trimBody(raw))
	}
	return resp.Body, resp.ContentLength, nil
}

// endpointURL turns a server-relative path into an absolute URL on the server's
// origin. The MCP URL itself may name /mcp; the byte routes are siblings of it.
func (c *httpClient) endpointURL(path string) string {
	if u, err := url.Parse(path); err == nil && u.IsAbs() {
		return path
	}
	base, err := url.Parse(c.server.URL)
	if err != nil {
		return path
	}
	base.Path = path
	base.RawPath = ""
	base.RawQuery = ""
	base.Fragment = ""
	return base.String()
}

func (c *httpClient) Close() error {
	c.mu.Lock()
	c.state = StateDisconnected
	c.client = nil
	c.sessionID = ""
	c.mu.Unlock()
	return nil
}
