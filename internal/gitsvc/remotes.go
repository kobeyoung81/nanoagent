package gitsvc

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

// Remote describes one configured remote.
type Remote struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	Push string `json:"push,omitempty"`
}

// Remotes lists configured remotes.
func (s *Service) Remotes(ctx context.Context, path string) ([]Remote, error) {
	root, err := s.repoRoot(ctx, path)
	if err != nil {
		return nil, err
	}
	out, err := s.run(ctx, root, "remote", "-v")
	if err != nil {
		return nil, fmt.Errorf("git remote failed: %w", err)
	}
	var remotes []Remote
	seen := map[string]int{}
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 3 {
			continue
		}
		name, url, kind := parts[0], parts[1], parts[2]
		if idx, ok := seen[name]; ok {
			if kind == "(push)" {
				remotes[idx].Push = url
			}
			continue
		}
		r := Remote{Name: name, URL: url}
		if kind == "(push)" {
			r.Push = url
		}
		remotes = append(remotes, r)
		seen[name] = len(remotes) - 1
	}
	return nonNil(remotes), nil
}

// AddRemote adds (or updates) a remote.
func (s *Service) AddRemote(ctx context.Context, path, name, url string) error {
	root, err := s.repoRoot(ctx, path)
	if err != nil {
		return err
	}
	if name == "" || url == "" {
		return fmt.Errorf("remote name and url are required")
	}
	if !validURL(url) {
		return fmt.Errorf("unsupported remote URL: only http(s) and ssh URLs are allowed")
	}
	// If the remote already exists, update its URL.
	if _, err := s.run(ctx, root, "remote", "get-url", name); err == nil {
		if _, err := s.run(ctx, root, "remote", "set-url", name, url); err != nil {
			return fmt.Errorf("git remote set-url failed: %w", err)
		}
		return nil
	}
	if _, err := s.run(ctx, root, "remote", "add", name, url); err != nil {
		return fmt.Errorf("git remote add failed: %w", err)
	}
	return nil
}

// RemoveRemote removes a remote.
func (s *Service) RemoveRemote(ctx context.Context, path, name string) error {
	root, err := s.repoRoot(ctx, path)
	if err != nil {
		return err
	}
	if name == "" {
		return fmt.Errorf("remote name is required")
	}
	if _, err := s.run(ctx, root, "remote", "remove", name); err != nil {
		return fmt.Errorf("git remote remove failed: %w", err)
	}
	return nil
}

// Fetch fetches from a remote (default origin).
func (s *Service) Fetch(ctx context.Context, path, remote string, prune bool) error {
	root, err := s.repoRoot(ctx, path)
	if err != nil {
		return err
	}
	if remote == "" {
		remote = "origin"
	}
	args := []string{"fetch", remote}
	if prune {
		args = append(args, "--prune")
	}
	if _, err := s.run(ctx, root, args...); err != nil {
		return fmt.Errorf("git fetch failed: %w", err)
	}
	return nil
}

// PushOpts configures a push.
type PushOpts struct {
	Remote      string `json:"remote"`
	Branch      string `json:"branch"`
	SetUpstream bool   `json:"set_upstream"`
	Force       bool   `json:"force"`
}

// Push pushes branch to remote (or pulls the current branch when unset).
func (s *Service) Push(ctx context.Context, path string, o PushOpts) error {
	root, err := s.repoRoot(ctx, path)
	if err != nil {
		return err
	}
	if o.Remote == "" {
		o.Remote = "origin"
	}
	args := []string{"push"}
	if o.Force {
		args = append(args, "--force-with-lease")
	}
	if o.SetUpstream {
		args = append(args, "-u")
	}
	args = append(args, o.Remote)
	if o.Branch != "" {
		args = append(args, o.Branch)
	}
	if _, err := s.run(ctx, root, args...); err != nil {
		return fmt.Errorf("git push failed: %w", err)
	}
	return nil
}

// Pull pulls from the upstream (default origin + current branch). When rebase
// is set the pull is performed with --rebase.
func (s *Service) Pull(ctx context.Context, path, remote, branch string, rebase bool) error {
	root, err := s.repoRoot(ctx, path)
	if err != nil {
		return err
	}
	args := []string{"pull"}
	if rebase {
		args = append(args, "--rebase")
	}
	if remote == "" {
		remote = "origin"
	}
	args = append(args, remote)
	if branch != "" {
		args = append(args, branch)
	}
	if _, err := s.run(ctx, root, args...); err != nil {
		return fmt.Errorf("git pull failed: %w", err)
	}
	return nil
}

// LastFetchState reports when FETCH_HEAD was last written as an advisory
// "last sync" indicator, plus the agent name it was written for.
type LastFetchState struct {
	At  string `json:"at,omitempty"`
	For string `json:"for,omitempty"`
}

// LastFetch returns the mtime of FETCH_HEAD (empty when never fetched).
func (s *Service) LastFetch(ctx context.Context, path string) (*LastFetchState, error) {
	root, err := s.repoRoot(ctx, path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(root + "/.git/FETCH_HEAD")
	if err != nil {
		return &LastFetchState{}, nil
	}
	return &LastFetchState{At: info.ModTime().UTC().Format(time.RFC3339)}, nil
}

// validURL is a stricter remote validation (no scp-like shorthand for remotes
// set via API, matching Clone).
func validURL(raw string) bool {
	if strings.HasPrefix(raw, "https://") || strings.HasPrefix(raw, "http://") {
		return true
	}
	if strings.HasPrefix(raw, "ssh://") {
		return true
	}
	if strings.HasPrefix(raw, "git@") && strings.Contains(raw, ":") {
		return true
	}
	return false
}
