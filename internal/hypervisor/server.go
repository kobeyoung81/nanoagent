package hypervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"gitee.com/kobeyoung81/nanosec/internal/gitsvc"
	"gitee.com/kobeyoung81/nanosec/internal/workspace"
)

// maxFileUploadSize caps file uploads (10 MiB).
const maxFileUploadSize = 10 << 20

// Server is the hypervisor external HTTP bridge (health + agent files browser).
type Server struct {
	addr         string
	dataRoot     string
	server       *http.Server
	listener     net.Listener
	terminals    *TerminalManager
	homeResolver func(agentID string) string
	git          *gitsvc.Service
}

// NewServer creates the hypervisor HTTP bridge. dataRoot is set separately via
// SetDataRoot and used to resolve agent file paths.
func NewServer(addr string) *Server {
	if addr == "" {
		addr = ":9090"
	}
	return &Server{
		addr:         addr,
		homeResolver: func(agentID string) string { return agentID },
	}
}

// SetDataRoot configures the files root used for browsing agent files.
func (s *Server) SetDataRoot(dataRoot string) *Server {
	s.dataRoot = dataRoot
	return s
}

// SetGitService wires the git (and data-root browse) service. Without it the
// git endpoints report 501.
func (s *Server) SetGitService(svc *gitsvc.Service) *Server {
	s.git = svc
	return s
}

// SetHomeResolver wires the agentID -> home folder name lookup (display-name
// derived, see the workspace package) used to resolve agent file paths.
func (s *Server) SetHomeResolver(fn func(agentID string) string) *Server {
	if fn != nil {
		s.homeResolver = fn
	}
	return s
}

// SetTerminalManager wires terminal session management to the HTTP bridge
// (endpoints used to list/attach/kill sessions).
func (s *Server) SetTerminalManager(m *TerminalManager) *Server {
	s.terminals = m
	return s
}

