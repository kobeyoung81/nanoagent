package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanosec/internal/mcp"
	"gitee.com/kobeyoung81/nanosec/internal/tool"
)

// McpServerStatus is the per-server connection state surfaced to the portal.
type McpServerStatus struct {
	ID        string    `json:"id"`
	Transport string    `json:"transport"`
	State     mcp.State `json:"state"`
	ToolCount int       `json:"tool_count"`
	Tools     []string  `json:"tools"`
	LastError string    `json:"last_error,omitempty"`
}

// McpBridge owns the MCP client connections for one agent and adapts remote
// MCP tools into local tool.Tool instances. It reconciles on capability
// changes (connect/cull/restart by spec hash) and serializes tool calls.
type McpBridge struct {
	// agentID travels on every MCP request as X-Nanowork-Agent-ID. A
	// first-party server (nanoartifacts) uses it to resolve the owner; the
	// shared internal token alone would let any caller act as any agent.
	agentID string

	// workspace and shared bound a file argument's path, exactly as they bound
	// the file tools: a tool argument must not be able to read or write outside.
	workspace string
	shared    string

	callTimeout time.Duration

	mu      sync.Mutex
	clients map[string]mcp.Client
	specs   map[string]mcpspec
}

type mcpspec struct {
	server mcp.Server
	tools  map[string]mcp.Tool
}

// NewMcpBridge creates an empty bridge for one agent.
func NewMcpBridge(agentID, workspace, shared string) *McpBridge {
	return &McpBridge{
		agentID:     agentID,
		workspace:   workspace,
		shared:      shared,
		callTimeout: 120 * time.Second,
		clients:     map[string]mcp.Client{},
		specs:       map[string]mcpspec{},
	}
}

// Sync reconciles clients with the enabled server set and returns the tool
// adapters to register. Servers that fail to connect are logged and skipped so
// the agent keeps working with the healthy subset. An empty set culls
// everything (used when the per-agent MCP master toggle is off).
func (b *McpBridge) Sync(ctx context.Context, servers []nanoprotocol.McpServerDef) []tool.Tool {
	b.mu.Lock()
	defer b.mu.Unlock()

	target := make(map[string]mcp.Server, len(servers))
	for _, def := range servers {
		// Copy the catalog headers before adding the agent identity: the
		// definition came from a shared snapshot, and mutating its map would
		// change what every other reader of that snapshot sees.
		headers := make(map[string]string, len(def.Headers)+1)
		for k, v := range def.Headers {
			headers[k] = v
		}
		if b.agentID != "" {
			headers["X-Nanowork-Agent-ID"] = b.agentID
		}
		target[def.ID] = mcp.Server{
			ID:        def.ID,
			Transport: def.Transport,
			URL:       def.URL,
			Headers:   headers,
		}
	}

	// Drop or restart servers whose spec changed.
	for id, spec := range b.specs {
		next, keep := target[id]
		if !keep || specHash(next) != specHash(spec.server) {
			b.closeLocked(id)
		}
	}

	listCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	adapters := []tool.Tool{}
	for id, server := range target {
		spec, ok := b.specs[id]
		if !ok || b.clients[id] == nil {
			// New server (or a prior connect was culled): connect + discover.
			client := mcp.NewClient(server)
			tools, err := withListTimeout(listCtx, client)
			if err != nil {
				slog.Warn("mcp: server connection failed, skipping", "server", id, "error", err)
				_ = client.Close()
				continue
			}
			b.clients[id] = client
			spec = mcpspec{server: server, tools: map[string]mcp.Tool{}}
			for _, t := range tools {
				spec.tools[t.Name] = t
			}
			b.specs[id] = spec
		} else {
			// A server can add or remove tools without changing its spec: the spec
			// hash covers connection parameters, not the tool list. Re-list on every
			// reconcile so a tool deployed behind an unchanged URL becomes visible
			// without restarting the agent (a degraded client reconnects inside
			// ListTools).
			if tools, err := withListTimeout(listCtx, b.clients[id]); err == nil {
				spec.tools = map[string]mcp.Tool{}
				for _, t := range tools {
					spec.tools[t.Name] = t
				}
				b.specs[id] = spec
			} else {
				slog.Debug("mcp: tool refresh failed, keeping the known list", "server", id, "error", err)
			}
		}
		for toolName, t := range spec.tools {
			// The model gets a plain string property; the extension is an
			// instruction to this bridge, not to the model.
			schema := stripFileParams(t.InputSchema)
			if len(schema) == 0 {
				schema = []byte(`{"type":"object","properties":{}}`)
			}
			adapters = append(adapters, &tool.McpTool{
				NameValue:        mcp.FullName(id, toolName),
				DescriptionValue: t.Description,
				RawSchema:        schema,
				Caller:           b,
				ServerID:         id,
				ToolName:         toolName,
			})
		}
	}

	for id := range b.specs {
		if _, keep := target[id]; !keep {
			b.closeLocked(id)
		}
	}
	return adapters
}

