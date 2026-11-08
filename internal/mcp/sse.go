package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// sseClient speaks the legacy HTTP+SSE MCP transport: a long-lived GET stream
// announces a POST endpoint (event: endpoint), JSON-RPC requests are POSTed to
// it, and responses arrive back on the stream (event: message). The stream is
// owned by the client and lives until Close, independent of per-call timeouts.
type sseClient struct {
	server Server

	mu       sync.Mutex
	state    State
	lastErr  error
	gen      int
	client   *http.Client
	connCtx  context.Context
	cancel   context.CancelFunc
	body     io.ReadCloser
	endpoint string
	pending  map[int64]chan jsonrpcResponse

	initialized bool
}

func (c *sseClient) State() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

func (c *sseClient) LastError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastErr
}

func (c *sseClient) setState(s State, err error) {
	c.mu.Lock()
	c.state = s
	if err != nil {
		c.lastErr = err
	} else if s == StateConnected {
		c.lastErr = nil
	}
	c.mu.Unlock()
}

func (c *sseClient) ensure(ctx context.Context) error {
	c.mu.Lock()
	if c.state == StateConnected {
		c.mu.Unlock()
		return nil
	}
	if c.state == StateConnecting {
		c.mu.Unlock()
		return fmt.Errorf("mcp %s: connection in progress", c.server.ID)
	}
	c.state = StateConnecting
	c.mu.Unlock()

	if err := c.connect(ctx); err != nil {
		c.setState(StateFailed, err)
		return err
	}
	c.setState(StateConnected, nil)
	return nil
}

func (c *sseClient) connect(ctx context.Context) error {
	if c.server.URL == "" {
		return fmt.Errorf("mcp server %q: sse transport requires a url", c.server.ID)
	}
	connCtx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(connCtx, http.MethodGet, c.server.URL, nil)
	if err != nil {
		cancel()
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-store")
	for k, v := range c.server.Headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 0} // stream stays open
	resp, err := client.Do(req)
	if err != nil {
		cancel()
		return fmt.Errorf("mcp %s: sse connect: %w", c.server.ID, err)
	}
	if resp.StatusCode >= 400 {
		cancel()
		_ = resp.Body.Close()
		return fmt.Errorf("mcp %s: sse HTTP %d", c.server.ID, resp.StatusCode)
	}

	br := bufio.NewReader(resp.Body)
	endpoint, err := readSSEEndpoint(br, c.server.URL)
	if err != nil {
		cancel()
		_ = resp.Body.Close()
		return fmt.Errorf("mcp %s: sse endpoint: %w", c.server.ID, err)
	}

	c.mu.Lock()
	c.gen++
	gen := c.gen
	c.client = client
	c.connCtx = connCtx
	c.cancel = cancel
	c.body = resp.Body
	c.endpoint = endpoint
	c.mu.Unlock()

	go c.readLoop(gen, br)
	return nil
}

// readSSEEndpoint consumes events until the endpoint announcement arrives.
func readSSEEndpoint(br *bufio.Reader, base string) (string, error) {
	for {
		event, data, err := readSSEEvent(br)
		if err != nil {
			return "", err
		}
		if event == "endpoint" && strings.TrimSpace(data) != "" {
			return resolveURL(base, strings.TrimSpace(data)), nil
		}
	}
}

func resolveURL(base, ref string) string {
	b, err := url.Parse(base)
	if err != nil {
		return ref
	}
	r, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return b.ResolveReference(r).String()
}

func (c *sseClient) readLoop(gen int, br *bufio.Reader) {
	for {
		event, data, err := readSSEEvent(br)
		if err != nil {
			c.markDead(gen, err)
			return
		}
		if event == "endpoint" {
			if u := strings.TrimSpace(data); u != "" {
				c.mu.Lock()
				c.endpoint = resolveURL(c.server.URL, u)
				c.mu.Unlock()
			}
			continue
		}
		if strings.TrimSpace(data) == "" {
			continue
		}
		var resp jsonrpcResponse
		if json.Unmarshal([]byte(data), &resp) != nil {
			continue
		}
		c.mu.Lock()
		ch, ok := c.pending[resp.ID]
		if ok {
			delete(c.pending, resp.ID)
		}
		c.mu.Unlock()
		if ok {
			ch <- resp
		}
	}
}

func (c *sseClient) markDead(gen int, err error) {
	c.mu.Lock()
	if c.gen != gen {
		c.mu.Unlock()
		return
	}
	for id, ch := range c.pending {
		delete(c.pending, id)
		close(ch)
	}
	if c.state != StateDisconnected {
		c.state = StateFailed
		c.lastErr = fmt.Errorf("mcp %s: %w", c.server.ID, err)
	}
	c.mu.Unlock()
}

