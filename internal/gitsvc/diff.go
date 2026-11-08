package gitsvc

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Record/field separator bytes used when talking to git: RS groups one log
// record, FS separates fields. Bodies may contain newlines, so we never split
// on \n.
const (
	recSep = "\x1e"
	fldSep = "\x1f"
)

// LogEntry is one commit returned by Log.
type LogEntry struct {
	Hash        string   `json:"hash"`
	AuthorName  string   `json:"author_name"`
	AuthorEmail string   `json:"author_email"`
	Timestamp   string   `json:"timestamp"` // ISO 8601 (author date)
	Subject     string   `json:"subject"`
	Body        string   `json:"body,omitempty"`
	Refs        []string `json:"refs,omitempty"` // decorations (branch/tag names)
	Parents     int      `json:"parents"`
}

// LogPage is a page of commit history.
type LogPage struct {
	Commits []LogEntry `json:"commits"`
	Path    string     `json:"path"`
	HasMore bool       `json:"has_more"`
}

// Log returns up to limit commits of the repository containing path, starting
// at skip. When file is non-empty the history is limited to that file.
func (s *Service) Log(ctx context.Context, path, file string, limit, skip int) (*LogPage, error) {
	root, err := s.repoRoot(ctx, path)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}

	format := recSep +
		"%H" + fldSep + "%an" + fldSep + "%ae" + fldSep + "%aI" + fldSep +
		"%s" + fldSep + "%b" + fldSep + "%d" + fldSep + "%p"

	// Ask for limit+1 so we can report has_more.
	args := []string{"log", "--format=" + format, "-n", strconv.Itoa(limit + 1)}
	if skip > 0 {
		args = append(args, "--skip", strconv.Itoa(skip))
	}
	if file != "" {
		resolved, rerr := s.resolveInRepo(root, file)
		if rerr != nil {
			return nil, rerr
		}
		args = append(args, "--", resolved)
	}

	out, err := s.run(ctx, root, args...)
	if err != nil {
		return nil, fmt.Errorf("git log failed: %w", err)
	}

	records := strings.Split(string(out), recSep)
	var commits []LogEntry
	for _, rec := range records {
		rec = strings.TrimSuffix(rec, "\n")
		if rec == "" {
			continue
		}
		if len(commits) >= limit {
			continue
		}
		fields := strings.Split(rec, fldSep)
		if len(fields) < 8 {
			continue
		}
		entry := LogEntry{
			Hash:        fields[0],
			AuthorName:  fields[1],
			AuthorEmail: fields[2],
			Timestamp:   fields[3],
			Subject:     fields[4],
			Body:        strings.TrimSuffix(fields[5], "\n"),
			Refs:        parseRefDecorations(fields[6]),
		}
		if n, err := strconv.Atoi(fields[7]); err == nil {
			entry.Parents = n
		}
		commits = append(commits, entry)
	}
	// hasMore is true when more than limit non-empty records were returned.
	nonEmpty := 0
	for _, rec := range records {
		if strings.TrimSuffix(rec, "\n") != "" {
			nonEmpty++
		}
	}
	return &LogPage{Commits: nonNil(commits), Path: path, HasMore: nonEmpty > limit}, nil
}

// parseRefDecorations converts git log %d output like " (HEAD -> main,
// origin/main, tag: v1)" into a list of ref names.
func parseRefDecorations(raw string) []string {
	raw = strings.TrimSpace(raw)
	raw = strings.Trim(raw, "()")
	if raw == "" {
		return nil
	}
	var refs []string
	for _, part := range strings.Split(raw, ", ") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if idx := strings.Index(part, " -> "); idx >= 0 {
			part = part[idx+4:]
		}
		part = strings.TrimPrefix(part, "tag: ")
		if part != "" {
			refs = append(refs, part)
		}
	}
	return refs
}

// resolveInRepo returns a path argument relative to the repo root (no
// traversal).
func (s *Service) resolveInRepo(root, rel string) (string, error) {
	if rel == "" {
		return "", nil
	}
	cleaned := strings.TrimPrefix(rel, "/")
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("invalid file path %q", rel)
	}
	return cleaned, nil
}

// CommitFile is a file changed in a commit (from --name-status).
type CommitFile struct {
	Status string `json:"status"`
	Path   string `json:"path"`
}

// CommitDetail describes a single commit and the files it changed.
type CommitDetail struct {
	LogEntry
	Message string       `json:"message"`
	Files   []CommitFile `json:"files"`
	Stage   string       `json:"stage,omitempty"`
}

// CommitDetail returns the metadata and changed files for a commit.
func (s *Service) CommitDetail(ctx context.Context, path, sha string) (*CommitDetail, error) {
	root, err := s.repoRoot(ctx, path)
	if err != nil {
		return nil, err
	}
	if sha == "" {
		sha = "HEAD"
	}

	format := recSep +
		"%H" + fldSep + "%an" + fldSep + "%ae" + fldSep + "%aI" + fldSep +
		"%s" + fldSep + "%b" + fldSep + "%d" + fldSep + "%p" + fldSep + "%P"
	out, err := s.run(ctx, root, "show", "--format="+format, "--name-status", sha)
	if err != nil {
		return nil, fmt.Errorf("git show failed: %w", err)
	}
	return parseCommitDetail(out, sha)
}

