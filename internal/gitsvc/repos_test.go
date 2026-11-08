package gitsvc

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// initRepo initializes a git repository at dir with a single commit.
func initRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "init", "-b", "main")
	git(t, dir, "config", "user.email", "t@example.com")
	git(t, dir, "config", "user.name", "Tester")
	writeFile(t, dir, "README.md", "hello")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "init")
}

// newRepoFixture builds a data root containing:
//
//	codes/super                    repository with a real submodule
//	codes/super/modules/lib        submodule of super (.git is a file)
//	codes/super/extra/nested       unregistered nested repository
//	codes/super/node_modules/junk  pruned decoy repository
//	codes/super/.inner/repo        hidden directory holding a repository
//	codes/other                    sibling repository
//	codes/.hidden/repo             hidden directory holding a repository
//
// and returns the service, the data root and the fixture directories.
func newRepoFixture(t *testing.T) (*Service, string, map[string]string) {
	t.Helper()
	root := t.TempDir()
	svc := New(root, 30*time.Second)

	paths := map[string]string{
		"super":  filepath.Join(root, "codes", "super"),
		"lib":    filepath.Join(root, "codes", "super", "modules", "lib"),
		"nested": filepath.Join(root, "codes", "super", "extra", "nested"),
		"junk":   filepath.Join(root, "codes", "super", "node_modules", "junk"),
		"inner":  filepath.Join(root, "codes", "super", ".inner", "repo"),
		"other":  filepath.Join(root, "codes", "other"),
		"hidden": filepath.Join(root, "codes", ".hidden", "repo"),
		"origin": filepath.Join(root, "src", "lib"),
	}

	initRepo(t, paths["origin"])
	initRepo(t, paths["super"])
	initRepo(t, paths["nested"])
	initRepo(t, paths["junk"])
	initRepo(t, paths["other"])
	initRepo(t, paths["inner"])
	initRepo(t, paths["hidden"])

	// A real submodule: its .git entry is a file pointing into the
	// superproject's gitdir, which is what the discovery walk must recognize.
	git(t, paths["super"], "-c", "protocol.file.allow=always", "submodule", "add", paths["origin"], "modules/lib")
	git(t, paths["super"], "commit", "-m", "add submodule")

	return svc, root, paths
}

