package hypervisor

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	server := NewServer(":0").SetDataRoot(t.TempDir())
	return server
}

func fileRequest(t *testing.T, method, agentID, path string, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, "/api/v1/files/"+agentID+"/content?path="+path, strings.NewReader(body))
	req.SetPathValue("agentID", agentID)
	return req
}

func TestFileContentUploadAndDownload(t *testing.T) {
	server := newTestServer(t)

	// Upload a file into a nested folder.
	put := httptest.NewRecorder()
	server.handleFileContent(put, fileRequest(t, http.MethodPut, "agent-1", "bootstrap%2Fnotes.md", "hello world"))
	if put.Code != http.StatusOK {
		t.Fatalf("upload: expected 200, got %d: %s", put.Code, put.Body.String())
	}

	resolved := filepath.Join(server.dataRoot, "agents", "agent-1", "bootstrap", "notes.md")
	content, err := os.ReadFile(resolved)
	if err != nil {
		t.Fatalf("read uploaded file: %v", err)
	}
	if string(content) != "hello world" {
		t.Fatalf("uploaded content = %q, want %q", content, "hello world")
	}

	// Download it back.
	get := httptest.NewRecorder()
	server.handleFileContent(get, fileRequest(t, http.MethodGet, "agent-1", "bootstrap%2Fnotes.md", ""))
	if get.Code != http.StatusOK {
		t.Fatalf("download: expected 200, got %d: %s", get.Code, get.Body.String())
	}
	if get.Body.String() != "hello world" {
		t.Fatalf("download body = %q, want %q", get.Body.String(), "hello world")
	}
}

func TestFileContentRejectsOutsideRoot(t *testing.T) {
	server := newTestServer(t)

	put := httptest.NewRecorder()
	server.handleFileContent(put, fileRequest(t, http.MethodPut, "agent-1", "..%2F..%2Fescape.txt", "nope"))
	if put.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for path traversal, got %d: %s", put.Code, put.Body.String())
	}
}

func TestFileContentMissing(t *testing.T) {
	server := newTestServer(t)

	get := httptest.NewRecorder()
	server.handleFileContent(get, fileRequest(t, http.MethodGet, "agent-1", "missing.txt", ""))
	if get.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for missing file, got %d", get.Code)
	}
}

func TestFileContentDelete(t *testing.T) {
	server := newTestServer(t)

	put := httptest.NewRecorder()
	server.handleFileContent(put, fileRequest(t, http.MethodPut, "agent-1", "notes.txt", "hello"))
	if put.Code != http.StatusOK {
		t.Fatalf("upload: expected 200, got %d", put.Code)
	}

	del := httptest.NewRecorder()
	server.handleFileContent(del, fileRequest(t, http.MethodDelete, "agent-1", "notes.txt", ""))
	if del.Code != http.StatusOK {
		t.Fatalf("delete: expected 200, got %d: %s", del.Code, del.Body.String())
	}

	if _, err := os.Stat(filepath.Join(server.dataRoot, "agents", "agent-1", "notes.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected file removed, stat err = %v", err)
	}

	// Deleting a missing file returns 404.
	delAgain := httptest.NewRecorder()
	server.handleFileContent(delAgain, fileRequest(t, http.MethodDelete, "agent-1", "notes.txt", ""))
	if delAgain.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for missing delete, got %d", delAgain.Code)
	}
}

