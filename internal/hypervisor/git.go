package hypervisor

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gitee.com/kobeyoung81/nanosec/internal/gitsvc"
)

// maxBlobSize caps read/write of text files via the git browse blob endpoint.
const maxBlobSize = 16 << 20

// resolveGitService returns the configured git service or writes a 501 when
// the git browser is disabled.
func (s *Server) gitSvc(w http.ResponseWriter) *gitsvc.Service {
	if s.git == nil || s.dataRoot == "" {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "git browser disabled"})
		return nil
	}
	return s.git
}

func (s *Server) handleGitInfo(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("agentID")
	svc := s.gitSvc(w)
	if svc == nil {
		return
	}
	home := s.homeResolver(agentID)
	// data_root is the absolute, canonical data root — not just its base name.
	// The browse tree is rooted there while git reports paths relative to each
	// repository root, so the frontend rebases every change path with the data
	// root as the prefix; a bare directory name could never match a repo_root.
	root, err := svc.ResolvePath("")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user_home":  "",
		"agent_home": filepath.ToSlash(filepath.Join("agents", home)),
		"data_root":  filepath.ToSlash(root),
	})
}

// handleGitList lists a directory rooted at the data root (user home).
func (s *Server) handleGitList(w http.ResponseWriter, r *http.Request) {
	svc := s.gitSvc(w)
	if svc == nil {
		return
	}
	rel := r.URL.Query().Get("path")
	resolved, err := svc.ResolvePath(rel)
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}
	// Avoid descending into a running repository's internals unless explicitly
	// requested (browsing inside .git is allowed but flagged).
	entries, err := readDirEntries(resolved, defaultRuntimeEntries)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": rel, "entries": entries})
}

// handleGitSuggest returns up to limit data-root-relative paths whose basename
// contains q (case-insensitive). Heavy directories are pruned. When dirs=1 only
// directories are returned, which is what the file-tree root navigator wants.
func (s *Server) handleGitSuggest(w http.ResponseWriter, r *http.Request) {
	svc := s.gitSvc(w)
	if svc == nil {
		return
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	dirsOnly := r.URL.Query().Get("dirs") == "1"
	limit := 20
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 50 {
			limit = n
		}
	}
	root, err := svc.ResolvePath("")
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}

	prune := map[string]bool{
		".git": true, "node_modules": true, ".cache": true,
		".venv": true, "venv": true, "target": true, "dist": true,
		"__pycache__": true, ".npm": true, "media": true,
	}

	matches := make([]string, 0, limit)
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if len(matches) >= limit || depth > 8 {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if len(matches) >= limit {
				return
			}
			name := e.Name()
			if prune[name] {
				continue
			}
			rel, rerr := filepath.Rel(root, filepath.Join(dir, name))
			if rerr != nil {
				continue
			}
			rel = filepath.ToSlash(rel)
			if (q == "" || strings.Contains(strings.ToLower(name), q)) && (!dirsOnly || e.IsDir()) {
				matches = append(matches, rel)
			}
			if e.IsDir() {
				walk(filepath.Join(dir, name), depth+1)
			}
		}
	}
	walk(root, 0)
	writeJSON(w, http.StatusOK, map[string]any{"matches": matches})
}

// handleGitBlob reads (GET) or writes (PUT) a text file at a data-root-relative
// path, for the plugin's own viewer/editor.
func (s *Server) handleGitBlob(w http.ResponseWriter, r *http.Request) {
	svc := s.gitSvc(w)
	if svc == nil {
		return
	}
	rel := r.URL.Query().Get("file")
	if rel == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "file is required"})
		return
	}
	resolved, err := svc.ResolvePath(rel)
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}
	if r.Method == http.MethodPut {
		s.writeBlob(w, r, resolved)
		return
	}
	s.readBlob(w, resolved)
}

func (s *Server) readBlob(w http.ResponseWriter, resolved string) {
	info, err := os.Stat(resolved)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "file not found"})
		return
	}
	if info.IsDir() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path is a directory"})
		return
	}
	if info.Size() > maxBlobSize {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{
			"error": "file exceeds the viewable size limit",
		})
		return
	}
	content, err := os.ReadFile(resolved)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	contentType := http.DetectContentType(content)
	if contentType == "text/plain; charset=utf-8" {
		// Let the browser infer charset from the file.
		contentType = ""
	}
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(content)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

