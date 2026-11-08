package gitsvc

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ChangeStatus describes one changed path in a repository.
type ChangeStatus struct {
	Path   string `json:"path"`
	XY     string `json:"xy"` // XY codes (X=index, Y=worktree)
	Staged bool   `json:"staged"`
	IsDir  bool   `json:"is_dir,omitempty"`
}

// Status is the parsed result of `git status --porcelain`.
type Status struct {
	RepoRoot   string         `json:"repo_root"`
	Path       string         `json:"path"`
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

// Status runs `git status --porcelain` for the repository containing path and
// parses the result into a structured Status.
func (s *Service) Status(ctx context.Context, path string) (*Status, error) {
	root, err := s.repoRoot(ctx, path)
	if err != nil {
		return nil, err
	}

	out, err := s.run(ctx, root, "status", "--porcelain=v1", "-b", "-z", "--untracked-files=all")
	if err != nil {
		return nil, fmt.Errorf("git status failed: %w", err)
	}

	st := &Status{RepoRoot: root, Path: path}
	entry, err := parsePorcelain(out, root)
	if err != nil {
		return nil, err
	}
	st.Branch = entry.Branch
	st.Upstream = entry.Upstream
	st.Ahead = entry.Ahead
	st.Behind = entry.Behind
	st.Detached = entry.Detached
	// The porcelain header does not reliably carry merge/rebase state (the
	// "|MERGING"/"|REBASING" suffix only appears in human short output), so
	// detect in-progress operations from the git dir state files instead.
	mergeInP, rebaseInP := s.operationState(ctx, root)
	st.MergeInP = mergeInP
	st.RebaseInP = rebaseInP
	st.Staged = entry.Staged
	st.Unstaged = entry.Unstaged
	st.Untracked = entry.Untracked
	st.Conflicted = entry.Conflicted

	sortSet(st.Staged)
	sortSet(st.Unstaged)
	sortSet(st.Untracked)
	sortSet(st.Conflicted)
	// Emit [] rather than null for empty categories.
	st.Staged = nonNil(st.Staged)
	st.Unstaged = nonNil(st.Unstaged)
	st.Untracked = nonNil(st.Untracked)
	st.Conflicted = nonNil(st.Conflicted)
	return st, nil
}

// operationState detects an in-progress merge or rebase from .git state files.
func (s *Service) operationState(ctx context.Context, root string) (merge, rebase bool) {
	gitDir := ""
	if out, err := s.run(ctx, root, "rev-parse", "--git-dir"); err == nil {
		gitDir = strings.TrimSpace(string(out))
	}
	if gitDir == "" || strings.HasPrefix(gitDir, "-") {
		return false, false
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(root, gitDir)
	}
	if _, err := os.Stat(filepath.Join(gitDir, "MERGE_HEAD")); err == nil {
		merge = true
	}
	if _, err := os.Stat(filepath.Join(gitDir, "rebase-merge")); err == nil {
		rebase = true
	}
	if _, err := os.Stat(filepath.Join(gitDir, "rebase-apply")); err == nil {
		rebase = true
	}
	return merge, rebase
}

func sortSet(set []ChangeStatus) {
	sort.Slice(set, func(i, j int) bool {
		return set[i].Path < set[j].Path
	})
}

type porcelainEntry struct {
	Branch     string
	Upstream   string
	Ahead      int
	Behind     int
	Detached   bool
	MergeInP   bool
	RebaseInP  bool
	Staged     []ChangeStatus
	Unstaged   []ChangeStatus
	Untracked  []ChangeStatus
	Conflicted []ChangeStatus
}

// parsePorcelain parses `git status --porcelain=v1 -z --branch` output.
// With -z the first field is the branch header ("## master...origin/master
// [ahead 1, behind 2]"), each entry is "XY <path>", and rename/copy entries
// are "XY <old>" followed by a separate NUL field holding the destination.
func parsePorcelain(out []byte, root string) (porcelainEntry, error) {
	var e porcelainEntry
	if root == "" {
		root = "."
	}
	fields := strings.Split(string(out), "\x00")
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if f == "" {
			continue
		}
		if strings.HasPrefix(f, "##") {
			parseBranchHeader(f[2:], &e)
			continue
		}
		if len(f) < 4 { // "XY " + at least one path char
			continue
		}
		xy := f[:2]
		path := strings.TrimLeft(f[2:], " ")
		// Rename/copy destination follows as its own NUL field.
		if (xy[0] == 'R' || xy[0] == 'C') && i+1 < len(fields) && fields[i+1] != "" {
			path = path + "\x00=>\x00" + fields[i+1]
			i++
		}
		st := ChangeStatus{XY: xy, Path: path, Staged: xy[0] != ' ' && xy[0] != '?' && xy[0] != 'U'}
		idx, wt := xy[0], xy[1]
		isConflict := idx == 'U' || wt == 'U' || (idx == 'A' && wt == 'A') || (idx == 'D' && wt == 'D')
		isStaged := idx != ' ' && idx != '?' && idx != '.' && idx != 'U'
		isWorkTree := wt != ' ' && wt != '?' && wt != '.' && wt != 'U'
		switch {
		case isConflict:
			e.Conflicted = append(e.Conflicted, st)
		default:
			if isStaged {
				e.Staged = append(e.Staged, st)
			}
			if isWorkTree {
				e.Unstaged = append(e.Unstaged, st)
			}
			if idx == '?' || wt == '?' {
				e.Untracked = append(e.Untracked, st)
			}
		}
	}
	// Attach IsDir based on the first path component of each change.
	for i := range e.Staged {
		e.Staged[i].IsDir = isDirChecked(root, firstPath(e.Staged[i].Path))
	}
	for i := range e.Unstaged {
		e.Unstaged[i].IsDir = isDirChecked(root, firstPath(e.Unstaged[i].Path))
	}
	for i := range e.Conflicted {
		e.Conflicted[i].IsDir = isDirChecked(root, firstPath(e.Conflicted[i].Path))
	}
	for i := range e.Untracked {
		e.Untracked[i].IsDir = isDirChecked(root, firstPath(e.Untracked[i].Path))
	}
	return e, nil
}