func TestFilesListFlagsRuntimeEntries(t *testing.T) {
	server := newTestServer(t)

	home := filepath.Join(server.dataRoot, "agents", "agent-1")
	agentDir := filepath.Join(home, "agent")
	if err := os.MkdirAll(filepath.Join(agentDir, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(agentDir, "media"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "cron.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "AGENT.md"), []byte("# agent"), 0o644); err != nil {
		t.Fatal(err)
	}

	list := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/files/agent-1?path=agent", nil)
	req.SetPathValue("agentID", "agent-1")
	server.handleListFiles(list, req)
	if list.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d: %s", list.Code, list.Body.String())
	}

	body := list.Body.String()
	for _, runtime := range []string{"sessions", "cron.json", "media"} {
		if !strings.Contains(body, `"name":"`+runtime+`","is_dir"`) || !strings.Contains(body, `"runtime":true`) {
			t.Errorf("expected runtime entry %q visible with runtime flag, got: %s", runtime, body)
		}
	}
	if !strings.Contains(body, "AGENT.md") {
		t.Errorf("expected AGENT.md visible in the agent home, got: %s", body)
	}
}

func TestFilesListSharedspaceIsDirectory(t *testing.T) {
	server := newTestServer(t)

	home := filepath.Join(server.dataRoot, "agents", "agent-1")
	if err := os.MkdirAll(filepath.Join(server.dataRoot, "sharedspace"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(server.dataRoot, "sharedspace"), filepath.Join(home, "sharedspace")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "workspace"), 0o755); err != nil {
		t.Fatal(err)
	}

	list := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/files/agent-1", nil)
	req.SetPathValue("agentID", "agent-1")
	server.handleListFiles(list, req)
	if list.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d: %s", list.Code, list.Body.String())
	}
	if !strings.Contains(list.Body.String(), `"name":"sharedspace","is_dir":true`) {
		t.Fatalf("expected sharedspace listed as a directory, got: %s", list.Body.String())
	}
}