func parseCommitDetail(out []byte, sha string) (*CommitDetail, error) {
	text := string(out)
	// The first record is the commit header. Subsequent records (if any) are
	// from parent history; name-status lines come after a blank line.
	idx := strings.Index(text, recSep)
	if idx < 0 {
		return nil, fmt.Errorf("malformed commit output")
	}
	head := text[idx+len(recSep):]
	end := strings.Index(head, "\n")
	if end < 0 {
		end = len(head)
	}
	header := head[:end]
	rest := head[end:]

	d := &CommitDetail{}
	fields := strings.Split(header, fldSep)
	if len(fields) >= 8 {
		d.Hash = fields[0]
		d.AuthorName = fields[1]
		d.AuthorEmail = fields[2]
		d.Timestamp = fields[3]
		d.Subject = fields[4]
		d.Body = strings.TrimSuffix(fields[5], "\n")
		d.Refs = parseRefDecorations(fields[6])
		if n, err := strconv.Atoi(fields[7]); err == nil {
			d.Parents = n
		}
		d.Message = strings.TrimRight(d.Subject+"\n\n"+d.Body, "\n")
	}
	if len(fields) >= 9 {
		d.Stage = fields[8]
	}
	for _, line := range strings.Split(rest, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		d.Files = append(d.Files, parseNameStatusLine(line)...)
	}
	d.Files = nonNil(d.Files)
	return d, nil
}

// parseNameStatusLine parses one "--name-status" line into zero or more
// CommitFile entries.
func parseNameStatusLine(line string) []CommitFile {
	// "M\tpath", "A\tpath", "D\tpath", "R100\told\tnew", "C100\told\tnew".
	parts := strings.Split(line, "\t")
	if len(parts) < 2 {
		return nil
	}
	status := strings.ToUpper(parts[0][:1])
	switch status {
	case "M", "A", "D", "T", "U":
		return []CommitFile{{Status: status, Path: parts[1]}}
	case "R", "C":
		if len(parts) >= 3 {
			return []CommitFile{{Status: status, Path: parts[2]}}
		}
		return []CommitFile{{Status: status, Path: parts[1]}}
	default:
		return nil
	}
}

// Diff returns the unified diff of file (or the entire repo when file is
// empty). staged selects index vs HEAD instead of worktree vs index; ref
// compares against a specific commit/branch.
func (s *Service) Diff(ctx context.Context, path, file string, staged bool, ref string) (string, error) {
	root, err := s.repoRoot(ctx, path)
	if err != nil {
		return "", err
	}
	args := []string{"diff", "--no-color"}
	if staged {
		args = append(args, "--cached")
	}
	if ref != "" {
		if staged {
			args = append(args, ref)
		} else {
			args = append(args, "HEAD", ref)
		}
	}
	args = append(args, "--")
	if file != "" {
		resolved, rerr := s.resolveInRepo(root, file)
		if rerr != nil {
			return "", rerr
		}
		args = append(args, resolved)
	}
	out, err := s.run(ctx, root, args...)
	if err != nil {
		return "", fmt.Errorf("git diff failed: %w", err)
	}
	return string(out), nil
}

// DiffStat returns a compact numstat of changes with the same ref semantics
// as Diff.
func (s *Service) DiffStat(ctx context.Context, path string, staged bool, ref string) ([]CommitStat, error) {
	root, err := s.repoRoot(ctx, path)
	if err != nil {
		return nil, err
	}
	args := []string{"diff", "--no-color", "--numstat"}
	if staged {
		args = append(args, "--cached")
	}
	if ref != "" {
		if staged {
			args = append(args, ref)
		} else {
			args = append(args, "HEAD", ref)
		}
	}
	out, err := s.run(ctx, root, args...)
	if err != nil {
		return nil, fmt.Errorf("git diff --numstat failed: %w", err)
	}
	var stats []CommitStat
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 3 {
			continue
		}
		a, _ := strconv.Atoi(parts[0])
		d, _ := strconv.Atoi(parts[1])
		stats = append(stats, CommitStat{Path: parts[2], Added: a, Deleted: d})
	}
	return nonNil(stats), nil
}

// CommitStat is one entry of `git diff --numstat`.
type CommitStat struct {
	Path    string `json:"path"`
	Added   int    `json:"added"`
	Deleted int    `json:"deleted"`
}

// ShowFile returns the content of a file at a ref (":<path>" selects the
// index, "HEAD:<path>", a sha, or a branch). Used by conflict views.
func (s *Service) ShowFile(ctx context.Context, path, ref, file string) ([]byte, error) {
	root, err := s.repoRoot(ctx, path)
	if err != nil {
		return nil, err
	}
	if ref == "" {
		ref = "HEAD"
	}
	resolved, rerr := s.resolveInRepo(root, file)
	if rerr != nil {
		return nil, rerr
	}
	return s.run(ctx, root, "show", ref+":"+resolved)
}
