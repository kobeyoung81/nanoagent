// Package mcp implements a minimal Model Context Protocol client using only
// the standard library. It speaks JSON-RPC 2.0 over two remote transports:
//
//   - "http": a remote streamable-HTTP server (JSON-RPC over POST; SSE and
//     single JSON responses are both accepted);
//   - "sse":  a legacy HTTP+SSE server (GET stream announces a POST endpoint,
//     responses arrive back on the stream).
//
// The protocol surface is deliberately small: initialize, tools/list and
// tools/call. Tool schemas are passed through verbatim so the LLM sees the
// server's native input schema.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
)

// Protocol versions we are willing to speak, newest first.
var supportedProtocolVersions = []string{"2025-03-26", "2024-11-05", "2024-10-07"}

// State is the lifecycle state of one MCP connection.
type State string

const (
	StateDisconnected State = "disconnected"
	StateConnecting   State = "connecting"
	StateConnected    State = "connected"
	StateFailed       State = "failed"
)

// Server describes an MCP server catalog entry.
type Server struct {
	ID        string
	Transport string            // "http" | "sse"
	URL       string            // http / sse base url
	Headers   map[string]string // http auth/headers
}

// Tool is an MCP tool exposed by a server.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

// BlobRef is a body a server accepted outside the JSON-RPC channel.
type BlobRef struct {
	URI       string `json:"uri"`
	UploadID  string `json:"upload_id"`
	SizeBytes int64  `json:"size_bytes"`
}

// BlobUploader is the side channel for file arguments.
//
// JSON-RPC carries JSON values, so a tool argument can only be a path when both
// ends agree on a byte path beside it. A server declares that agreement per
// property (x-mcp-file in the input schema); a transport implements this
// interface to honour it. Without one the runtime refuses the call rather than
// sending a path the server cannot read.
type BlobUploader interface {
	// UploadBlob sends the bytes to endpoint (server-relative, e.g. /mcp/blobs)
	// and returns the handle the tool argument should carry.
	UploadBlob(ctx context.Context, body io.Reader, size int64, endpoint string) (*BlobRef, error)
	// OpenBlob streams a server-relative path, which is what a download
	// descriptor's raw_path names. The caller closes it.
	OpenBlob(ctx context.Context, path string) (io.ReadCloser, int64, error)
}

// Client is a connected MCP session for one server.
type Client interface {
	// ListTools returns the tools the server exposes.
	ListTools(ctx context.Context) ([]Tool, error)
	// CallTool invokes a tool and returns its textual result (or an error,
	// including server-reported { isError: true } outcomes).
	CallTool(ctx context.Context, toolName string, arguments json.RawMessage) (string, error)
	// State reports the current connection lifecycle state.
	State() State
	// LastError reports the most recent connection/protocol error, if any.
	LastError() error
	// Close tears down the session.
	Close() error
}

var clientSeq atomic.Int64

func nextID() int64 {
	return clientSeq.Add(1)
}

// NewClient creates a client for the given server description.
func NewClient(server Server) Client {
	switch server.Transport {
	case "http":
		return &httpClient{server: server, state: StateDisconnected}
	case "sse":
		return &sseClient{server: server, state: StateDisconnected, pending: map[int64]chan jsonrpcResponse{}}
	default:
		return &unsupportedClient{server: server}
	}
}

// unsupportedClient reports a failure for any transport other than http/sse.
type unsupportedClient struct {
	server Server
}

func (c *unsupportedClient) err() error {
	return fmt.Errorf("mcp %s: unsupported transport %q", c.server.ID, c.server.Transport)
}

func (c *unsupportedClient) ListTools(context.Context) ([]Tool, error) { return nil, c.err() }

func (c *unsupportedClient) CallTool(context.Context, string, json.RawMessage) (string, error) {
	return "", c.err()
}

func (c *unsupportedClient) State() State     { return StateFailed }
func (c *unsupportedClient) LastError() error { return c.err() }
func (c *unsupportedClient) Close() error     { return nil }

// sanitizeToolName produces a provider-safe function name (letters, digits,
// '_', '-') limited to 64 chars, preserving readability.
func sanitizeToolName(name string) string {
	name = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			return r
		default:
			return '_'
		}
	}, name)
	if name == "" {
		return "tool"
	}
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}

// FullName builds the llm-facing function name mcp__<server>__<tool>, capped
// at the provider's 64-character function-name limit.
func FullName(serverID, toolName string) string {
	s := sanitizeToolName("mcp__" + serverID + "__" + toolName)
	if len(s) <= 64 {
		return s
	}
	s = s[:64]
	return strings.TrimRight(s, "_-")
}

// ======== JSON-RPC 2.0 wire types ========

type jsonrpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type jsonrpcNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type jsonrpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// callToolResult mirrors a tools/call response for the merge/extraction step.
type callToolResult struct {
	Content           []map[string]any `json:"content"`
	IsError           bool             `json:"isError,omitempty"`
	StructuredContent any              `json:"structuredContent,omitempty"`
}

// toString extracts joinable text from a tools/call result.
func renderCallResult(result json.RawMessage) (string, bool, error) {
	var ctr callToolResult
	if err := json.Unmarshal(result, &ctr); err != nil {
		return string(result), false, nil
	}
	var b strings.Builder
	for _, part := range ctr.Content {
		typ, _ := part["type"].(string)
		switch typ {
		case "text":
			if text, ok := part["text"].(string); ok {
				b.WriteString(text)
			}
		case "resource":
			if uri, ok := part["uri"].(string); ok {
				b.WriteString(uri)
				if text, ok := part["text"].(string); ok {
					b.WriteString("\n")
					b.WriteString(text)
				}
			}
		default:
			data, _ := json.Marshal(part)
			b.Write(data)
		}
		b.WriteString("\n")
	}
	if b.Len() == 0 {
		return "(no output)", ctr.IsError, nil
	}
	return strings.TrimSpace(b.String()), ctr.IsError, nil
}
