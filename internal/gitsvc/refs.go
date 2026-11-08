package gitsvc

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Tag is one annotated/lightweight tag.
type Tag struct {
	Name string `json:"name"`
	Sha  string `json:"sha,omitempty"`
	Ref  string `json:"ref,omitempty"`
}

// Tags lists tags sorted newest first.
func (s *Service) Tags(ctx context.Context, path string) ([]Tag, error) {
	root, err := s.repoRoot(ctx, path)
	if err != nil {
		return nil, err
	}
	format := "%(refname:short)" + fldSep + "%(objectname:short)"
	out, err := s.run(ctx, root, "tag", "--sort=-creatordate", "--format="+format)
	if err != nil {
		return nil, fmt.Errorf("git tag failed: %w", err)
	}
	var tags []Tag
	for _, rec := range strings.Split(string(out), "\n") {
		rec = strings.TrimSuffix(rec, "\n")
		if rec == "" {
			continue
		}
		parts := strings.SplitN(rec, fldSep, 2)
		if len(parts) != 2 {
			continue
		}
		tags = append(tags, Tag{Name: parts[0], Sha: parts[1]})
	}
	return nonNil(tags), nil
}

// CreateTag creates a tag (annotated when message is provided).
func (s *Service) CreateTag(ctx context.Context, path, name, ref, message string) error {
	root, err := s.repoRoot(ctx, path)
	if err != nil {
		return err
	}
	if name == "" {
		return fmt.Errorf("tag name is required")
	}
	args := []string{"tag"}
	if message != "" {
		args = append(args, "-a", "-m", message)
	}
	args = append(args, name)
	if ref != "" {
		args = append(args, ref)
	}
	if _, err := s.run(ctx, root, args...); err != nil {
		return fmt.Errorf("git tag failed: %w", err)
	}
	return nil
}

// DeleteTag removes a tag.
func (s *Service) DeleteTag(ctx context.Context, path, name string) error {
	root, err := s.repoRoot(ctx, path)
	if err != nil {
		return err
	}
	if name == "" {
		return fmt.Errorf("tag name is required")
	}
	if _, err := s.run(ctx, root, "tag", "-d", name); err != nil {
		return fmt.Errorf("git tag -d failed: %w", err)
	}
	return nil
}

// Stash is one entry of `git stash list`.
type Stash struct {
	Ref     string `json:"ref"` // stash@{0}
	Subject string `json:"subject"`
	Sha     string `json:"sha,omitempty"`
}

// Stashes lists stash entries newest first.
func (s *Service) Stashes(ctx context.Context, path string) ([]Stash, error) {
	root, err := s.repoRoot(ctx, path)
	if err != nil {
		return nil, err
	}
	format := "%gd" + fldSep + "%s" + fldSep + "%H"
	out, err := s.run(ctx, root, "stash", "list", "--format="+format)
	if err != nil {
		return nil, fmt.Errorf("git stash list failed: %w", err)
	}
	var stashes []Stash
	for _, rec := range strings.Split(string(out), "\n") {
		rec = strings.TrimSuffix(rec, "\n")
		if rec == "" {
			continue
		}
		parts := strings.SplitN(rec, fldSep, 3)
		if len(parts) < 2 {
			continue
		}
		s := Stash{Ref: parts[0], Subject: parts[1]}
		if len(parts) >= 3 {
			s.Sha = parts[2]
		}
		stashes = append(stashes, s)
	}
	return nonNil(stashes), nil
}

// StashOpts configures a stash operation.
type StashOpts struct {
	Action  string `json:"action"` // push | apply | pop | drop
	Message string `json:"message,omitempty"`
	Index   int    `json:"index,omitempty"` // stash@<index>
	Keep    bool   `json:"keep"`
}

// Stash applies the requested stash action.
func (s *Service) Stash(ctx context.Context, path string, o StashOpts) error {
	root, err := s.repoRoot(ctx, path)
	if err != nil {
		return err
	}
	ref := "stash@{" + strconv.Itoa(o.Index) + "}"
	switch o.Action {
	case "push":
		args := []string{"stash", "push"}
		if o.Keep {
			args = append(args, "--keep-index")
		}
		if o.Message != "" {
			args = append(args, "-m", o.Message)
		}
		if _, err := s.run(ctx, root, args...); err != nil {
			return fmt.Errorf("git stash push failed: %w", err)
		}
	case "apply":
		if _, err := s.run(ctx, root, "stash", "apply", ref); err != nil {
			return fmt.Errorf("git stash apply failed: %w", err)
		}
	case "pop":
		if _, err := s.run(ctx, root, "stash", "pop", ref); err != nil {
			return fmt.Errorf("git stash pop failed: %w", err)
		}
	case "drop":
		if _, err := s.run(ctx, root, "stash", "drop", ref); err != nil {
			return fmt.Errorf("git stash drop failed: %w", err)
		}
	default:
		return fmt.Errorf("stash action must be push, apply, pop or drop")
	}
	return nil
}