// readSSEEvent reads one SSE event (event:, data: lines terminated by a blank
// line) from the stream.
func readSSEEvent(br *bufio.Reader) (event, data string, err error) {
	var dataLines []string
	for {
		line, rerr := br.ReadString('\n')
		if len(line) == 0 && rerr != nil {
			if len(dataLines) > 0 {
				return event, strings.Join(dataLines, "\n"), nil
			}
			return "", "", rerr
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if len(dataLines) == 0 && event == "" {
				if rerr != nil {
					return "", "", rerr
				}
				continue
			}
			return event, strings.Join(dataLines, "\n"), nil
		}
		switch {
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
		if rerr != nil {
			return event, strings.Join(dataLines, "\n"), rerr
		}
	}
}

// request POSTs a JSON-RPC request and waits for its response on the stream.
func (c *sseClient) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	endpoint := c.endpoint
	client := c.client
	if endpoint == "" || client == nil {
		c.mu.Unlock()
		return nil, fmt.Errorf("mcp %s: sse endpoint not ready", c.server.ID)
	}
	id := nextID()
	ch := make(chan jsonrpcResponse, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	raw, err := json.Marshal(jsonrpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range c.server.Headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("mcp %s: sse post: %w", c.server.ID, err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("mcp %s: sse HTTP %d", c.server.ID, resp.StatusCode)
	}

	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("mcp %s: %s: %w", c.server.ID, method, ctx.Err())
	case r, ok := <-ch:
		if !ok {
			c.mu.Lock()
			le := c.lastErr
			c.mu.Unlock()
			return nil, fmt.Errorf("mcp %s: stream closed: %v", c.server.ID, le)
		}
		if r.Error != nil {
			return nil, fmt.Errorf("mcp %s: %s", c.server.ID, r.Error.Message)
		}
		return r.Result, nil
	}
}

func (c *sseClient) initialize(ctx context.Context) error {
	var lastErr error
	for _, version := range supportedProtocolVersions {
		if _, err := c.request(ctx, "initialize", map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"clientInfo":      map[string]any{"name": "nanosec", "version": "0.1"},
		}); err == nil {
			_ = c.notify(ctx, "notifications/initialized", nil)
			c.mu.Lock()
			c.initialized = true
			c.mu.Unlock()
			return nil
		} else {
			lastErr = err
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no compatible protocol version")
	}
	return fmt.Errorf("mcp %s: initialize: %w", c.server.ID, lastErr)
}

// notify POSTs a JSON-RPC notification (no id, no response awaited).
func (c *sseClient) notify(ctx context.Context, method string, params any) error {
	c.mu.Lock()
	endpoint := c.endpoint
	client := c.client
	c.mu.Unlock()
	if endpoint == "" || client == nil {
		return fmt.Errorf("mcp %s: sse endpoint not ready", c.server.ID)
	}
	raw, err := json.Marshal(jsonrpcNotification{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range c.server.Headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return nil
}

func (c *sseClient) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if err := c.ensure(ctx); err != nil {
		return nil, err
	}
	if c.needsInit() {
		if err := c.initialize(ctx); err != nil {
			c.setState(StateFailed, err)
			return nil, err
		}
		c.mu.Lock()
		c.lastErr = nil
		c.mu.Unlock()
	}
	res, err := c.request(ctx, method, params)
	if err == nil {
		return res, nil
	}
	if c.State() == StateConnected {
		return nil, err
	}
	c.shutdown()
	if rerr := c.ensure(ctx); rerr != nil {
		return nil, err
	}
	if c.needsInit() {
		if ierr := c.initialize(ctx); ierr != nil {
			return nil, err
		}
	}
	return c.request(ctx, method, params)
}

func (c *sseClient) needsInit() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.initialized
}

func (c *sseClient) ListTools(ctx context.Context) ([]Tool, error) {
	result, err := c.call(ctx, "tools/list", nil)
	if err != nil {
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

func (c *sseClient) CallTool(ctx context.Context, toolName string, arguments json.RawMessage) (string, error) {
	var args any
	if len(arguments) > 0 && string(arguments) != "null" {
		args = json.RawMessage(arguments)
	}
	result, err := c.call(ctx, "tools/call", map[string]any{"name": toolName, "arguments": args})
	if err != nil {
		return "", err
	}
	text, isErr, err := renderCallResult(result)
	if err != nil {
		return "", err
	}
	if isErr {
		return "", fmt.Errorf("mcp tool %s/%s failed: %s", c.server.ID, toolName, text)
	}
	return text, nil
}

func (c *sseClient) Close() error {
	c.shutdown()
	return nil
}

func (c *sseClient) shutdown() {
	c.mu.Lock()
	cancel := c.cancel
	body := c.body
	c.cancel = nil
	c.body = nil
	c.endpoint = ""
	c.client = nil
	c.gen++
	for id, ch := range c.pending {
		delete(c.pending, id)
		close(ch)
	}
	c.state = StateDisconnected
	c.initialized = false
	c.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if body != nil {
		_ = body.Close()
	}
}
