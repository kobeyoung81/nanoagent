package gitsvc

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Limits for the repository discovery walk. The walk is depth- and count-
// bounded so browsing a very large directory cannot stall the request.
const (
	maxRepoDepth = 8
	maxRepoCount = 50
)

// repoPrune lists directory names the discovery walk never descends into: VCS
// internals, dependency trees and build output. It mirrors the prune set used
// by the path-suggestion endpoint.
var repoPrune = map[string]bool{
	".git": true, "node_modules": true, ".cache": true,
	".venv": true, "venv": true, "target": true, "dist": true,
	"__pycache__": true, ".npm": true, "media": true,
}

// RepoSummary describes one discovered repository together with its branch
// state and working-tree changes, so a single request can populate the whole
// multi-repository view. RepoRoot is an absolute path and is accepted verbatim
// by the other git operations (it is the same value Status reports).
type RepoSummary struct {
	RepoRoot   string         `json:"repo_root"`
	RelPath    string         `json:"rel_path"`
	Name       string         `json:"name"`
	Branch     string         `json:"branch"`
	Upstream   string         `json:"upstream,omitempty"`
	Ahead      int            `json:"ahead"`
	Behind     int            `json:"behind"`
	Detached   bool           `json:"detached"`
	MergeInP   bool           `json:"merge_in_progress"`
	RebaseInP  bool           `json:"rebase_in_progress"`
	Staged     []ChangeStatus `json:"staged"`
	Unstaged   []ChangeStatus `json:"unstaged"`
	Untracked  []ChangeStatus `json:"untracked"`
	Conflicted []ChangeStatus `json:"conflicted"`
}

// Repos discovers every git repository at or under path — including nested
// repositories such as submodules — and returns each one's state. path may
// point at a file, a directory, or be empty (the data root).
//
// Discovery rules:
//   - only a directory that is itself a repository root is scanned: the selected
//     path owns what the panel shows, so a plain directory — the data root, or a
//     subdirectory inside a repository — yields an empty list instead of a walk
//     over everything below it (walking the data root used to spend one
//     `git status` per repository found anywhere under it);
//   - a directory counts as a repository root when it holds a .git entry (a
//     directory for ordinary repositories, a file for submodules/worktrees), so
//     opening a submodule directory lists that submodule;
//   - the walk keeps descending into a found repository root so submodules
//     nested below it are discovered too, but skips repoPrune directories;
//   - hidden directories (a name starting with a dot, e.g. ".nvm") are never
//     descended into, so tool-internal repositories are not listed; the scan
//     root itself is exempt, so a path pointing inside such a directory still
//     lists whatever repository is found there;
//   - at most maxRepoCount repositories are returned and the walk stops
//     maxRepoDepth levels below the scan root.
//
// Repositories whose status cannot be read are skipped rather than failing the
// whole call, so one broken repository never blanks the panel.
func (s *Service) Repos(ctx context.Context, path string) ([]RepoSummary, error) {
	base, err := s.resolve(path)
	if err != nil {
		return nil, err
	}
	if info, statErr := os.Stat(base); statErr != nil || !info.IsDir() {
		base = filepath.Dir(base)
	}

	// Only a directory that is itself a repository root is scanned. A plain
	// directory — the data root, or a subdirectory inside a repository — has
	// nothing to list: walking it would scan the whole home directory and spend
	// a `git status` on every repository found below.
	if !hasGitEntry(base) {
		return []RepoSummary{}, nil
	}

	roots := discoverRepos(base)
	sort.Strings(roots)

	out := make([]RepoSummary, 0, len(roots))
	for _, root := range roots {
		status, statusErr := s.Status(ctx, root)
		if statusErr != nil {
			continue
		}
		out = append(out, summarizeRepo(root, s.dataRoot, status))
	}
	return nonNil(out), nil
}

// discoverRepos walks root and returns the absolute path of every repository
// found at or below it, bounded by maxRepoDepth and maxRepoCount.
func discoverRepos(root string) []string {
	roots := make([]string, 0, 4)
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if len(roots) >= maxRepoCount || depth > maxRepoDepth {
			return
		}
		if hasGitEntry(dir) {
			roots = append(roots, dir)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, entry := range entries {
			name := entry.Name()
			// Hidden directories are tool internals rather than the user's
			// projects (".nvm", ".cache", ".config", ...), so the walk never
			// enters them. The scan root itself is not filtered here, so
			// browsing into a hidden directory still lists its repository.
			if !entry.IsDir() || repoPrune[name] || strings.HasPrefix(name, ".") {
				continue
			}
			walk(filepath.Join(dir, name), depth+1)
		}
	}
	walk(root, 0)
	return roots
}

// hasGitEntry reports whether dir is a repository root. A .git directory marks
// an ordinary repository; a .git file marks a submodule or a linked worktree.
func hasGitEntry(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil
}

// summarizeRepo builds the API view of one repository. The display path is the
// repository root relative to dataRoot (slashes normalized).
func summarizeRepo(root, dataRoot string, status *Status) RepoSummary {
	rel := root
	if r, err := filepath.Rel(dataRoot, root); err == nil {
		rel = filepath.ToSlash(r)
	}
	return RepoSummary{
		RepoRoot:   root,
		RelPath:    rel,
		Name:       filepath.Base(root),
		Branch:     status.Branch,
		Upstream:   status.Upstream,
		Ahead:      status.Ahead,
		Behind:     status.Behind,
		Detached:   status.Detached,
		MergeInP:   status.MergeInP,
		RebaseInP:  status.RebaseInP,
		Staged:     status.Staged,
		Unstaged:   status.Unstaged,
		Untracked:  status.Untracked,
		Conflicted: status.Conflicted,
	}
}