// Start serves the hypervisor HTTP API until ctx is cancelled.
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": Version})
	})
	mux.HandleFunc("GET /api/v1/files/{agentID}", s.handleListFiles)
	mux.HandleFunc("GET /api/v1/files/{agentID}/content", s.handleFileContent)
	mux.HandleFunc("PUT /api/v1/files/{agentID}/content", s.handleFileContent)
	mux.HandleFunc("DELETE /api/v1/files/{agentID}/content", s.handleFileContent)
	mux.HandleFunc("POST /api/v1/files/{agentID}/mkdir", s.handleFileMkdir)
	mux.HandleFunc("POST /api/v1/files/{agentID}/rename", s.handleFileRename)
	mux.HandleFunc("POST /api/v1/files/{agentID}/copy", s.handleFileCopy)
	mux.HandleFunc("GET /api/v1/terminals/{agentID}", s.handleTerminalList)
	mux.HandleFunc("POST /api/v1/terminals/{agentID}/attach", s.handleTerminalAttach)
	mux.HandleFunc("DELETE /api/v1/terminals/{agentID}/{terminalID}", s.handleTerminalKill)

	// Git browser: data-root browse endpoints + git operations.
	mux.HandleFunc("GET /api/v1/git/{agentID}/info", s.handleGitInfo)
	mux.HandleFunc("GET /api/v1/git/{agentID}/list", s.handleGitList)
	mux.HandleFunc("GET /api/v1/git/{agentID}/suggest", s.handleGitSuggest)
	mux.HandleFunc("GET /api/v1/git/{agentID}/blob", s.handleGitBlob)
	mux.HandleFunc("PUT /api/v1/git/{agentID}/blob", s.handleGitBlob)
	mux.HandleFunc("GET /api/v1/git/{agentID}/repo", s.handleGitRepo)
	mux.HandleFunc("GET /api/v1/git/{agentID}/status", s.handleGitStatus)
	mux.HandleFunc("GET /api/v1/git/{agentID}/repos", s.handleGitRepos)
	mux.HandleFunc("GET /api/v1/git/{agentID}/diff", s.handleGitDiff)
	mux.HandleFunc("GET /api/v1/git/{agentID}/diffstat", s.handleGitDiffStat)
	mux.HandleFunc("GET /api/v1/git/{agentID}/log", s.handleGitLog)
	mux.HandleFunc("GET /api/v1/git/{agentID}/commit", s.handleGitCommitDetail)
	mux.HandleFunc("GET /api/v1/git/{agentID}/branches", s.handleGitBranches)
	mux.HandleFunc("GET /api/v1/git/{agentID}/remotes", s.handleGitRemotes)
	mux.HandleFunc("GET /api/v1/git/{agentID}/tags", s.handleGitTags)
	mux.HandleFunc("GET /api/v1/git/{agentID}/stashes", s.handleGitStashes)
	mux.HandleFunc("GET /api/v1/git/{agentID}/show", s.handleGitShow)
	mux.HandleFunc("POST /api/v1/git/{agentID}/stage", s.handleGitStage)
	mux.HandleFunc("POST /api/v1/git/{agentID}/unstage", s.handleGitUnstage)
	mux.HandleFunc("POST /api/v1/git/{agentID}/discard", s.handleGitDiscard)
	mux.HandleFunc("POST /api/v1/git/{agentID}/commit", s.handleGitCommit)
	mux.HandleFunc("POST /api/v1/git/{agentID}/branch", s.handleGitBranchCreate)
	mux.HandleFunc("POST /api/v1/git/{agentID}/checkout", s.handleGitCheckout)
	mux.HandleFunc("DELETE /api/v1/git/{agentID}/branch", s.handleGitBranchDelete)
	mux.HandleFunc("POST /api/v1/git/{agentID}/remote", s.handleGitRemote)
	mux.HandleFunc("POST /api/v1/git/{agentID}/fetch", s.handleGitFetch)
	mux.HandleFunc("POST /api/v1/git/{agentID}/push", s.handleGitPush)
	mux.HandleFunc("POST /api/v1/git/{agentID}/pull", s.handleGitPull)
	mux.HandleFunc("POST /api/v1/git/{agentID}/clone", s.handleGitClone)
	mux.HandleFunc("POST /api/v1/git/{agentID}/merge", s.handleGitMerge)
	mux.HandleFunc("POST /api/v1/git/{agentID}/rebase", s.handleGitRebase)
	mux.HandleFunc("POST /api/v1/git/{agentID}/abort", s.handleGitAbort)
	mux.HandleFunc("POST /api/v1/git/{agentID}/resolve", s.handleGitResolve)
	mux.HandleFunc("POST /api/v1/git/{agentID}/tag", s.handleGitTag)
	mux.HandleFunc("POST /api/v1/git/{agentID}/stash", s.handleGitStash)

	s.server = &http.Server{Addr: s.addr, Handler: mux}

	listener, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("hypervisor bridge listen: %w", err)
	}
	s.listener = listener
	s.addr = listener.Addr().String()

	errCh := make(chan error, 1)
	go func() {
		slog.Info("hypervisor bridge listening", "addr", s.addr)
		if err := s.server.Serve(listener); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.server.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		return err
	}
}

// Stop shuts down the HTTP server.
func (s *Server) Stop() error {
	if s.server != nil {
		return s.server.Close()
	}
	return nil
}

// Addr returns the bound listen address (useful when started with :0).
func (s *Server) Addr() string {
	if s.listener != nil {
		return s.listener.Addr().String()
	}
	return s.addr
}

// fsEntry is a plain directory entry shared by the files and git browse APIs.
type fsEntry struct {
	Name     string `json:"name"`
	IsDir    bool   `json:"is_dir"`
	Size     int64  `json:"size,omitempty"`
	Modified int64  `json:"modified,omitempty"` // unix seconds
	Runtime  bool   `json:"runtime,omitempty"`  // agent-managed runtime entry (sessions/cron/media)
}

// defaultRuntimeEntries are structural, agent-managed entries surfaced but
// flagged so the UI can mark them and prevent destructive actions.
var defaultRuntimeEntries = map[string]bool{"sessions": true, "cron.json": true, "media": true}

