package hypervisor

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitee.com/kobeyoung81/nanosec/internal/gitsvc"
)

// TestGitReposRoute checks the multi-repository endpoint returns the enclosing
// repository (with its changes) as a flat list.
func TestGitReposRoute(t *testing.T) {
	server, repo := newGitTestServer(t)
	if err := os.WriteFile(filepath.Join(repo, "new.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	server.handleGitRepos(rec, gitReq(http.MethodGet, "agent-1", "/repos?path=work/proj", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("repos: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Repos []gitsvc.RepoSummary `json:"repos"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Repos) != 1 {
		t.Fatalf("got %d repos, want 1: %+v", len(resp.Repos), resp.Repos)
	}
	got := resp.Repos[0]
	if got.Name != "proj" || got.RelPath != "work/proj" {
		t.Errorf("repo identity = %q / %q, want proj / work/proj", got.Name, got.RelPath)
	}
	if len(got.Untracked) != 1 {
		t.Errorf("untracked = %d, want 1", len(got.Untracked))
	}
	if got.Staged == nil || got.Unstaged == nil || got.Conflicted == nil {
		t.Errorf("change slices must be non-nil: %+v", got)
	}
}

// TestGitReposRouteEmptyDirectory checks a directory holding no repository
// returns an empty list with 200 (not an error, not null).
func TestGitReposRouteEmptyDirectory(t *testing.T) {
	server, repo := newGitTestServer(t)
	root := filepath.Dir(filepath.Dir(repo))
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	server.handleGitRepos(rec, gitReq(http.MethodGet, "agent-1", "/repos?path=empty", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("repos: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"repos":[]}` {
		t.Fatalf("body = %s, want {\"repos\":[]}", got)
	}
}

// TestGitReposRoutePathEscape checks confinement is still enforced.
func TestGitReposRoutePathEscape(t *testing.T) {
	server, _ := newGitTestServer(t)

	rec := httptest.NewRecorder()
	server.handleGitRepos(rec, gitReq(http.MethodGet, "agent-1", "/repos?path=../../etc", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an escaping path, got %d: %s", rec.Code, rec.Body.String())
	}
}
