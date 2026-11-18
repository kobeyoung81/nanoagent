package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"gitee.com/kobeyoung81/nanoprotocol"
)

// TestMcpBridgeInjectsAgentIdentity is the contract nanoartifacts relies on:
// every MCP request carries the agent's id, and the catalog's own headers are
// preserved alongside it. Without the header the library cannot attribute a
// publish; a test on the server side alone cannot prove the client sends it.
func TestMcpBridgeInjectsAgentIdentity(t *testing.T) {
	var (
		mu       sync.Mutex
		requests int
		agentIDs []string
		auths    []string
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		agentIDs = append(agentIDs, r.Header.Get("X-Nanowork-Agent-ID"))
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()

		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if len(req.ID) == 0 {
			// Notification: no response body.
			w.WriteHeader(http.StatusAccepted)
			return
		}
		result := any(map[string]any{})
		switch req.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": "2025-03-26",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "fake", "version": "1"},
			}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{{"name": "artifact_publish"}}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	defer server.Close()

	bridge := NewMcpBridge("agent-42", "", "")
	tools := bridge.Sync(context.Background(), []nanoprotocol.McpServerDef{{
		ID:        "artifacts",
		Transport: "http",
		URL:       server.URL,
		Headers:   map[string]string{"Authorization": "Bearer shared"},
	}})
	if len(tools) != 1 {
		t.Fatalf("got %d tools, want 1", len(tools))
	}

	mu.Lock()
	defer mu.Unlock()
	if requests == 0 {
		t.Fatal("the server saw no requests")
	}
	for i, id := range agentIDs {
		if id != "agent-42" {
			t.Fatalf("request %d carried agent id %q", i, id)
		}
	}
	for i, auth := range auths {
		if auth != "Bearer shared" {
			t.Fatalf("request %d carried authorization %q", i, auth)
		}
	}
}

// A tool can be added to a running server without its URL changing. The bridge
// must ask again on every reconcile, otherwise a deployed tool never reaches a
// connected agent until it restarts.
func TestMcpBridgeRelistsToolsOnReconcile(t *testing.T) {
	var (
		mu      sync.Mutex
		listing int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if len(req.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		result := any(map[string]any{})
		switch req.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": "2025-03-26",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "fake", "version": "1"},
			}
		case "tools/list":
			mu.Lock()
			listing++
			round := listing
			mu.Unlock()
			tools := []map[string]any{{"name": "artifact_publish"}}
			if round > 1 {
				tools = append(tools, map[string]any{"name": "folder_create"})
			}
			result = map[string]any{"tools": tools}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	defer server.Close()

	defs := []nanoprotocol.McpServerDef{{
		ID:        "artifacts",
		Transport: "http",
		URL:       server.URL,
		Headers:   map[string]string{"Authorization": "Bearer shared"},
	}}
	bridge := NewMcpBridge("agent-42", "", "")
	if tools := bridge.Sync(context.Background(), defs); len(tools) != 1 {
		t.Fatalf("first sync returned %d tools, want 1", len(tools))
	}
	if tools := bridge.Sync(context.Background(), defs); len(tools) != 2 {
		t.Fatalf("second sync returned %d tools, want 2 (the list was not refreshed)", len(tools))
	}
}
