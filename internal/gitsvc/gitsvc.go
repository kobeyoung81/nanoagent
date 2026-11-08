// Package gitsvc runs git commands against repositories rooted in the user's
// data directory (the agent container's user home). It confines all operations
// to resolved, canonical paths inside the data root and executes git strictly
// via argv (no shell interpolation), mirroring the security posture of the
// workspace manager.
package gitsvc

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ErrNotARepository is returned when a path is not inside a git repository.
var ErrNotARepository = fmt.Errorf("not a git repository")

// nonNil returns an empty (non-nil) slice when s is nil. JSON encodes a nil
// slice as null, which forces every API consumer to null-check; list payloads
// are always emitted as [] instead.
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// Service runs git operations confined to a data root (user home).
type Service struct {
	dataRoot string
	timeout  time.Duration
}

// New returns a Service rooted at dataRoot with a per-command timeout.
func New(dataRoot string, timeout time.Duration) *Service {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &Service{dataRoot: dataRoot, timeout: timeout}
}

// DataRoot returns the confinement root.
func (s *Service) DataRoot() string {
	return s.dataRoot
}

// ResolvePath returns the canonical absolute path for a data-root-relative
// path, verifying it stays inside the data root (symlinks resolved). Empty
// resolves to the data root. This is the shared containment gate used by the
// browse endpoints (list/suggest/blob).
func (s *Service) ResolvePath(path string) (string, error) {
	return s.resolve(path)
}

// resolve canonicalizes path inside the data root. An empty path resolves to
// the data root itself. Symlinks are resolved so a link cannot escape.
func (s *Service) resolve(path string) (string, error) {
	root, err := canonical(s.dataRoot)
	if err != nil {
		return "", fmt.Errorf("resolve data root: %w", err)
	}
	if path == "" {
		return root, nil
	}
	joined := path
	if !filepath.IsAbs(joined) {
		joined = filepath.Join(s.dataRoot, joined)
	}
	absPath, err := filepath.Abs(joined)
	if err != nil {
		return "", fmt.Errorf("invalid path %q: %w", path, err)
	}
	canon, err := canonical(absPath)
	if err != nil {
		return "", fmt.Errorf("resolve path %q: %w", path, err)
	}
	if !within(canon, root) {
		return "", fmt.Errorf("access denied: path %q is outside the data root", path)
	}
	return absPath, nil
}

// repoRoot resolves the git repository root containing path (which may point
// at a file or directory inside a repo). Returns ErrNotARepository for paths
// not inside a repository.
func (s *Service) repoRoot(ctx context.Context, path string) (string, error) {
	resolved, err := s.resolve(path)
	if err != nil {
		return "", err
	}
	out, err := s.run(ctx, resolved, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", ErrNotARepository
	}
	root := strings.TrimSpace(string(out))
	if root == "" {
		return "", ErrNotARepository
	}
	return filepath.Clean(root), nil
}

// run executes git with args in dir. It returns the combined output and any
// error.
func (s *Service) run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	if err != nil {
		return buf.Bytes(), err
	}
	return buf.Bytes(), nil
}

// runArgsArg runs git with args plus a single path argument, ensuring the path
// is confined and prefixed with -- for safety against option-like filenames.
func (s *Service) runArg(ctx context.Context, dir string, args []string, arg string) ([]byte, error) {
	return s.run(ctx, dir, append(args, "--", arg)...)
}

// within reports whether child is the same as or inside parent.
func within(child, parent string) bool {
	if child == parent {
		return true
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// canonical resolves symlinks for path (which may not exist yet) by resolving
// the nearest existing ancestor and re-appending the remaining suffix.
func canonical(path string) (string, error) {
	current := path
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			rel, rerr := filepath.Rel(current, path)
			if rerr != nil {
				return "", rerr
			}
			return filepath.Join(resolved, rel), nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		current = parent
	}
}

// reposDir returns the path "<dataRoot>/.repos" reserved for cloned
// repositories (cloning writes to disk so the destination is confined).
func (s *Service) reposDir() string {
	return filepath.Join(s.dataRoot, ".repos")
}

// CloneOpts configures a clone.
type CloneOpts struct {
	URL string `json:"url"`
	Dir string `json:"dir"`
}

// Clone clones url into <dataRoot>/<dir>, validating both the URL scheme and
// the destination path so neither network nor filesystem can be abused.
func (s *Service) Clone(ctx context.Context, o CloneOpts) (string, error) {
	if !validCloneURL(o.URL) {
		return "", fmt.Errorf("unsupported clone URL: only http(s) and ssh URLs are allowed")
	}
	if err := os.MkdirAll(s.reposDir(), 0o755); err != nil {
		return "", fmt.Errorf("create repos dir: %w", err)
	}
	dest, err := s.resolve(filepath.Join(".repos", o.Dir))
	if err != nil {
		return "", err
	}
	if _, statErr := os.Stat(dest); statErr == nil {
		entries, _ := os.ReadDir(dest)
		if len(entries) > 0 {
			return "", fmt.Errorf("destination directory is not empty")
		}
	}
	args := []string{"clone", o.URL, dest}
	// The destination is appended as a plain arg, not a -- path, because git
	// clone takes positional arguments.
	out, err := s.run(ctx, s.reposDir(), args...)
	if err != nil {
		return "", fmt.Errorf("clone failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return dest, nil
}

// validCloneURL permits only https, ssh, and "user@host:path" (scp-like) URLs.
func validCloneURL(raw string) bool {
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
