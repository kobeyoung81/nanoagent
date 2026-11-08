package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanosec/internal/mcp"
	"gitee.com/kobeyoung81/nanosec/internal/tool"
)

// fakeFileServer is a minimal MCP server that also speaks the byte side
// channel: /mcp for JSON-RPC, /mcp/blobs for uploads and a raw path for
// downloads. It records what the tool call actually carried, which is the whole
// point of the extension.
type fakeFileServer struct {
	mu         sync.Mutex
	arguments  map[string]any
	uploaded   []byte
	uploadHits int
	rawBody    []byte
	rawHash    string
	callResult string
	maxUpload  int
}

func (f *fakeFileServer) maxUploadSize() int {
	if f.maxUpload > 0 {
		return f.maxUpload
	}
	return 16 << 20
}

func (f *fakeFileServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp/blobs", func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, 0, 1024)
		buf := make([]byte, 4096)
		for {
			n, err := r.Body.Read(buf)
			body = append(body, buf[:n]...)
			if err != nil {
				break
			}
		}
		f.mu.Lock()
		f.uploaded = body
		f.uploadHits++
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"upload_id": "abc", "uri": "nanoartifacts://upload/abc", "size_bytes": len(body),
		})
	})
	mux.HandleFunc("/mcp/artifacts/art-1/versions/1/raw", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		body := f.rawBody
		hash := f.rawHash
		f.mu.Unlock()
		w.Header().Set("X-Content-SHA256", hash)
		_, _ = w.Write(body)
	})
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if len(req.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any = map[string]any{}
		switch req.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": "2025-03-26",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "fake", "version": "1"},
			}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{
				{
					"name":        "artifact_publish",
					"description": "publish",
					"inputSchema": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"html_file": map[string]any{
								"type":   "string",
								"format": "uri",
								"x-mcp-file": map[string]any{
									"transferModes":  []string{"upload"},
									"uploadEndpoint": "/mcp/blobs",
									"maxSize":        f.maxUploadSize(),
								},
							},
						},
					},
				},
				{
					"name": "artifact_get",
					"inputSchema": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"download_to": map[string]any{
								"type":       "string",
								"x-mcp-file": map[string]any{"transferModes": []string{"download"}},
							},
						},
					},
				},
			}}
		case "tools/call":
			f.mu.Lock()
			f.arguments = req.Params.Arguments
			callResult := f.callResult
			f.mu.Unlock()
			if callResult == "" {
				callResult = `{"artifact_id":"art-1"}`
			}
			result = map[string]any{
				"content": []map[string]string{{"type": "text", "text": callResult}},
				"isError": false,
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	})
	return mux
}

func newFakeBridge(t *testing.T, workspace string) (*McpBridge, *fakeFileServer, []tool.Tool) {
	t.Helper()
	fake := &fakeFileServer{}
	server := httptest.NewServer(fake.handler())
	t.Cleanup(server.Close)

	bridge := NewMcpBridge("agent-7", workspace, "")
	tools := bridge.Sync(context.Background(), []nanoprotocol.McpServerDef{{
		ID:        "artifacts",
		Transport: "http",
		URL:       server.URL + "/mcp",
		Headers:   map[string]string{"Authorization": "Bearer shared"},
	}})
	return bridge, fake, tools
}

func toolByName(tools []tool.Tool, name string) *tool.McpTool {
	for _, candidate := range tools {
		if mcpTool, ok := candidate.(*tool.McpTool); ok && mcpTool.ToolName == name {
			return mcpTool
		}
	}
	return nil
}