// readDirEntries lists a directory (os.Stat follows symlinks so a link to a
// directory is reported as a directory).
func readDirEntries(dir string, runtimeEntries map[string]bool) ([]fsEntry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	result := make([]fsEntry, 0, len(entries))
	for _, e := range entries {
		info, err := os.Stat(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		result = append(result, fsEntry{
			Name:     e.Name(),
			IsDir:    info.IsDir(),
			Size:     info.Size(),
			Modified: info.ModTime().Unix(),
			Runtime:  runtimeEntries != nil && runtimeEntries[e.Name()],
		})
	}
	return result, nil
}

func (s *Server) handleListFiles(w http.ResponseWriter, r *http.Request) {
	if s.dataRoot == "" {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "file browsing disabled"})
		return
	}

	agentID := r.PathValue("agentID")
	if agentID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "agent_id is required"})
		return
	}

	ws := workspace.NewNamed(agentID, s.homeResolver(agentID), s.dataRoot)
	if err := ws.EnsureDirs(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	rel := r.URL.Query().Get("path")
	resolved, err := ws.Resolve(rel)
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}

	entries, err := readDirEntries(resolved, defaultRuntimeEntries)
	if err != nil {
		// A remembered directory can be gone (the agent was re-provisioned, a
		// folder was deleted). That is the caller's fact, not a server failure:
		// the browser's file panel keys its "go back home" recovery off a 404.
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such directory"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"agent_id": agentID,
		"path":     rel,
		"entries":  entries,
	})
}

// handleFileContent reads (GET) or writes (PUT) a file inside an agent's files
// workspace. The target is resolved through the workspace manager so both the
// agent and shared file spaces are reachable but nothing outside them.
func (s *Server) handleFileContent(w http.ResponseWriter, r *http.Request) {
	if s.dataRoot == "" {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "file browsing disabled"})
		return
	}

	agentID := r.PathValue("agentID")
	if agentID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "agent_id is required"})
		return
	}

	rel := r.URL.Query().Get("path")
	if rel == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path is required"})
		return
	}

	ws := workspace.NewNamed(agentID, s.homeResolver(agentID), s.dataRoot)
	if err := ws.EnsureDirs(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	resolved, err := ws.Resolve(rel)
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}

	if r.Method == http.MethodPut {
		s.writeFileContent(w, r, resolved)
		return
	}
	if r.Method == http.MethodDelete {
		if ws.IsProtected(resolved) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "cannot delete a protected entry"})
			return
		}
		s.deleteFileContent(w, resolved)
		return
	}
	s.readFileContent(w, resolved)
}

// deleteFileContent removes a file (or empty directory).
func (s *Server) deleteFileContent(w http.ResponseWriter, resolved string) {
	info, err := os.Lstat(resolved)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "file not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if info.IsDir() {
		if err := os.RemoveAll(resolved); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	if err := os.Remove(resolved); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) readFileContent(w http.ResponseWriter, resolved string) {
	info, err := os.Stat(resolved)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "file not found"})
		return
	}
	if info.IsDir() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path is a directory"})
		return
	}

	content, err := os.ReadFile(resolved)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	contentType := mime.TypeByExtension(filepath.Ext(resolved))
	if contentType == "" {
		contentType = http.DetectContentType(content)
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(content)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

func (s *Server) writeFileContent(w http.ResponseWriter, r *http.Request, resolved string) {
	if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxFileUploadSize+1))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if len(body) > maxFileUploadSize {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "file exceeds max size"})
		return
	}

	if err := os.WriteFile(resolved, body, 0o644); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleFileMkdir creates a directory (including any missing parents)
// inside an agent's files workspace.
func (s *Server) handleFileMkdir(w http.ResponseWriter, r *http.Request) {
	if s.dataRoot == "" {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "file browsing disabled"})
		return
	}

	agentID := r.PathValue("agentID")
	if agentID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "agent_id is required"})
		return
	}

	rel := r.URL.Query().Get("path")
	if rel == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path is required"})
		return
	}

	ws := workspace.NewNamed(agentID, s.homeResolver(agentID), s.dataRoot)
	if err := ws.EnsureDirs(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	resolved, err := ws.Resolve(rel)
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}

	if err := os.MkdirAll(resolved, 0o755); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleFileRename renames or moves an entry inside an agent's files workspace.
