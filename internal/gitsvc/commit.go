package gitsvc

import (
	"context"
	"fmt"
	"strings"
)

// StageFiles stages the given paths (relative to the repo root) in the
// repository containing repoPath. An empty files slice stages everything.
// rgx supports simple globs (git pathspec uses fnmatch by default).
func (s *Service) StageFiles(ctx context.Context, path string, files []string) error {
	root, err := s.repoRoot(ctx, path)
	if err != nil {
		return err
	}
	args := []string{"add", "--"}
	if len(files) == 0 {
		args = append(args, ".")
	} else {
		for _, f := range files {
			resolved, rerr := s.resolveInRepo(root, f)
			if rerr != nil {
				return rerr
			}
			args = append(args, resolved)
		}
	}
	if _, err := s.run(ctx, root, args...); err != nil {
		return fmt.Errorf("git add failed: %w", err)
	}
	return nil
}

// UnstageFiles moves the given paths from the index back to the working tree.
func (s *Service) UnstageFiles(ctx context.Context, path string, files []string) error {
	root, err := s.repoRoot(ctx, path)
	if err != nil {
		return err
	}
	args := []string{"reset", "--", "HEAD"}
	if len(files) == 0 {
		args = append(args, ".")
	} else {
		for _, f := range files {
			resolved, rerr := s.resolveInRepo(root, f)
			if rerr != nil {
				return rerr
			}
			args = append(args, resolved)
		}
	}
	if _, err := s.run(ctx, root, args...); err != nil {
		return fmt.Errorf("git reset failed: %w", err)
	}
	return nil
}

// DiscardFiles reverts working-tree and index changes of the given paths to
// HEAD, removing untracked files. It is destructive and must be confirmed by
// the caller.
func (s *Service) DiscardFiles(ctx context.Context, path string, files []string) error {
	root, err := s.repoRoot(ctx, path)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no files to discard")
	}
	resolved := make([]string, 0, len(files))
	for _, f := range files {
		r, rerr := s.resolveInRepo(root, f)
		if rerr != nil {
			return rerr
		}
		resolved = append(resolved, r)
	}

	// 1. Unstage everything requested (harmless no-op for clean/index paths).
	resetArgs := append([]string{"reset", "-q", "HEAD", "--"}, resolved...)
	_, _ = s.run(ctx, root, resetArgs...)

	// 2. Restore tracked files from HEAD; remove the rest with git clean
	// (git clean refuses nested repositories, mirroring git clean -fd).
	for _, f := range resolved {
		out, err := s.run(ctx, root, "ls-files", "--error-unmatch", "--", f)
		if err == nil && strings.TrimSpace(string(out)) != "" {
			if _, err := s.run(ctx, root, "checkout", "--", f); err != nil {
				return fmt.Errorf("git checkout -- %q failed: %w", f, err)
			}
			continue
		}
		if _, err := s.run(ctx, root, "clean", "-fd", "--", f); err != nil {
			// A pathspec that matches nothing errors; nothing else to do.
			if !strings.Contains(err.Error(), "did not match") {
				return fmt.Errorf("git clean -- %q failed: %w", f, err)
			}
		}
	}
	return nil
}

// CommitOpts configures a commit.
type CommitOpts struct {
	Message string `json:"message"`
	Amend   bool   `json:"amend"`
	SignOff bool   `json:"signoff"`
	All     bool   `json:"all"`
	Empty   bool   `json:"allow_empty"`
}

// Commit creates a commit in the repository containing path. When All is set
// it commits all tracked modifications (git commit -a semantics). When
// AllowEmpty is not set, committing with nothing staged returns a clear
// error.
func (s *Service) Commit(ctx context.Context, path string, o CommitOpts) error {
	root, err := s.repoRoot(ctx, path)
	if err != nil {
		return err
	}
	if strings.TrimSpace(o.Message) == "" {
		return fmt.Errorf("commit message is required")
	}
	args := []string{"commit"}
	if o.All {
		// Stage everything (tracked modifications, deletions, untracked files)
		// before committing — matching "Commit All" semantics in GUIs.
		if _, err := s.run(ctx, root, "add", "-A"); err != nil {
			return fmt.Errorf("git add -A failed: %w", err)
		}
	}
	if o.Amend {
		args = append(args, "--amend")
	}
	if o.Empty {
		args = append(args, "--allow-empty")
	}
	if o.SignOff {
		args = append(args, "-s")
	}
	// Message via -m (two -m for subject + body when newline present).
	if idx := strings.IndexByte(o.Message, '\n'); idx >= 0 {
		args = append(args, "-m", strings.TrimSpace(o.Message[:idx]), "-m", strings.TrimSpace(o.Message[idx+1:]))
	} else {
		args = append(args, "-m", strings.TrimSpace(o.Message))
	}
	out, err := s.run(ctx, root, args...)
	if err != nil {
		msg := string(out)
		if strings.Contains(msg, "nothing to commit") || strings.Contains(msg, "no changes added") {
			return fmt.Errorf("nothing to commit (no staged changes)")
		}
		if strings.Contains(msg, "Please tell me who you are") || strings.Contains(msg, "user.name") {
			return fmt.Errorf("git user.name/user.email not configured in the container")
		}
		return fmt.Errorf("git commit failed: %v: %s", err, strings.TrimSpace(msg))
	}
	return nil
}

// LastCommit returns the most recent commit of the current branch.
func (s *Service) LastCommit(ctx context.Context, path string) (*LogEntry, error) {
	page, err := s.Log(ctx, path, "", 1, 0)
	if err != nil {
		return nil, err
	}
	if len(page.Commits) == 0 {
		return &LogEntry{}, nil
	}
	return &page.Commits[0], nil
}
