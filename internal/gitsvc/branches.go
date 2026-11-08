package gitsvc

import (
	"context"
	"fmt"
	"strings"
)

// Branch is one branch of the repository.
type Branch struct {
	Name      string `json:"name"`
	Current   bool   `json:"current"`
	Remote    bool   `json:"remote"`
	Tracking  string `json:"tracking,omitempty"`
	Behind    int    `json:"behind,omitempty"`
	CommitSha string `json:"commit,omitempty"`
}

// BranchList returns local and remote branches.
func (s *Service) Branches(ctx context.Context, path string) ([]Branch, error) {
	root, err := s.repoRoot(ctx, path)
	if err != nil {
		return nil, err
	}
	head, err := s.run(ctx, root, "symbolic-ref", "--short", "HEAD")
	current := strings.TrimSpace(string(head))
	if err != nil {
		// Detached HEAD: fall back to the short sha.
		if out, e := s.run(ctx, root, "rev-parse", "--short", "HEAD"); e == nil {
			current = strings.TrimSpace(string(out))
		}
	}

	format := "%(refname:short)" + fldSep + "%(objectname:short)"
	out, err := s.run(ctx, root, "for-each-ref", "--format="+format, "refs/heads", "refs/remotes")
	if err != nil {
		return nil, fmt.Errorf("git for-each-ref failed: %w", err)
	}

	var branches []Branch
	for _, rec := range strings.Split(string(out), "\n") {
		rec = strings.TrimSuffix(rec, "\n")
		if rec == "" {
			continue
		}
		parts := strings.SplitN(rec, fldSep, 2)
		if len(parts) != 2 {
			continue
		}
		b := Branch{
			Name:      parts[0],
			Current:   parts[0] == current,
			Remote:    strings.HasPrefix(parts[0], "remotes/"),
			CommitSha: parts[1],
		}
		if !b.Remote {
			if track, err := s.run(ctx, root, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}"); err == nil {
				tracking := strings.TrimSpace(string(track))
				if tracking != "" && tracking != parts[0] {
					b.Tracking = tracking
				}
			}
		}
		branches = append(branches, b)
	}
	return nonNil(branches), nil
}

// CreateBranch creates a branch at startPoint (default HEAD).
func (s *Service) CreateBranch(ctx context.Context, path, name, startPoint string) error {
	root, err := s.repoRoot(ctx, path)
	if err != nil {
		return err
	}
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("branch name is required")
	}
	args := []string{"branch", name}
	if startPoint != "" {
		args = append(args, startPoint)
	}
	if _, err := s.run(ctx, root, args...); err != nil {
		return fmt.Errorf("git branch failed: %w", err)
	}
	return nil
}

// Checkout switches to ref. When create is set the branch is created first
// (git checkout -b).
func (s *Service) Checkout(ctx context.Context, path, ref string, create bool) error {
	root, err := s.repoRoot(ctx, path)
	if err != nil {
		return err
	}
	if strings.TrimSpace(ref) == "" {
		return fmt.Errorf("ref is required")
	}
	args := []string{"checkout"}
	if create {
		args = append(args, "-b")
	}
	args = append(args, ref)
	if _, err := s.run(ctx, root, args...); err != nil {
		return fmt.Errorf("git checkout failed: %w", err)
	}
	return nil
}

// DeleteBranch deletes a branch, requiring force for unmerged branches.
func (s *Service) DeleteBranch(ctx context.Context, path, name string, force bool) error {
	root, err := s.repoRoot(ctx, path)
	if err != nil {
		return err
	}
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("branch name is required")
	}
	args := []string{"branch"}
	if force {
		args = append(args, "-D")
	} else {
		args = append(args, "-d")
	}
	args = append(args, name)
	if _, err := s.run(ctx, root, args...); err != nil {
		return fmt.Errorf("git branch -d failed: %w", err)
	}
	return nil
}

// Merge merges ref into the current branch.
func (s *Service) Merge(ctx context.Context, path, ref, message string) error {
	root, err := s.repoRoot(ctx, path)
	if err != nil {
		return err
	}
	args := []string{"merge"}
	if message != "" {
		args = append(args, "-m", message)
	}
	args = append(args, ref)
	out, err := s.run(ctx, root, args...)
	if err != nil {
		if strings.Contains(strings.ToLower(string(out)), "conflict") {
			return fmt.Errorf("merge conflicts: %v", err)
		}
		return fmt.Errorf("git merge failed: %w", err)
	}
	return nil
}

// Rebase rebases the current branch onto ref.
func (s *Service) Rebase(ctx context.Context, path, ref string) error {
	root, err := s.repoRoot(ctx, path)
	if err != nil {
		return err
	}
	args := []string{"rebase", ref}
	if _, err := s.run(ctx, root, args...); err != nil {
		return fmt.Errorf("git rebase failed: %w", err)
	}
	return nil
}

// AbortMergeRebase aborts an in-progress merge or rebase.
func (s *Service) AbortMergeRebase(ctx context.Context, path, kind string) error {
	root, err := s.repoRoot(ctx, path)
	if err != nil {
		return err
	}
	switch kind {
	case "merge":
		_, err = s.run(ctx, root, "merge", "--abort")
	case "rebase":
		_, err = s.run(ctx, root, "rebase", "--abort")
	default:
		return fmt.Errorf("kind must be merge or rebase")
	}
	if err != nil {
		return fmt.Errorf("git %s --abort failed: %w", kind, err)
	}
	return nil
}

// ResolveOpts resolves a conflicted file.
type ResolveOpts struct {
	File     string `json:"file"`
	Strategy string `json:"strategy"`      // ours | theirs | both
	Ref      string `json:"ref,omitempty"` // for "theirs": source ref when not resolving against MERGE_HEAD
}

// Resolve resolves a conflicted file using the given strategy and stages it.
func (s *Service) Resolve(ctx context.Context, path string, o ResolveOpts) error {
	root, err := s.repoRoot(ctx, path)
	if err != nil {
		return err
	}
	file, rerr := s.resolveInRepo(root, o.File)
	if rerr != nil {
		return rerr
	}
	switch o.Strategy {
	case "ours":
		if _, err := s.run(ctx, root, "checkout", "--ours", "--", file); err != nil {
			return fmt.Errorf("git checkout --ours failed: %w", err)
		}
	case "theirs":
		if _, err := s.run(ctx, root, "checkout", "--theirs", "--", file); err != nil {
			return fmt.Errorf("git checkout --theirs failed: %w", err)
		}
	case "both":
		// Take both sides: checkout theirs, then merge ours on top is complex;
		// instead instruct by leaving the file with conflict markers, which the
		// user resolves in the editor. "both" stages nothing automatically.
		return fmt.Errorf("both strategy is manual: edit the file and stage it")
	default:
		return fmt.Errorf("strategy must be ours or theirs")
	}
	// Mark as resolved by staging.
	if _, err := s.run(ctx, root, "add", "--", file); err != nil {
		return fmt.Errorf("git add (resolve) failed: %w", err)
	}
	return nil
}