// reposUnder returns the RelPath of every discovered repository located under
// root, sorted. Filtering makes the assertions robust no matter where the test
// temp dir lives (t.TempDir may itself sit inside a repository).
func reposUnder(repos []RepoSummary, root string) []string {
	out := make([]string, 0, len(repos))
	prefix := root + string(filepath.Separator)
	for _, repo := range repos {
		if repo.RepoRoot == root || strings.HasPrefix(repo.RepoRoot, prefix) {
			out = append(out, repo.RelPath)
		}
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestReposDiscoversNestedRepositories walks the superproject and expects it,
// its submodule and the unregistered nested repository — and nothing from the
// pruned node_modules decoy.
func TestReposDiscoversNestedRepositories(t *testing.T) {
	svc, root, paths := newRepoFixture(t)

	repos, err := svc.Repos(context.Background(), "codes/super")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"codes/super", "codes/super/extra/nested", "codes/super/modules/lib"}
	if got := reposUnder(repos, root); !equalStrings(got, want) {
		t.Fatalf("repos = %v, want %v", got, want)
	}
	for _, repo := range repos {
		if repo.Staged == nil || repo.Unstaged == nil || repo.Untracked == nil || repo.Conflicted == nil {
			t.Fatalf("repo %s has nil change slices: %+v", repo.RelPath, repo)
		}
		if repo.Name != filepath.Base(repo.RepoRoot) {
			t.Errorf("repo %s name = %q, want %q", repo.RelPath, repo.Name, filepath.Base(repo.RepoRoot))
		}
		if repo.Branch != "main" {
			t.Errorf("repo %s branch = %q, want main", repo.RelPath, repo.Branch)
		}
	}
	// The submodule must be recognized even though its .git entry is a file.
	if _, statErr := os.Stat(filepath.Join(paths["lib"], ".git")); statErr != nil {
		t.Fatalf("fixture submodule is missing its .git entry: %v", statErr)
	}
}

// TestReposListsSubmoduleDirectory checks that a submodule directory is a scan
// root of its own: its .git entry is a file, which is what hasGitEntry accepts.
func TestReposListsSubmoduleDirectory(t *testing.T) {
	svc, _, paths := newRepoFixture(t)

	repos, err := svc.Repos(context.Background(), filepath.Join("codes", "super", "modules", "lib"))
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 {
		t.Fatalf("got %d repos, want 1", len(repos))
	}
	if repos[0].RepoRoot != paths["lib"] {
		t.Fatalf("repo root = %q, want %q", repos[0].RepoRoot, paths["lib"])
	}
}

// TestReposHidesDotDirectories checks the walk never enters a hidden directory:
// repositories installed under ".hidden" (or any dot-prefixed name) are tool
// internals rather than the user's projects.
func TestReposHidesDotDirectories(t *testing.T) {
	svc, _, paths := newRepoFixture(t)

	repos, err := svc.Repos(context.Background(), filepath.Join("codes", "super"))
	if err != nil {
		t.Fatal(err)
	}
	for _, repo := range repos {
		if repo.RepoRoot == paths["hidden"] || repo.RepoRoot == paths["inner"] {
			t.Fatalf("hidden repository %s should not be listed", repo.RelPath)
		}
		for _, segment := range strings.Split(repo.RelPath, "/") {
			if strings.HasPrefix(segment, ".") {
				t.Fatalf("listed repository %s lives in a hidden directory", repo.RelPath)
			}
		}
	}
}

// TestReposListsHiddenRepositoryWhenTargeted covers the exemption: the scan root
// itself is never filtered, so navigating into a repository that lives under a
// dot-prefixed directory still lists it.
func TestReposListsHiddenRepositoryWhenTargeted(t *testing.T) {
	svc, _, paths := newRepoFixture(t)

	for _, target := range []string{
		filepath.Join("codes", ".hidden", "repo"),
		filepath.Join("codes", ".hidden", "repo", "README.md"),
	} {
		repos, err := svc.Repos(context.Background(), target)
		if err != nil {
			t.Fatalf("%s: %v", target, err)
		}
		found := false
		for _, repo := range repos {
			if repo.RepoRoot == paths["hidden"] {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s: hidden repository %s was not listed (got %v)", target, paths["hidden"], repos)
		}
	}

	// The directory holding it is not a repository root, so it lists nothing.
	repos, err := svc.Repos(context.Background(), filepath.Join("codes", ".hidden"))
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 0 {
		t.Fatalf("a plain directory listed %v, want none", repos)
	}
}

// TestReposListsNothingOutsideARepository checks discovery when the requested
// path is not itself a repository root: nothing is listed and the directory is
// not walked, so opening the data root no longer scans the whole home directory.
func TestReposListsNothingOutsideARepository(t *testing.T) {
	svc, root, _ := newRepoFixture(t)

	for _, target := range []string{"", "codes", filepath.Join("codes", "super", "extra")} {
		repos, err := svc.Repos(context.Background(), target)
		if err != nil {
			t.Fatalf("%s: %v", target, err)
		}
		if got := reposUnder(repos, root); len(got) != 0 {
			t.Fatalf("%s: repos = %v, want none", target, got)
		}
	}
}

// TestReposRespectsDepthLimit checks the walk stops below maxRepoDepth. The scan
// root is itself a repository, which is what starts the walk at all.
func TestReposRespectsDepthLimit(t *testing.T) {
	root := t.TempDir()
	svc := New(root, 30*time.Second)

	top := filepath.Join(root, "top")
	initRepo(t, top)
	deep := filepath.Join(root, "top", "a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "deep")
	initRepo(t, deep)

	repos, err := svc.Repos(context.Background(), "top")
	if err != nil {
		t.Fatal(err)
	}
	got := reposUnder(repos, root)
	if !equalStrings(got, []string{"top"}) {
		t.Fatalf("repos = %v, want [top] (deep repo is beyond the depth limit)", got)
	}
}

// TestReposEmptyResultIsNonNil guards the JSON contract: an empty discovery
// must encode as [] so the web client can safely call .length/.map.
func TestReposEmptyResultIsNonNil(t *testing.T) {
	root := t.TempDir()
	svc := New(root, 30*time.Second)

	repos, err := svc.Repos(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if repos == nil {
		t.Fatal("Repos must return a non-nil slice")
	}
	if got := reposUnder(repos, root); len(got) != 0 {
		t.Fatalf("repos under root = %v, want none", got)
	}
	blob, err := json.Marshal(repos)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), "null") {
		t.Fatalf("repos JSON must not contain null: %s", blob)
	}
}

// TestReposSkipsUnreadableRepository checks a repository that is discovered but
// cannot be read is skipped instead of failing the whole discovery.
func TestReposSkipsUnreadableRepository(t *testing.T) {
	svc, root, paths := newRepoFixture(t)

	// Replace the superproject's gitdir with an empty directory: the walk still
	// sees a .git entry (so the repo is discovered), but `git status` fails
	// there, and the submodule pointing into that gitdir fails too.
	if err := os.RemoveAll(filepath.Join(paths["super"], ".git")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(paths["super"], ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	repos, err := svc.Repos(context.Background(), filepath.Join("codes", "super"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"codes/super/extra/nested"}
	if got := reposUnder(repos, root); !equalStrings(got, want) {
		t.Fatalf("repos = %v, want %v", got, want)
	}
	for _, repo := range repos {
		if repo.RepoRoot == paths["super"] || repo.RepoRoot == paths["lib"] {
			t.Fatalf("unreadable repository %s should have been skipped", repo.RelPath)
		}
	}
}
