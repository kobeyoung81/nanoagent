package hypervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"gitee.com/kobeyoung81/nanosec/internal/gitsvc"
)

// newGitTestServer builds a hypervisor Server with a git service rooted at a
// temp dir and an initialized repo at <root>/work/proj.
func newGitTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	root := t.TempDir()
	server := NewServer(":0").SetDataRoot(root).SetGitService(gitsvc.New(root, 30*time.Second))
	repo := filepath.Join(root, "work", "proj")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo, "init", "-b", "main")
	gitCmd(t, repo, "config", "user.email", "t@example.com")
	gitCmd(t, repo, "config", "user.name", "Tester")
	return server, repo
}

func gitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out.String())
	}
	return out.String()
}

func gitReq(method, agentID, path string, body []byte) *http.Request {
	req := httptest.NewRequest(method, "/api/v1/git/"+agentID+path, bytes.NewReader(body))
	req.SetPathValue("agentID", agentID)
	return req
}

func TestGitListFromDataRoot(t *testing.T) {
	server, repo := newGitTestServer(t)
	if err := os.MkdirAll(filepath.Join(repo, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "readme.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	server.handleGitList(rec, gitReq(http.MethodGet, "agent-1", "/list?path=work/proj", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Path    string    `json:"path"`
		Entries []fsEntry `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Path != "work/proj" {
		t.Errorf("path = %q, want work/proj", resp.Path)
	}
	found := false
	for _, e := range resp.Entries {
		if e.Name == "readme.md" {
			found = true
		}
	}
	if !found {
		t.Errorf("readme.md not listed: %+v", resp.Entries)
	}
}

func TestGitListEscapesRejected(t *testing.T) {
	server, _ := newGitTestServer(t)
	rec := httptest.NewRecorder()
	server.handleGitList(rec, gitReq(http.MethodGet, "agent-1", "/list?path=../escape", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for traversal, got %d", rec.Code)
	}
}

func TestGitSuggestMatchesPaths(t *testing.T) {
	server, repo := newGitTestServer(t)
	if err := os.WriteFile(filepath.Join(repo, "agent-notes.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	server.handleGitSuggest(rec, gitReq(http.MethodGet, "agent-1", "/suggest?q=agent-notes", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("suggest: expected 200, got %d", rec.Code)
	}
	var resp struct {
		Matches []string `json:"matches"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Matches) == 0 {
		t.Fatal("expected at least one match")
	}
	if resp.Matches[0] != "work/proj/agent-notes.md" {
		t.Errorf("unexpected match %q", resp.Matches[0])
	}
}

func TestGitSuggestDirsOnly(t *testing.T) {
	server, repo := newGitTestServer(t)
	if err := os.MkdirAll(filepath.Join(repo, "agent-notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "agent-notes.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	server.handleGitSuggest(rec, gitReq(http.MethodGet, "agent-1", "/suggest?q=agent-notes&dirs=1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("suggest: expected 200, got %d", rec.Code)
	}
	var resp struct {
		Matches []string `json:"matches"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Matches) != 1 || resp.Matches[0] != "work/proj/agent-notes" {
		t.Fatalf("expected only the directory match, got %+v", resp.Matches)
	}
}

func TestGitBlobReadWrite(t *testing.T) {
	server, _ := newGitTestServer(t)
	rel := "work/proj/file.txt"

	put := httptest.NewRecorder()
	server.handleGitBlob(put, gitReq(http.MethodPut, "agent-1", "/blob?file="+rel, []byte("hello blob")))
	if put.Code != http.StatusOK {
		t.Fatalf("blob put: expected 200, got %d: %s", put.Code, put.Body.String())
	}

	get := httptest.NewRecorder()
	server.handleGitBlob(get, gitReq(http.MethodGet, "agent-1", "/blob?file="+rel, nil))
	if get.Code != http.StatusOK {
		t.Fatalf("blob get: expected 200, got %d", get.Code)
	}
	if get.Body.String() != "hello blob" {
		t.Errorf("blob content = %q, want hello blob", get.Body.String())
	}
}

func TestGitStatusStageCommitFlow(t *testing.T) {
	server, repo := newGitTestServer(t)

	// status on a fresh repo -> no repo status errors or not-a-repo?
	rec := httptest.NewRecorder()
	server.handleGitStatus(rec, gitReq(http.MethodGet, "agent-1", "/status?path=work/proj", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// create a file through the files API and commit via git handlers
	full := filepath.Join(repo, "hello.txt")
	if err := os.WriteFile(full, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	stRec := httptest.NewRecorder()
	server.handleGitStage(stRec, gitReq(http.MethodPost, "agent-1", "/stage", marshall(map[string]any{
		"path":  "work/proj",
		"files": []string{"hello.txt"},
	})))
	if stRec.Code != http.StatusOK {
		t.Fatalf("stage: expected 200, got %d: %s", stRec.Code, stRec.Body.String())
	}

	commitRec := httptest.NewRecorder()
	server.handleGitCommit(commitRec, gitReq(http.MethodPost, "agent-1", "/commit", marshall(map[string]any{
		"path":    "work/proj",
		"message": "add hello",
	})))
	if commitRec.Code != http.StatusOK {
		t.Fatalf("commit: expected 200, got %d: %s", commitRec.Code, commitRec.Body.String())
	}

	logRec := httptest.NewRecorder()
	server.handleGitLog(logRec, gitReq(http.MethodGet, "agent-1", "/log?path=work/proj&limit=10", nil))
	if logRec.Code != http.StatusOK {
		t.Fatalf("log: expected 200, got %d", logRec.Code)
	}
	var logResp struct {
		Commits []map[string]any `json:"commits"`
	}
	if err := json.Unmarshal(logRec.Body.Bytes(), &logResp); err != nil {
		t.Fatal(err)
	}
	if len(logResp.Commits) != 1 {
		t.Fatalf("expected 1 commit, got %d", len(logResp.Commits))
	}
}

func TestGitRepoInfoNotARepo(t *testing.T) {
	server, _ := newGitTestServer(t)
	rec := httptest.NewRecorder()
	server.handleGitRepo(rec, gitReq(http.MethodGet, "agent-1", "/repo?path=work", nil))
	// "Not a repository" is an ordinary state: 200 with in_repo:false, so the
	// UI can render its clone/empty state without a failed request on every poll.
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for non-repo path, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		InRepo *bool `json:"in_repo"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.InRepo == nil || *body.InRepo {
		t.Fatalf("expected in_repo:false, got %s", rec.Body.String())
	}
}

func TestGitStatusNotARepo(t *testing.T) {
	server, _ := newGitTestServer(t)
	rec := httptest.NewRecorder()
	server.handleGitStatus(rec, gitReq(http.MethodGet, "agent-1", "/status?path=work", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for non-repo status, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		InRepo *bool `json:"in_repo"`
		Staged []any `json:"staged"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.InRepo == nil || *body.InRepo {
		t.Fatalf("expected in_repo:false, got %s", rec.Body.String())
	}
	if body.Staged == nil {
		t.Fatalf("expected non-nil staged array, got %s", rec.Body.String())
	}
}

func marshall(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func TestGitDisabled(t *testing.T) {
	server := NewServer(":0").SetDataRoot(t.TempDir()) // no git service wired
	rec := httptest.NewRecorder()
	server.handleGitStatus(rec, gitReq(http.MethodGet, "agent-1", "/status?path=work", nil))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501 when git disabled, got %d", rec.Code)
	}
}

// TestGitRoutesEndToEnd starts the real hypervisor HTTP bridge and exercises
// the registered git routes over the network (list, status, diff, log, commit).
func TestGitRoutesEndToEnd(t *testing.T) {
	root := t.TempDir()
	server := NewServer("127.0.0.1:0").SetDataRoot(root).SetGitService(gitsvc.New(root, 30*time.Second))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = server.Start(ctx)
	}()
	// Wait for the listener to come up.
	var base string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if addr := server.Addr(); addr != "" && addr != "127.0.0.1:0" {
			base = "http://" + addr
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if base == "" {
		t.Fatal("hypervisor bridge did not bind an address")
	}
	t.Log("bridge base", base)

	repo := filepath.Join(root, "work", "proj")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo, "init", "-b", "main")
	gitCmd(t, repo, "config", "user.email", "t@example.com")
	gitCmd(t, repo, "config", "user.name", "Tester")
	if err := os.WriteFile(filepath.Join(repo, "hello.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	call := func(method, path string, body []byte) *http.Response {
		url := base + path
		req, err := http.NewRequest(method, url, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		return resp
	}

	// 1. status shows the untracked file.
	resp := call(http.MethodGet, "/api/v1/git/agent-1/status?path=work/proj", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: expected 200, got %d", resp.StatusCode)
	}
	var status struct {
		Untracked []map[string]any `json:"untracked"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if len(status.Untracked) != 1 {
		t.Fatalf("expected 1 untracked file, got %+v", status.Untracked)
	}

	// 2. stage + commit.
	resp = call(http.MethodPost, "/api/v1/git/agent-1/stage", marshall(map[string]any{"path": "work/proj", "files": []string{"hello.txt"}}))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stage: expected 200, got %d", resp.StatusCode)
	}
	resp = call(http.MethodPost, "/api/v1/git/agent-1/commit", marshall(map[string]any{"path": "work/proj", "message": "init"}))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("commit: expected 200, got %d", resp.StatusCode)
	}

	// 3. log shows the commit.
	resp = call(http.MethodGet, "/api/v1/git/agent-1/log?path=work/proj&limit=10", nil)
	defer resp.Body.Close()
	var log struct {
		Commits []map[string]any `json:"commits"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&log); err != nil {
		t.Fatal(err)
	}
	if len(log.Commits) != 1 || log.Commits[0]["subject"] != "init" {
		t.Fatalf("unexpected log: %+v", log.Commits)
	}

	// 4. list rooted at data root lists the repo folder.
	resp = call(http.MethodGet, "/api/v1/git/agent-1/list?path=work", nil)
	defer resp.Body.Close()
	var listing struct {
		Entries []map[string]any `json:"entries"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Entries) != 1 || listing.Entries[0]["name"] != "proj" {
		t.Fatalf("unexpected listing: %+v", listing.Entries)
	}
}

// testAddr returns the host:port the test server bound to.
func testAddr(t *testing.T, s *Server) string {
	t.Helper()
	return s.Addr()
}

// TestGitInfoReturnsCanonicalDataRoot checks the info endpoint reports the data
// root as an absolute, canonical path rather than just its base name. The browse
// tree is rooted at the data root while git reports paths relative to each
// repository root, so the frontend rebases every change path with the data root
// as a prefix — a bare directory name never matched a repo_root, which left the
// file tree without a single change marker.
func TestGitInfoReturnsCanonicalDataRoot(t *testing.T) {
	server, repo := newGitTestServer(t)

	rec := httptest.NewRecorder()
	server.handleGitInfo(rec, gitReq(http.MethodGet, "agent-1", "/info", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("info: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		AgentHome string `json:"agent_home"`
		DataRoot  string `json:"data_root"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(resp.DataRoot) {
		t.Fatalf("data_root = %q, want an absolute path", resp.DataRoot)
	}
	// repo_root values come out of git (symlinks already resolved), so the
	// reported data root has to be canonical for the two to be comparable.
	canonRepo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(filepath.FromSlash(resp.DataRoot), canonRepo)
	if err != nil {
		t.Fatalf("data_root %q is unrelated to repo %q: %v", resp.DataRoot, canonRepo, err)
	}
	if filepath.ToSlash(rel) != "work/proj" {
		t.Fatalf("repo relative to data_root = %q, want work/proj (data_root %q)", rel, resp.DataRoot)
	}
	if resp.AgentHome != "agents/agent-1" {
		t.Errorf("agent_home = %q, want agents/agent-1", resp.AgentHome)
	}
}