// Both the source (path) and destination (to) are resolved through the
// workspace manager so nothing outside the allowed roots can be touched.
func (s *Server) handleFileRename(w http.ResponseWriter, r *http.Request) {
	if s.dataRoot == "" {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "file browsing disabled"})
		return
	}

	agentID := r.PathValue("agentID")
	if agentID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "agent_id is required"})
		return
	}

	from := r.URL.Query().Get("path")
	to := r.URL.Query().Get("to")
	if from == "" || to == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path and to are required"})
		return
	}

	ws := workspace.NewNamed(agentID, s.homeResolver(agentID), s.dataRoot)
	if err := ws.EnsureDirs(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	fromResolved, err := ws.Resolve(from)
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}
	if ws.IsProtected(fromResolved) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "cannot rename a protected entry"})
		return
	}
	toResolved, err := ws.Resolve(to)
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}

	if _, err := os.Lstat(toResolved); err == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "destination already exists"})
		return
	}

	if err := os.Rename(fromResolved, toResolved); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleFileCopy copies a file or directory (recursively) inside an
// agent's workspace. Both the source (path) and destination (to) are resolved
// through the workspace manager so nothing outside the allowed roots can be
// touched; the destination must not already exist.
func (s *Server) handleFileCopy(w http.ResponseWriter, r *http.Request) {
	if s.dataRoot == "" {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "file browsing disabled"})
		return
	}

	agentID := r.PathValue("agentID")
	if agentID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "agent_id is required"})
		return
	}

	from := r.URL.Query().Get("path")
	to := r.URL.Query().Get("to")
	if from == "" || to == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path and to are required"})
		return
	}

	ws := workspace.NewNamed(agentID, s.homeResolver(agentID), s.dataRoot)
	if err := ws.EnsureDirs(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	fromResolved, err := ws.Resolve(from)
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}
	toResolved, err := ws.Resolve(to)
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}

	info, err := os.Stat(fromResolved)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "source not found"})
		return
	}
	if _, err := os.Lstat(toResolved); err == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "destination already exists"})
		return
	}

	if err := copyPath(fromResolved, toResolved, info); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// copyPath copies a file or directory (recursively) from src to dst.
func copyPath(src, dst string, info os.FileInfo) error {
	if !info.IsDir() {
		return copyFile(src, dst)
	}
	if err := os.MkdirAll(dst, info.Mode().Perm()); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		srcChild := filepath.Join(src, e.Name())
		dstChild := filepath.Join(dst, e.Name())
		if e.Type()&os.ModeSymlink != 0 {
			// Re-create the symlink (its target is validated by the path guards
			// on any later access).
			target, err := os.Readlink(srcChild)
			if err != nil {
				return err
			}
			if err := os.Symlink(target, dstChild); err != nil {
				return err
			}
			continue
		}
		childInfo, err := e.Info()
		if err != nil {
			return err
		}
		if err := copyPath(srcChild, dstChild, childInfo); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

// handleTerminalList reports the live terminal sessions for an agent.
func (s *Server) handleTerminalList(w http.ResponseWriter, r *http.Request) {
	if s.terminals == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "terminals disabled"})
		return
	}
	agentID := r.PathValue("agentID")
	if agentID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "agent_id is required"})
		return
	}
	sessions := s.terminals.List(agentID)
	if sessions == nil {
		sessions = []TerminalInfo{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"agent_id": agentID, "terminals": sessions})
}

// handleTerminalAttach creates a new terminal session for an agent, or attaches
// to an existing one (terminal_id supplied) replaying its ring buffer.
func (s *Server) handleTerminalAttach(w http.ResponseWriter, r *http.Request) {
	if s.terminals == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "terminals disabled"})
		return
	}
	agentID := r.PathValue("agentID")
	if agentID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "agent_id is required"})
		return
	}

	var req struct {
		TerminalID string `json:"terminal_id"`
		Cols       int    `json:"cols"`
		Rows       int    `json:"rows"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}

	att, err := s.terminals.Open(agentID, req.Cols, req.Rows, req.TerminalID, true)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, att)
}

// handleTerminalKill terminates a terminal session.
func (s *Server) handleTerminalKill(w http.ResponseWriter, r *http.Request) {
	if s.terminals == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "terminals disabled"})
		return
	}
	agentID := r.PathValue("agentID")
	terminalID := r.PathValue("terminalID")
	if agentID == "" || terminalID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "agent_id and terminal_id are required"})
		return
	}
	session, ok := s.terminals.Get(terminalID)
	if !ok || session.AgentID != agentID {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "terminal not found"})
		return
	}
	if err := s.terminals.Kill(terminalID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