func withListTimeout(ctx context.Context, client mcp.Client) ([]mcp.Tool, error) {
	type result struct {
		tools []mcp.Tool
		err   error
	}
	ch := make(chan result, 1)
	go func() {
		tools, err := client.ListTools(ctx)
		ch <- result{tools, err}
	}()
	select {
	case r := <-ch:
		return r.tools, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func specHash(s mcp.Server) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func (b *McpBridge) closeLocked(id string) {
	if c, ok := b.clients[id]; ok {
		_ = c.Close()
		delete(b.clients, id)
	}
	delete(b.specs, id)
}

// Status returns the connection state of every known MCP server.
func (b *McpBridge) Status() []McpServerStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]McpServerStatus, 0, len(b.specs))
	for id, spec := range b.specs {
		st := McpServerStatus{
			ID:        id,
			Transport: spec.server.Transport,
			State:     mcp.StateDisconnected,
			Tools:     []string{},
		}
		if c, ok := b.clients[id]; ok {
			st.State = c.State()
			if err := c.LastError(); err != nil {
				st.LastError = err.Error()
			}
		}
		for name := range spec.tools {
			st.Tools = append(st.Tools, name)
		}
		st.ToolCount = len(st.Tools)
		out = append(out, st)
	}
	return out
}

// CallTool implements tool.McpCaller.
//
// File arguments are resolved here, around the JSON-RPC call: uploads before it
// (the server is handed a handle instead of a path), downloads after it (the
// descriptor becomes a file in the workspace). Everything else passes through
// untouched.
func (b *McpBridge) CallTool(ctx context.Context, serverID, toolName, argumentsJSON string) (string, error) {
	b.mu.Lock()
	client, ok := b.clients[serverID]
	spec := b.specs[serverID]
	b.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("mcp server %q is not connected", serverID)
	}

	var args map[string]any
	if trimmed := strings.TrimSpace(argumentsJSON); trimmed != "" && trimmed != "null" {
		if err := json.Unmarshal([]byte(trimmed), &args); err != nil {
			return "", fmt.Errorf("mcp tool %s/%s: invalid arguments: %w", serverID, toolName, err)
		}
	}
	var params map[string]fileParam
	if t, known := spec.tools[toolName]; known {
		params = fileParams(t.InputSchema)
	}

	ctx, cancel := context.WithTimeout(ctx, b.callTimeout)
	defer cancel()

	if len(params) > 0 {
		if err := b.materializeUploads(ctx, client, params, args); err != nil {
			return "", fmt.Errorf("mcp tool %s/%s: %w", serverID, toolName, err)
		}
	}

	encoded := argumentsJSON
	if args != nil {
		raw, err := json.Marshal(args)
		if err != nil {
			return "", fmt.Errorf("mcp tool %s/%s: encode arguments: %w", serverID, toolName, err)
		}
		encoded = string(raw)
	}

	res, err := client.CallTool(ctx, toolName, json.RawMessage(encoded))
	if err != nil {
		return "", err
	}
	if len(params) > 0 {
		if err := b.materializeDownload(ctx, client, params, args, &res); err != nil {
			return "", fmt.Errorf("mcp tool %s/%s: %w", serverID, toolName, err)
		}
	}
	return res, nil
}

// Close tears down all MCP clients.
func (b *McpBridge) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id := range b.specs {
		b.closeLocked(id)
	}
}
