package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFullName_SanitizesAndCaps(t *testing.T) {
	name := FullName("git hub-srv", "read file!")
	if name != "mcp__git_hub-srv__read_file_" {
		t.Fatalf("unexpected name %q", name)
	}
	long := FullName(strings.Repeat("s", 100), strings.Repeat("t", 100))
	if len(long) > 64 {
		t.Fatalf("name too long: %d", len(long))
	}
}

// fixtureHTTPServer implements the same minimal MCP over HTTP.
func fixtureHTTPServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int64          `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"protocolVersion": "2025-03-26", "capabilities": map[string]any{"tools": map[string]any{}},
			}})
		case "tools/list":
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"tools": []map[string]any{{"name": "http_tool", "description": "http", "inputSchema": map[string]any{"type": "object"}}},
			}})
		case "tools/call":
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"content": []map[string]any{{"type": "text", "text": "http result"}},
			}})
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
}

func TestHTTPClient_ListAndCall(t *testing.T) {
	srv := fixtureHTTPServer()
	defer srv.Close()

	c := NewClient(Server{ID: "remote", Transport: "http", URL: srv.URL})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	tools, err := c.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "http_tool" {
		t.Fatalf("unexpected tools: %+v", tools)
	}
	res, err := c.CallTool(ctx, "http_tool", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !strings.Contains(res, "http result") {
		t.Fatalf("unexpected result %q", res)
	}
}

func TestUnsupportedTransport(t *testing.T) {
	c := NewClient(Server{ID: "x", Transport: "stdio"})
	if _, err := c.ListTools(context.Background()); err == nil {
		t.Fatal("expected an unsupported transport error")
	}
	if c.State() != StateFailed {
		t.Fatalf("state = %q, want failed", c.State())
	}
}