func TestFileArgumentIsUploadedAndReplaced(t *testing.T) {
	workspace := t.TempDir()
	page := "<!doctype html><html><body>uploaded</body></html>"
	if err := os.WriteFile(filepath.Join(workspace, "page.html"), []byte(page), 0o644); err != nil {
		t.Fatalf("write page: %v", err)
	}

	_, fake, tools := newFakeBridge(t, workspace)
	publish := toolByName(tools, "artifact_publish")
	if publish == nil {
		t.Fatal("artifact_publish was not adapted")
	}

	// The provider must not see the extension: the model is told to pass a
	// string, not to interpret a runtime instruction.
	var schema map[string]any
	if err := json.Unmarshal(publish.RawParameters(), &schema); err != nil {
		t.Fatalf("decode schema: %v", err)
	}
	props := schema["properties"].(map[string]any)
	htmlFile := props["html_file"].(map[string]any)
	if _, has := htmlFile["x-mcp-file"]; has {
		t.Fatalf("x-mcp-file leaked into the provider schema: %v", htmlFile)
	}

	if _, err := publish.Execute(context.Background(), map[string]any{
		"html_file": "page.html",
		"title":     "page",
	}); err != nil {
		t.Fatalf("execute: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if string(fake.uploaded) != page {
		t.Fatalf("uploaded %q, want the file's bytes", fake.uploaded)
	}
	if got, _ := fake.arguments["html_file"].(string); got != "nanoartifacts://upload/abc" {
		t.Fatalf("tool argument html_file = %q, want the handle", got)
	}
}

func TestFileArgumentOutsideTheWorkspaceIsRefused(t *testing.T) {
	workspace := t.TempDir()
	// The file exists, one level up: the refusal must come from the boundary,
	// not from the file being missing.
	if err := os.WriteFile(filepath.Join(workspace, "..", "outside.html"), []byte("<html></html>"), 0o644); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	_, fake, tools := newFakeBridge(t, workspace)

	_, err := toolByName(tools, "artifact_publish").Execute(context.Background(), map[string]any{
		"html_file": "../outside.html",
	})
	if err == nil {
		t.Fatal("a path outside the workspace must be refused")
	}
	if !strings.Contains(err.Error(), "outside the allowed workspaces") {
		t.Fatalf("error = %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.uploadHits != 0 {
		t.Fatalf("the server saw %d uploads", fake.uploadHits)
	}
}

func TestFileArgumentOverTheLimitIsRefused(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "big.html"), []byte(strings.Repeat("x", 20)), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	fake := &fakeFileServer{maxUpload: 10}
	server := httptest.NewServer(fake.handler())
	t.Cleanup(server.Close)
	bridge := NewMcpBridge("agent-7", workspace, "")
	tools := bridge.Sync(context.Background(), []nanoprotocol.McpServerDef{{
		ID:        "artifacts",
		Transport: "http",
		URL:       server.URL + "/mcp",
	}})
	if len(tools) == 0 {
		t.Fatal("no tools adapted")
	}
	_, err := toolByName(tools, "artifact_publish").Execute(context.Background(), map[string]any{
		"html_file": "big.html",
	})
	if err == nil || !strings.Contains(err.Error(), "over the 10-byte limit") {
		t.Fatalf("error = %v", err)
	}
}

func TestFileArgumentThatDoesNotExistIsRefused(t *testing.T) {
	workspace := t.TempDir()
	_, _, tools := newFakeBridge(t, workspace)
	_, err := toolByName(tools, "artifact_publish").Execute(context.Background(), map[string]any{
		"html_file": "missing.html",
	})
	if err == nil || !strings.Contains(err.Error(), "cannot read") {
		t.Fatalf("error = %v", err)
	}
}

func TestDownloadIsWrittenToTheWorkspace(t *testing.T) {
	workspace := t.TempDir()
	page := "<html><body>downloaded</body></html>"
	sum := sha256.Sum256([]byte(page))

	_, fake, tools := newFakeBridge(t, workspace)
	fake.mu.Lock()
	fake.rawBody = []byte(page)
	fake.rawHash = hex.EncodeToString(sum[:])
	fake.callResult = fmt.Sprintf(`{"artifact_id":"art-1","download":{"artifact_id":"art-1","seq":1,"size_bytes":%d,"content_hash":%q,"raw_path":"/mcp/artifacts/art-1/versions/1/raw"}}`,
		len(page), hex.EncodeToString(sum[:]))
	fake.mu.Unlock()

	got, err := toolByName(tools, "artifact_get").Execute(context.Background(), map[string]any{
		"artifact_id": "art-1",
		"download_to": "nested/page.html",
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	// The body must be on disk, not in the result.
	if strings.Contains(got, "<html>") {
		t.Fatalf("result still carries the body: %s", got)
	}
	saved := filepath.Join(workspace, "nested", "page.html")
	onDisk, err := os.ReadFile(saved)
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	if string(onDisk) != page {
		t.Fatalf("saved %q", onDisk)
	}
	var payload struct {
		SavedTo     string `json:"saved_to"`
		SizeBytes   int64  `json:"size_bytes"`
		ContentHash string `json:"content_hash"`
	}
	if err := json.Unmarshal([]byte(got), &payload); err != nil {
		t.Fatalf("decode result: %v (%s)", err, got)
	}
	if payload.SavedTo != "nested/page.html" || payload.SizeBytes != int64(len(page)) {
		t.Fatalf("payload = %+v", payload)
	}
	if payload.ContentHash != hex.EncodeToString(sum[:]) {
		t.Fatalf("content_hash = %q", payload.ContentHash)
	}
}

func TestDownloadWithTheWrongHashIsDiscarded(t *testing.T) {
	workspace := t.TempDir()
	page := "<html><body>tampered</body></html>"

	_, fake, tools := newFakeBridge(t, workspace)
	fake.mu.Lock()
	fake.rawBody = []byte(page)
	fake.rawHash = "deadbeef"
	fake.callResult = `{"artifact_id":"art-1","download":{"seq":1,"size_bytes":30,"content_hash":"deadbeef","raw_path":"/mcp/artifacts/art-1/versions/1/raw"}}`
	fake.mu.Unlock()

	_, err := toolByName(tools, "artifact_get").Execute(context.Background(), map[string]any{
		"artifact_id": "art-1",
		"download_to": "page.html",
	})
	if err == nil || !strings.Contains(err.Error(), "hash") {
		t.Fatalf("error = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(workspace, "page.html")); !os.IsNotExist(statErr) {
		t.Fatalf("a mismatch must not leave a file behind: %v", statErr)
	}
}

// jsonOnlyClient is a transport with no byte side channel (an SSE or stdio
// server). A file argument must fail loudly rather than send a path the server
// cannot read.
type jsonOnlyClient struct{}

func (jsonOnlyClient) ListTools(context.Context) ([]mcp.Tool, error) { return nil, nil }
func (jsonOnlyClient) CallTool(context.Context, string, json.RawMessage) (string, error) {
	return "", nil
}
func (jsonOnlyClient) State() mcp.State { return mcp.StateConnected }
func (jsonOnlyClient) LastError() error { return nil }
func (jsonOnlyClient) Close() error     { return nil }

func TestFileArgumentNeedsABlobTransport(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "page.html"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	bridge := NewMcpBridge("agent-7", workspace, "")
	err := bridge.materializeUploads(context.Background(), jsonOnlyClient{},
		map[string]fileParam{"html_file": {Mode: "upload"}},
		map[string]any{"html_file": "page.html"})
	if err == nil || !strings.Contains(err.Error(), "cannot upload file arguments") {
		t.Fatalf("error = %v", err)
	}
}