// firstPath returns the source path of a change (the part before a rename
// marker if present).
func firstPath(p string) string {
	if idx := strings.Index(p, "\x00=>\x00"); idx >= 0 {
		return p[:idx]
	}
	return p
}

func parseBranchHeader(h string, e *porcelainEntry) {
	original := h
	h = strings.TrimSpace(h)
	// Strip merge/rebase state suffix ("|MERGING", "|REBASING 2/3").
	if idx := strings.Index(h, "|"); idx >= 0 {
		state := h[idx+1:]
		h = h[:idx]
		if strings.HasPrefix(state, "MERGING") {
			e.MergeInP = true
		} else if strings.HasPrefix(state, "REBAS") || strings.HasPrefix(state, "REBASE") {
			e.RebaseInP = true
		}
	}
	if h == "HEAD" || strings.HasPrefix(h, "HEAD ") {
		e.Detached = true
	}
	_ = original
	// Split off the "[ahead 1, behind 2]" meta.
	if idx := strings.Index(h, " ["); idx >= 0 {
		parseAheadBehind(h[idx+1:], e)
		h = h[:idx]
	}
	if idx := strings.Index(h, "..."); idx >= 0 {
		e.Branch = strings.TrimSpace(h[:idx])
		e.Upstream = strings.TrimSpace(h[idx+3:])
	} else {
		e.Branch = strings.TrimSpace(h)
	}
}

func parseAheadBehind(meta string, e *porcelainEntry) {
	meta = strings.Trim(meta, " []")
	for _, part := range strings.Split(meta, ",") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "ahead ") {
			if n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(part, "ahead "))); err == nil {
				e.Ahead = n
			}
		} else if strings.HasPrefix(part, "behind ") {
			if n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(part, "behind "))); err == nil {
				e.Behind = n
			}
		}
	}
}

// isDirChecked reports whether the changed path (relative to repo root) is a
// directory according to the current filesystem.
func isDirChecked(root, path string) bool {
	full := filepath.Join(root, path)
	info, err := os.Stat(full)
	if err != nil {
		return false
	}
	return info.IsDir()
}

// RepoInfo returns lightweight repo identity for the repository containing path.
type RepoInfo struct {
	RepoRoot string `json:"repo_root"`
	Branch   string `json:"branch"`
	Upstream string `json:"upstream,omitempty"`
	Ahead    int    `json:"ahead"`
	Behind   int    `json:"behind"`
	Detached bool   `json:"detached"`
}

// RepoInfo resolves the repo root and branch state for a path.
func (s *Service) RepoInfo(ctx context.Context, path string) (*RepoInfo, error) {
	root, err := s.repoRoot(ctx, path)
	if err != nil {
		return nil, err
	}
	st, err := s.Status(ctx, root)
	if err != nil {
		return nil, err
	}
	return &RepoInfo{
		RepoRoot: root,
		Branch:   st.Branch,
		Upstream: st.Upstream,
		Ahead:    st.Ahead,
		Behind:   st.Behind,
		Detached: st.Detached,
	}, nil
}
