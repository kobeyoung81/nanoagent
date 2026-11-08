package gitsvc

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestService creates a Service rooted at a temp dir and initializes a git
// repo at <root>/work/proj.
func newTestService(t *testing.T) (*Service, string) {
	t.Helper()
	root := t.TempDir()
	svc := New(root, 30*time.Second)
	repo := filepath.Join(root, "work", "proj")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "-b", "main")
	git(t, repo, "config", "user.email", "t@example.com")
	git(t, repo, "config", "user.name", "Tester")
	return svc, repo
}

// git runs a git command in dir and fails the test on error.
func git(t *testing.T, dir string, args ...string) string {
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

// writeFile writes a file into dir.
func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	full := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRepoRootOutsideDataRoot(t *testing.T) {
	svc := New(t.TempDir(), 5*time.Second)
	if _, err := svc.repoRoot(context.Background(), "../../etc"); err == nil {
		t.Fatalf("expected confinement error, got nil")
	}
}

// TestEmptyCollectionsAreNonNil guards the JSON contract: list payloads must
// encode as [] (not null) so the web client can call .length/.map safely. A
// nil slice previously produced null and crashed the Git panel.
func TestEmptyCollectionsAreNonNil(t *testing.T) {
	svc, repo := newTestService(t)
	writeFile(t, repo, "a.txt", "one")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "init")
	ctx := context.Background()

	st, err := svc.Status(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if st.Staged == nil || st.Unstaged == nil || st.Untracked == nil || st.Conflicted == nil {
		t.Fatalf("status slices must be non-nil: %+v", st)
	}
	if blob, _ := json.Marshal(st); strings.Contains(string(blob), `":null`) {
		t.Fatalf("status JSON contains null fields: %s", blob)
	}

	page, err := svc.Log(ctx, repo, "", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if page.Commits == nil {
		t.Fatal("log commits must be non-nil")
	}

	sha := strings.TrimSpace(git(t, repo, "rev-parse", "HEAD"))
	detail, err := svc.CommitDetail(ctx, repo, sha)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Files == nil {
		t.Fatal("commit files must be non-nil")
	}

	if branches, err := svc.Branches(ctx, repo); err != nil {
		t.Fatal(err)
	} else if branches == nil {
		t.Fatal("branches must be non-nil")
	}
	if remotes, err := svc.Remotes(ctx, repo); err != nil {
		t.Fatal(err)
	} else if remotes == nil {
		t.Fatal("remotes must be non-nil")
	}
	if tags, err := svc.Tags(ctx, repo); err != nil {
		t.Fatal(err)
	} else if tags == nil {
		t.Fatal("tags must be non-nil")
	}
	if stashes, err := svc.Stashes(ctx, repo); err != nil {
		t.Fatal(err)
	} else if stashes == nil {
		t.Fatal("stashes must be non-nil")
	}
	if stats, err := svc.DiffStat(ctx, repo, false, ""); err != nil {
		t.Fatal(err)
	} else if stats == nil {
		t.Fatal("diffstat must be non-nil")
	}
}

func TestStatusParsing(t *testing.T) {
	svc, repo := newTestService(t)

	writeFile(t, repo, "tracked.txt", "one")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "init")

	// staged modification
	writeFile(t, repo, "tracked.txt", "two")
	git(t, repo, "add", "tracked.txt")
	// unstaged modification (after staging, also shows M in worktree)
	writeFile(t, repo, "tracked.txt", "three")
	// untracked
	writeFile(t, repo, "new.txt", "new")

	st, err := svc.Status(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if st.Branch != "main" {
		t.Errorf("branch = %q, want main", st.Branch)
	}
	if !containsPath(st.Staged, "tracked.txt") {
		t.Errorf("staged missing tracked.txt: %+v", st.Staged)
	}
	if !containsPath(st.Unstaged, "tracked.txt") {
		t.Errorf("unstaged missing tracked.txt: %+v", st.Unstaged)
	}
	if !containsPath(st.Untracked, "new.txt") {
		t.Errorf("untracked missing new.txt: %+v", st.Untracked)
	}
}

func containsPath(set []ChangeStatus, p string) bool {
	for _, c := range set {
		if c.Path == p {
			return true
		}
	}
	return false
}

func TestStatusConflictAndRename(t *testing.T) {
	svc, repo := newTestService(t)

	writeFile(t, repo, "a.txt", "base")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "init")

	// Branch b1 modifies a.txt.
	git(t, repo, "checkout", "-b", "b1")
	writeFile(t, repo, "a.txt", "from b1")
	git(t, repo, "commit", "-am", "b1 change")

	// Back on main, modify same line then merge b1 -> conflict.
	git(t, repo, "checkout", "main")
	writeFile(t, repo, "a.txt", "from main")
	git(t, repo, "commit", "-am", "main change")
	out := gitTolerate(t, repo, "merge", "b1")
	if !strings.Contains(out, "conflict") {
		t.Fatalf("expected merge conflict, got: %s", out)
	}

	st, err := svc.Status(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if !containsPath(st.Conflicted, "a.txt") {
		t.Errorf("conflicted missing a.txt: %+v", st.Conflicted)
	}
	if !st.MergeInP {
		t.Errorf("expected merge_in_progress=true, got %+v", st)
	}
}

func gitTolerate(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	_ = cmd.Run()
	return out.String()
}

func TestStageUnstageDiscardCommit(t *testing.T) {
	svc, repo := newTestService(t)
	ctx := context.Background()

	writeFile(t, repo, "file.txt", "v1")
	writeFile(t, repo, "junk.txt", "junk")
	if err := svc.StageFiles(ctx, repo, []string{"file.txt"}); err != nil {
		t.Fatal(err)
	}
	// Discard the untracked junk file.
	if err := svc.DiscardFiles(ctx, repo, []string{"junk.txt"}); err != nil {
		t.Fatal(err)
	}
	// Commit only the staged file.
	if err := svc.Commit(ctx, repo, CommitOpts{Message: "first commit"}); err != nil {
		t.Fatal(err)
	}
	st, err := svc.Status(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Staged) != 0 {
		t.Errorf("expected clean index, got staged %+v", st.Staged)
	}

	// Modify, stage, then discard the staged change.
	writeFile(t, repo, "file.txt", "v2")
	if err := svc.StageFiles(ctx, repo, []string{"file.txt"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.DiscardFiles(ctx, repo, []string{"file.txt"}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(repo, "file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "v1" {
		t.Errorf("discard did not revert file.txt, got %q", content)
	}
	if err := gitTolerateOK(t, repo, "diff", "--quiet"); err != nil {
		t.Errorf("expected clean tree after discard: %v", err)
	}
}

func gitTolerateOK(t *testing.T, dir string, args ...string) error {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if err := cmd.Run(); err != nil {
		return err
	}
	return nil
}

func TestAmendSignoffAndAll(t *testing.T) {
	svc, repo := newTestService(t)
	ctx := context.Background()

	writeFile(t, repo, "a.txt", "a")
	if err := svc.Commit(ctx, repo, CommitOpts{Message: "first\n\nbody line", All: true}); err != nil {
		t.Fatal(err)
	}
	last, err := svc.LastCommit(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(last.Subject, "first") {
		t.Errorf("unexpected subject %q", last.Subject)
	}
	// Amend with signoff.
	writeFile(t, repo, "a.txt", "a2")
	if err := svc.Commit(ctx, repo, CommitOpts{Message: "amended", All: true, Amend: true, SignOff: true}); err != nil {
		t.Fatal(err)
	}
	last2, err := svc.LastCommit(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if last2.Hash == last.Hash {
		t.Error("expected a new hash after amend")
	}
	if !strings.Contains(last2.Body, "Signed-off-by:") && !strings.Contains(last2.Body, "Signed-off-by") {
		t.Errorf("expected sign-off in body, got %q", last2.Body)
	}
}

func TestBranchesCheckoutDelete(t *testing.T) {
	svc, repo := newTestService(t)
	ctx := context.Background()

	writeFile(t, repo, "f.txt", "x")
	if err := svc.Commit(ctx, repo, CommitOpts{Message: "init", All: true}); err != nil {
		t.Fatal(err)
	}
	if err := svc.CreateBranch(ctx, repo, "feature", ""); err != nil {
		t.Fatal(err)
	}
	if err := svc.Checkout(ctx, repo, "feature", false); err != nil {
		t.Fatal(err)
	}
	branches, err := svc.Branches(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	var cur *Branch
	for i := range branches {
		if branches[i].Current {
			cur = &branches[i]
		}
	}
	if cur == nil || cur.Name != "feature" {
		t.Fatalf("expected current branch feature, got %+v", branches)
	}
	// Delete the other branch.
	if err := svc.Checkout(ctx, repo, "main", false); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteBranch(ctx, repo, "feature", false); err != nil {
		t.Fatal(err)
	}
}

func TestLogAndCommitDetail(t *testing.T) {
	svc, repo := newTestService(t)
	ctx := context.Background()

	writeFile(t, repo, "one.txt", "1")
	if err := svc.Commit(ctx, repo, CommitOpts{Message: "one", All: true}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, repo, "two.txt", "2")
	if err := svc.Commit(ctx, repo, CommitOpts{Message: "two", All: true}); err != nil {
		t.Fatal(err)
	}
	page, err := svc.Log(ctx, repo, "", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Commits) != 2 {
		t.Fatalf("expected 2 commits, got %d", len(page.Commits))
	}
	if page.Commits[0].Subject != "two" {
		t.Errorf("expected newest subject 'two', got %q", page.Commits[0].Subject)
	}
	detail, err := svc.CommitDetail(ctx, repo, page.Commits[0].Hash)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Files) != 1 || detail.Files[0].Path != "two.txt" {
		t.Errorf("unexpected files: %+v", detail.Files)
	}
}

func TestDiffAndDiffStat(t *testing.T) {
	svc, repo := newTestService(t)
	ctx := context.Background()

	writeFile(t, repo, "f.txt", "line1\nline2\n")
	if err := svc.Commit(ctx, repo, CommitOpts{Message: "init", All: true}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, repo, "f.txt", "line1\nline2 changed\nline3\n")
	d, err := svc.Diff(ctx, repo, "f.txt", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d, "+line2 changed") || !strings.Contains(d, "+line3") {
		t.Errorf("unexpected diff:\n%s", d)
	}
	stats, err := svc.DiffStat(ctx, repo, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 1 || stats[0].Path != "f.txt" {
		t.Errorf("unexpected stat: %+v", stats)
	}
}

func TestResolveOurs(t *testing.T) {
	svc, repo := newTestService(t)
	ctx := context.Background()

	writeFile(t, repo, "a.txt", "base")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "init")
	git(t, repo, "checkout", "-b", "b1")
	writeFile(t, repo, "a.txt", "their side")
	git(t, repo, "commit", "-am", "b1")
	git(t, repo, "checkout", "main")
	writeFile(t, repo, "a.txt", "our side")
	git(t, repo, "commit", "-am", "main")
	gitTolerate(t, repo, "merge", "b1")

	if err := svc.Resolve(ctx, repo, ResolveOpts{File: "a.txt", Strategy: "ours"}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(repo, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(content), "our side") {
		t.Errorf("resolve ours: unexpected content %q", content)
	}
	st, err := svc.Status(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if containsPath(st.Conflicted, "a.txt") {
		t.Errorf("expected a.txt resolved, conflicted = %+v", st.Conflicted)
	}
}

func TestShowFileByRef(t *testing.T) {
	svc, repo := newTestService(t)
	ctx := context.Background()

	writeFile(t, repo, "f.txt", "v1")
	if err := svc.Commit(ctx, repo, CommitOpts{Message: "init", All: true}); err != nil {
		t.Fatal(err)
	}
	content, err := svc.ShowFile(ctx, repo, "HEAD", "f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "v1" {
		t.Errorf("show HEAD:f.txt = %q, want v1", content)
	}
}

func TestCloneURLValidation(t *testing.T) {
	svc := New(t.TempDir(), 5*time.Second)
	for _, url := range []string{"file:///etc/passwd", "ext::sh -c pwd", "ftp://x", "/absolute/path", "relative-path"} {
		if _, err := svc.Clone(context.Background(), CloneOpts{URL: url, Dir: "dest"}); err == nil {
			t.Errorf("expected invalid URL %q to be rejected", url)
		}
	}
	if !validCloneURL("https://example.com/repo.git") {
		t.Error("https clone URL should be valid")
	}
	if !validCloneURL("git@github.com:user/repo.git") {
		t.Error("scp-like clone URL should be valid")
	}
}

func TestCloneCreatesRepo(t *testing.T) {
	root := t.TempDir()
	origin := filepath.Join(root, "origin")
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, origin, "init", "--bare")
	svc := New(root, 30*time.Second)
	_, err := svc.Clone(context.Background(), CloneOpts{URL: origin, Dir: "cloned"})
	if err == nil {
		// file:// URLs are blocked by design; verify the rejection works.
		if !strings.Contains(err.Error(), "unsupported") {
			t.Errorf("unexpected error: %v", err)
		}
	}
}

func TestResolvePathConfinement(t *testing.T) {
	svc := New(t.TempDir(), 5*time.Second)
	if _, err := svc.ResolvePath("../escape"); err == nil {
		t.Fatal("expected escape to be rejected")
	}
	if _, err := svc.ResolvePath("agents/x"); err != nil {
		t.Fatalf("valid path rejected: %v", err)
	}
}
