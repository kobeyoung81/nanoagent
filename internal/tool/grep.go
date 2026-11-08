package tool

import (
	"bufio"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gitee.com/kobeyoung81/nanosec/internal/provider"
)

// GrepTool searches file contents for a regular expression, returning matching
// lines as "path:line: content". Pure Go (no ripgrep dependency).
type GrepTool struct {
	Workspace       string
	SharedWorkspace string
}

// NewGrepTool creates a GrepTool scoped to the workspace(s).
func NewGrepTool(workspace, shared string) *GrepTool {
	return &GrepTool{Workspace: workspace, SharedWorkspace: shared}
}

func (t *GrepTool) Name() string { return "grep" }

func (t *GrepTool) Description() string {
	return "Search file contents using a regular expression. Returns matching lines as path:line: content. " +
		"Use the include parameter to filter by file pattern (e.g. **/*.go)."
}

func (t *GrepTool) Parameters() provider.ParameterSchema {
	return provider.ParameterSchema{
		Type: "object",
		Properties: map[string]provider.PropertySchema{
			"pattern":          {Type: "string", Description: "Regular expression to search for"},
			"path":             {Type: "string", Description: "File or directory to search (default: workspace root)"},
			"include":          {Type: "string", Description: "Only search files matching this glob (e.g. **/*.ts)"},
			"case_insensitive": {Type: "boolean", Description: "Case-insensitive matching"},
			"max_results":      {Type: "number", Description: "Maximum number of matching lines (default 200)"},
		},
		Required: []string{"pattern"},
	}
}

func (t *GrepTool) Execute(_ context.Context, params map[string]any) (string, error) {
	pattern, ok := params["pattern"].(string)
	if !ok || pattern == "" {
		return "", fmt.Errorf("missing pattern")
	}
	if ci, ok := params["case_insensitive"].(bool); ok && ci {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", fmt.Errorf("invalid pattern: %w", err)
	}

	root := t.Workspace
	if p, ok := params["path"].(string); ok && p != "" {
		root = p
	}
	if root == "" {
		root = "."
	}

	safe, err := safePathIn(t.Workspace, t.SharedWorkspace, root)
	if err != nil {
		return "", err
	}

	maxResults := 200
	if m, ok := params["max_results"].(float64); ok && m > 0 {
		maxResults = int(m)
	}
	include, _ := params["include"].(string)
	var includeRe *regexp.Regexp
	if include != "" {
		includeRe = compileGlob(include)
	}

	var out strings.Builder
	matches := 0
	truncated := false
	stats := &grepStats{}

	visit := func(path string, d fs.DirEntry) error {
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if includeRe != nil && !matchGlob(includeRe, include, path, safe) {
			return nil
		}
		lines, err := grepFile(path, re, &out, maxResults-matches, t.Workspace, stats)
		if err != nil {
			return nil
		}
		matches += lines
		if matches >= maxResults {
			truncated = true
			return fs.SkipAll
		}
		return nil
	}

	info, statErr := os.Stat(safe)
	if statErr == nil && !info.IsDir() {
		if n, err := grepFile(safe, re, &out, maxResults, t.Workspace, stats); err == nil {
			matches += n
		}
	} else {
		_ = filepath.WalkDir(safe, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			return visit(path, d)
		})
	}

	if matches == 0 {
		return "No matches found." + stats.suffix(), nil
	}
	result := out.String()
	if truncated {
		result += fmt.Sprintf("\n[truncated at %d matches]", maxResults)
	}
	return result + stats.suffix(), nil
}

// grepStats accumulates what the walk had to skip, so a search that silently
// ignored a binary file says so instead of looking like a complete answer.
type grepStats struct {
	skippedBinary int
}

func (s *grepStats) suffix() string {
	if s.skippedBinary == 0 {
		return ""
	}
	return fmt.Sprintf("\n[skipped %d binary file(s)]", s.skippedBinary)
}

// maxGrepLineChars caps one printed line: a minified bundle is a single line of
// megabytes, and a match inside it must not become the whole result.
const maxGrepLineChars = 500

func grepFile(path string, re *regexp.Regexp, out *strings.Builder, remaining int, workspace string, stats *grepStats) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	display := displayPath(path, workspace)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	count := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		if strings.IndexByte(line, 0) >= 0 {
			if stats != nil {
				stats.skippedBinary++
			}
			return count, nil // binary-ish
		}
		if re.MatchString(line) {
			fmt.Fprintf(out, "%s:%d: %s\n", display, lineNo, truncateGrepLine(line))
			count++
			if count >= remaining {
				return count, nil
			}
		}
	}
	return count, scanner.Err()
}

// truncateGrepLine shortens a printed line, marking the cut.
func truncateGrepLine(line string) string {
	if len(line) <= maxGrepLineChars {
		return line
	}
	return line[:maxGrepLineChars] + "…"
}

func displayPath(path, workspace string) string {
	if workspace == "" {
		return path
	}
	if abs, err := filepath.Abs(workspace); err == nil {
		if rel, err := filepath.Rel(abs, path); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel)
		}
	}
	return path
}