func (s *Server) writeBlob(w http.ResponseWriter, r *http.Request, resolved string) {
	if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBlobSize+1))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if len(body) > maxBlobSize {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "file exceeds max size"})
		return
	}
	if err := os.WriteFile(resolved, body, 0o644); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// gitPath extracts the request's git op (repo/status/diff/...) from the URL.
func gitPath(r *http.Request) string {
	return r.URL.Query().Get("path")
}

// ---- Git read operations ----

func (s *Server) handleGitRepo(w http.ResponseWriter, r *http.Request) {
	svc := s.gitSvc(w)
	if svc == nil {
		return
	}
	info, err := svc.RepoInfo(r.Context(), gitPath(r))
	if err != nil {
		if errors.Is(err, gitsvc.ErrNotARepository) {
			// "Not a repository" is an ordinary state, not a bad request. A 200
			// with in_repo:false lets the UI show its clone/empty state without
			// the browser logging a failed request on every poll.
			writeJSON(w, http.StatusOK, map[string]any{"in_repo": false})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) handleGitStatus(w http.ResponseWriter, r *http.Request) {
	svc := s.gitSvc(w)
	if svc == nil {
		return
	}
	st, err := svc.Status(r.Context(), gitPath(r))
	if err != nil {
		if errors.Is(err, gitsvc.ErrNotARepository) {
			writeJSON(w, http.StatusOK, map[string]any{
				"in_repo":    false,
				"staged":     []any{},
				"unstaged":   []any{},
				"untracked":  []any{},
				"conflicted": []any{},
			})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// handleGitRepos lists every repository at or under a path that is itself a
// repository root — including nested repositories such as submodules — together
// with each repository's branch state and working-tree changes. A path outside
// any repository, or a plain directory inside one, yields an empty list with
// 200, the same "ordinary state" contract handleGitStatus uses.
func (s *Server) handleGitRepos(w http.ResponseWriter, r *http.Request) {
	svc := s.gitSvc(w)
	if svc == nil {
		return
	}
	repos, err := svc.Repos(r.Context(), gitPath(r))
	if err != nil {
		if errors.Is(err, gitsvc.ErrNotARepository) {
			writeJSON(w, http.StatusOK, map[string]any{"repos": []gitsvc.RepoSummary{}})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"repos": repos})
}

func (s *Server) handleGitDiff(w http.ResponseWriter, r *http.Request) {
	svc := s.gitSvc(w)
	if svc == nil {
		return
	}
	file := r.URL.Query().Get("file")
	staged := r.URL.Query().Get("staged") == "true" || r.URL.Query().Get("staged") == "1"
	ref := r.URL.Query().Get("ref")
	diff, err := svc.Diff(r.Context(), gitPath(r), file, staged, ref)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"diff": diff})
}

func (s *Server) handleGitDiffStat(w http.ResponseWriter, r *http.Request) {
	svc := s.gitSvc(w)
	if svc == nil {
		return
	}
	staged := r.URL.Query().Get("staged") == "true" || r.URL.Query().Get("staged") == "1"
	ref := r.URL.Query().Get("ref")
	stats, err := svc.DiffStat(r.Context(), gitPath(r), staged, ref)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"stat": stats})
}

func (s *Server) handleGitLog(w http.ResponseWriter, r *http.Request) {
	svc := s.gitSvc(w)
	if svc == nil {
		return
	}
	file := r.URL.Query().Get("file")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	skip, _ := strconv.Atoi(r.URL.Query().Get("skip"))
	page, err := svc.Log(r.Context(), gitPath(r), file, limit, skip)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) handleGitCommitDetail(w http.ResponseWriter, r *http.Request) {
	svc := s.gitSvc(w)
	if svc == nil {
		return
	}
	sha := r.URL.Query().Get("sha")
	detail, err := svc.CommitDetail(r.Context(), gitPath(r), sha)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) handleGitBranches(w http.ResponseWriter, r *http.Request) {
	svc := s.gitSvc(w)
	if svc == nil {
		return
	}
	branches, err := svc.Branches(r.Context(), gitPath(r))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if branches == nil {
		branches = []gitsvc.Branch{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"branches": branches})
}

func (s *Server) handleGitRemotes(w http.ResponseWriter, r *http.Request) {
	svc := s.gitSvc(w)
	if svc == nil {
		return
	}
	remotes, err := svc.Remotes(r.Context(), gitPath(r))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if remotes == nil {
		remotes = []gitsvc.Remote{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"remotes": remotes})
}

func (s *Server) handleGitTags(w http.ResponseWriter, r *http.Request) {
	svc := s.gitSvc(w)
	if svc == nil {
		return
	}
	tags, err := svc.Tags(r.Context(), gitPath(r))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if tags == nil {
		tags = []gitsvc.Tag{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tags": tags})
}

func (s *Server) handleGitStashes(w http.ResponseWriter, r *http.Request) {
	svc := s.gitSvc(w)
	if svc == nil {
		return
	}
	stashes, err := svc.Stashes(r.Context(), gitPath(r))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if stashes == nil {
		stashes = []gitsvc.Stash{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"stashes": stashes})
}

func (s *Server) handleGitShow(w http.ResponseWriter, r *http.Request) {
	svc := s.gitSvc(w)
	if svc == nil {
		return
	}
	ref := r.URL.Query().Get("ref")
	file := r.URL.Query().Get("file")
	content, err := svc.ShowFile(r.Context(), gitPath(r), ref, file)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

// ---- Git write operations (JSON bodies) ----

// decodeBody decodes a JSON request body into v (capping body size).
func decodeBody(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}

func (s *Server) handleGitStage(w http.ResponseWriter, r *http.Request) {
	svc := s.gitSvc(w)
	if svc == nil {
		return
	}
	var req struct {
		Path  string   `json:"path"`
		Files []string `json:"files"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if err := svc.StageFiles(r.Context(), req.Path, req.Files); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleGitUnstage(w http.ResponseWriter, r *http.Request) {
	svc := s.gitSvc(w)
	if svc == nil {
		return
	}
	var req struct {
		Path  string   `json:"path"`
		Files []string `json:"files"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if err := svc.UnstageFiles(r.Context(), req.Path, req.Files); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleGitDiscard(w http.ResponseWriter, r *http.Request) {
	svc := s.gitSvc(w)
	if svc == nil {
		return
	}
	var req struct {
		Path  string   `json:"path"`
		Files []string `json:"files"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if err := svc.DiscardFiles(r.Context(), req.Path, req.Files); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleGitCommit(w http.ResponseWriter, r *http.Request) {
	svc := s.gitSvc(w)
	if svc == nil {
		return
	}
	var req struct {
		Path    string `json:"path"`
		Message string `json:"message"`
		Amend   bool   `json:"amend"`
		SignOff bool   `json:"signoff"`
		All     bool   `json:"all"`
		Empty   bool   `json:"allow_empty"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if err := svc.Commit(r.Context(), req.Path, gitsvc.CommitOpts{
		Message: req.Message, Amend: req.Amend, SignOff: req.SignOff,
		All: req.All, Empty: req.Empty,
	}); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleGitBranchCreate(w http.ResponseWriter, r *http.Request) {
	svc := s.gitSvc(w)
	if svc == nil {
		return
	}
	var req struct {
		Path       string `json:"path"`
		Name       string `json:"name"`
		StartPoint string `json:"start_point"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if err := svc.CreateBranch(r.Context(), req.Path, req.Name, req.StartPoint); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleGitCheckout(w http.ResponseWriter, r *http.Request) {
	svc := s.gitSvc(w)
	if svc == nil {
		return
	}
	var req struct {
		Path   string `json:"path"`
		Ref    string `json:"ref"`
		Create bool   `json:"create"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if err := svc.Checkout(r.Context(), req.Path, req.Ref, req.Create); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleGitBranchDelete(w http.ResponseWriter, r *http.Request) {
	svc := s.gitSvc(w)
	if svc == nil {
		return
	}
	name := r.URL.Query().Get("name")
	force := r.URL.Query().Get("force") == "true" || r.URL.Query().Get("force") == "1"
	if err := svc.DeleteBranch(r.Context(), gitPath(r), name, force); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleGitRemote(w http.ResponseWriter, r *http.Request) {
	svc := s.gitSvc(w)
	if svc == nil {
		return
	}
	var req struct {
		Action string `json:"action"` // add | remove
		Path   string `json:"path"`
		Name   string `json:"name"`
		URL    string `json:"url"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	var err error
	switch req.Action {
	case "add":
		err = svc.AddRemote(r.Context(), req.Path, req.Name, req.URL)
	case "remove":
		err = svc.RemoveRemote(r.Context(), req.Path, req.Name)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "action must be add or remove"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleGitFetch(w http.ResponseWriter, r *http.Request) {
	svc := s.gitSvc(w)
	if svc == nil {
		return
	}
	var req struct {
		Path   string `json:"path"`
		Remote string `json:"remote"`
		Prune  bool   `json:"prune"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if err := svc.Fetch(r.Context(), req.Path, req.Remote, req.Prune); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleGitPush(w http.ResponseWriter, r *http.Request) {
	svc := s.gitSvc(w)
	if svc == nil {
		return
	}
	var req struct {
		Path        string `json:"path"`
		Remote      string `json:"remote"`
		Branch      string `json:"branch"`
		SetUpstream bool   `json:"set_upstream"`
		Force       bool   `json:"force"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if err := svc.Push(r.Context(), req.Path, gitsvc.PushOpts{
		Remote: req.Remote, Branch: req.Branch, SetUpstream: req.SetUpstream, Force: req.Force,
	}); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleGitPull(w http.ResponseWriter, r *http.Request) {
	svc := s.gitSvc(w)
	if svc == nil {
		return
	}
	var req struct {
		Path   string `json:"path"`
		Remote string `json:"remote"`
		Branch string `json:"branch"`
		Rebase bool   `json:"rebase"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if err := svc.Pull(r.Context(), req.Path, req.Remote, req.Branch, req.Rebase); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleGitClone(w http.ResponseWriter, r *http.Request) {
	svc := s.gitSvc(w)
	if svc == nil {
		return
	}
	var req struct {
		URL string `json:"url"`
		Dir string `json:"dir"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	dest, err := svc.Clone(r.Context(), gitsvc.CloneOpts{URL: req.URL, Dir: req.Dir})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "dest": dest})
}

func (s *Server) handleGitMerge(w http.ResponseWriter, r *http.Request) {
	svc := s.gitSvc(w)
	if svc == nil {
		return
	}
	var req struct {
		Path    string `json:"path"`
		Ref     string `json:"ref"`
		Message string `json:"message"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if err := svc.Merge(r.Context(), req.Path, req.Ref, req.Message); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleGitRebase(w http.ResponseWriter, r *http.Request) {
	svc := s.gitSvc(w)
	if svc == nil {
		return
	}
	var req struct {
		Path string `json:"path"`
		Ref  string `json:"ref"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if err := svc.Rebase(r.Context(), req.Path, req.Ref); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleGitAbort(w http.ResponseWriter, r *http.Request) {
	svc := s.gitSvc(w)
	if svc == nil {
		return
	}
	var req struct {
		Path string `json:"path"`
		Kind string `json:"kind"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if err := svc.AbortMergeRebase(r.Context(), req.Path, req.Kind); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleGitResolve(w http.ResponseWriter, r *http.Request) {
	svc := s.gitSvc(w)
	if svc == nil {
		return
	}
	var req struct {
		Path     string `json:"path"`
		File     string `json:"file"`
		Strategy string `json:"strategy"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if err := svc.Resolve(r.Context(), req.Path, gitsvc.ResolveOpts{File: req.File, Strategy: req.Strategy}); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleGitTag(w http.ResponseWriter, r *http.Request) {
	svc := s.gitSvc(w)
	if svc == nil {
		return
	}
	var req struct {
		Action  string `json:"action"` // create | delete
		Path    string `json:"path"`
		Name    string `json:"name"`
		Ref     string `json:"ref"`
		Message string `json:"message"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	var err error
	switch req.Action {
	case "create":
		err = svc.CreateTag(r.Context(), req.Path, req.Name, req.Ref, req.Message)
	case "delete":
		err = svc.DeleteTag(r.Context(), req.Path, req.Name)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "action must be create or delete"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleGitStash(w http.ResponseWriter, r *http.Request) {
	svc := s.gitSvc(w)
	if svc == nil {
		return
	}
	var req struct {
		Path    string `json:"path"`
		Action  string `json:"action"`
		Message string `json:"message"`
		Index   int    `json:"index"`
		Keep    bool   `json:"keep"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if err := svc.Stash(r.Context(), req.Path, gitsvc.StashOpts{
		Action: req.Action, Message: req.Message, Index: req.Index, Keep: req.Keep,
	}); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