func TestFilesDeleteProtected(t *testing.T) {
	server := newTestServer(t)

	home := filepath.Join(server.dataRoot, "agents", "agent-1")
	if err := os.MkdirAll(server.dataRoot+"/sharedspace", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(server.dataRoot+"/sharedspace", home+"/sharedspace"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(server.dataRoot+"/sharedspace/keep.txt", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Deleting the sharedspace entry is blocked.
	del := httptest.NewRecorder()
	server.handleFileContent(del, fileRequest(t, http.MethodDelete, "agent-1", "sharedspace", ""))
	if del.Code != http.StatusForbidden {
		t.Fatalf("delete sharedspace: expected 403, got %d: %s", del.Code, del.Body.String())
	}

	// A file inside sharedspace is deletable.
	delIn := httptest.NewRecorder()
	server.handleFileContent(delIn, fileRequest(t, http.MethodDelete, "agent-1", "sharedspace%2Fkeep.txt", ""))
	if delIn.Code != http.StatusOK {
		t.Fatalf("delete sharedspace file: expected 200, got %d: %s", delIn.Code, delIn.Body.String())
	}
	if _, err := os.Stat(server.dataRoot + "/sharedspace/keep.txt"); !os.IsNotExist(err) {
		t.Fatalf("expected sharedspace file removed, stat err = %v", err)
	}
}

func TestFilesRenameProtected(t *testing.T) {
	server := newTestServer(t)

	if err := os.MkdirAll(server.dataRoot+"/sharedspace", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(server.dataRoot+"/agents/agent-1", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(server.dataRoot+"/sharedspace", server.dataRoot+"/agents/agent-1/sharedspace"); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	server.handleFileRename(rec, workspaceRequest(http.MethodPost, "agent-1", "/rename?path=sharedspace&to=elsewhere"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("rename sharedspace: expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestFilesRuntimeEntriesProtected(t *testing.T) {
	server := newTestServer(t)

	home := filepath.Join(server.dataRoot, "agents", "agent-1")
	if err := os.MkdirAll(filepath.Join(home, "agent", "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "agent", "media"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "agent", "cron.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, target := range []string{"agent%2Fsessions", "agent%2Fcron.json", "agent%2Fmedia"} {
		del := httptest.NewRecorder()
		server.handleFileContent(del, fileRequest(t, http.MethodDelete, "agent-1", target, ""))
		if del.Code != http.StatusForbidden {
			t.Fatalf("delete runtime entry %s: expected 403, got %d: %s", target, del.Code, del.Body.String())
		}
	}
}

func TestFilesCopy(t *testing.T) {
	server := newTestServer(t)

	// Copy a file.
	put := httptest.NewRecorder()
	server.handleFileContent(put, fileRequest(t, http.MethodPut, "agent-1", "notes.txt", "hello copy"))
	if put.Code != http.StatusOK {
		t.Fatalf("upload: expected 200, got %d", put.Code)
	}

	copyRec := httptest.NewRecorder()
	server.handleFileCopy(copyRec, workspaceRequest(http.MethodPost, "agent-1", "/copy?path=notes.txt&to=copied.txt"))
	if copyRec.Code != http.StatusOK {
		t.Fatalf("copy file: expected 200, got %d: %s", copyRec.Code, copyRec.Body.String())
	}
	data, err := os.ReadFile(filepath.Join(server.dataRoot, "agents", "agent-1", "copied.txt"))
	if err != nil || string(data) != "hello copy" {
		t.Fatalf("copied file content = %q (err %v)", data, err)
	}

	// Copy a directory recursively.
	mk := httptest.NewRecorder()
	server.handleFileMkdir(mk, workspaceRequest(http.MethodPost, "agent-1", "/mkdir?path=sub"))
	if mk.Code != http.StatusOK {
		t.Fatalf("mkdir: expected 200, got %d", mk.Code)
	}
	putChild := httptest.NewRecorder()
	server.handleFileContent(putChild, fileRequest(t, http.MethodPut, "agent-1", "sub%2Fchild.txt", "child"))
	if putChild.Code != http.StatusOK {
		t.Fatalf("upload child: expected 200, got %d", putChild.Code)
	}

	copyDir := httptest.NewRecorder()
	server.handleFileCopy(copyDir, workspaceRequest(http.MethodPost, "agent-1", "/copy?path=sub&to=sub-copy"))
	if copyDir.Code != http.StatusOK {
		t.Fatalf("copy dir: expected 200, got %d: %s", copyDir.Code, copyDir.Body.String())
	}
	if _, err := os.Stat(filepath.Join(server.dataRoot, "agents", "agent-1", "sub-copy", "child.txt")); err != nil {
		t.Fatalf("expected copied directory tree: %v", err)
	}
}

func TestFilesCopyToExistingConflicts(t *testing.T) {
	server := newTestServer(t)

	put := httptest.NewRecorder()
	server.handleFileContent(put, fileRequest(t, http.MethodPut, "agent-1", "a.txt", "a"))
	putB := httptest.NewRecorder()
	server.handleFileContent(putB, fileRequest(t, http.MethodPut, "agent-1", "b.txt", "b"))

	copyRec := httptest.NewRecorder()
	server.handleFileCopy(copyRec, workspaceRequest(http.MethodPost, "agent-1", "/copy?path=a.txt&to=b.txt"))
	if copyRec.Code != http.StatusConflict {
		t.Fatalf("copy to existing: expected 409, got %d: %s", copyRec.Code, copyRec.Body.String())
	}
}

func TestFilesListIncludesModified(t *testing.T) {
	server := newTestServer(t)

	put := httptest.NewRecorder()
	server.handleFileContent(put, fileRequest(t, http.MethodPut, "agent-1", "notes.txt", "hello"))
	if put.Code != http.StatusOK {
		t.Fatalf("upload: expected 200, got %d", put.Code)
	}

	list := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/files/agent-1", nil)
	req.SetPathValue("agentID", "agent-1")
	server.handleListFiles(list, req)
	if list.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d: %s", list.Code, list.Body.String())
	}
	if !strings.Contains(list.Body.String(), `"modified"`) {
		t.Fatalf("expected modified field in listing, got: %s", list.Body.String())
	}
}

func workspaceRequest(method, agentID, path string) *http.Request {
	req := httptest.NewRequest(method, "/api/v1/files/"+agentID+path, nil)
	req.SetPathValue("agentID", agentID)
	return req
}

func TestFilesMkdir(t *testing.T) {
	server := newTestServer(t)

	req := workspaceRequest(http.MethodPost, "agent-1", "/mkdir?path=nested%2Ffolder")
	rec := httptest.NewRecorder()
	server.handleFileMkdir(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("mkdir: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	dir := filepath.Join(server.dataRoot, "agents", "agent-1", "nested", "folder")
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("expected directory created, stat err = %v", err)
	}
}

func TestFilesMkdirRejectsTraversal(t *testing.T) {
	server := newTestServer(t)

	req := workspaceRequest(http.MethodPost, "agent-1", "/mkdir?path=..%2F..%2Fescape")
	rec := httptest.NewRecorder()
	server.handleFileMkdir(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for path traversal, got %d", rec.Code)
	}
}

func TestFilesRename(t *testing.T) {
	server := newTestServer(t)

	put := httptest.NewRecorder()
	server.handleFileContent(put, fileRequest(t, http.MethodPut, "agent-1", "old.txt", "hello"))
	if put.Code != http.StatusOK {
		t.Fatalf("upload: expected 200, got %d", put.Code)
	}

	req := workspaceRequest(http.MethodPost, "agent-1", "/rename?path=old.txt&to=new.txt")
	rec := httptest.NewRecorder()
	server.handleFileRename(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("rename: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	base := filepath.Join(server.dataRoot, "agents", "agent-1")
	if _, err := os.Stat(filepath.Join(base, "new.txt")); err != nil {
		t.Fatalf("expected renamed file to exist: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "old.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected old file removed, stat err = %v", err)
	}
}

func TestFilesRenameToExistingConflicts(t *testing.T) {
	server := newTestServer(t)

	for _, name := range []string{"a.txt", "b.txt"} {
		put := httptest.NewRecorder()
		server.handleFileContent(put, fileRequest(t, http.MethodPut, "agent-1", name, "x"))
		if put.Code != http.StatusOK {
			t.Fatalf("upload %s: expected 200, got %d", name, put.Code)
		}
	}

	req := workspaceRequest(http.MethodPost, "agent-1", "/rename?path=a.txt&to=b.txt")
	rec := httptest.NewRecorder()
	server.handleFileRename(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 for existing destination, got %d", rec.Code)
	}
}

func TestFilesDeleteFolderRecursively(t *testing.T) {
	server := newTestServer(t)

	mk := httptest.NewRecorder()
	server.handleFileMkdir(mk, workspaceRequest(http.MethodPost, "agent-1", "/mkdir?path=folder"))
	if mk.Code != http.StatusOK {
		t.Fatalf("mkdir: expected 200, got %d", mk.Code)
	}

	put := httptest.NewRecorder()
	server.handleFileContent(put, fileRequest(t, http.MethodPut, "agent-1", "folder%2Fchild.txt", "x"))
	if put.Code != http.StatusOK {
		t.Fatalf("upload child: expected 200, got %d", put.Code)
	}

	del := httptest.NewRecorder()
	server.handleFileContent(del, fileRequest(t, http.MethodDelete, "agent-1", "folder", ""))
	if del.Code != http.StatusOK {
		t.Fatalf("delete folder: expected 200, got %d: %s", del.Code, del.Body.String())
	}

	dir := filepath.Join(server.dataRoot, "agents", "agent-1", "folder")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("expected folder removed recursively, stat err = %v", err)
	}
}

// A directory the browser remembered can be gone (a re-provisioned agent, a
// deleted folder). The file panel recovers from a 404, so this must not look like
// a server failure.
func TestListFilesMissingDirectoryIs404(t *testing.T) {
	server := newTestServer(t)
	home := filepath.Join(server.dataRoot, "agents", "agent-1")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}

	list := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/files/agent-1?path=workspace/gone", nil)
	req.SetPathValue("agentID", "agent-1")
	server.handleListFiles(list, req)
	if list.Code != http.StatusNotFound {
		t.Fatalf("list of a missing directory: expected 404, got %d: %s", list.Code, list.Body.String())
	}
}
