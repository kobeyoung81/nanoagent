package tool

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gitee.com/kobeyoung81/nanosec/internal/provider"
)

// GlobTool finds files by glob pattern (supporting **), returning paths sorted
// by modification time (newest first).
type GlobTool struct {
	Workspace       string
	SharedWorkspace string
}

// NewGlobTool creates a GlobTool scoped to the workspace(s).
func NewGlobTool(workspace, shared string) *GlobTool {
	return &GlobTool{Workspace: workspace, SharedWorkspace: shared}
}

func (t *GlobTool) Name() string { return "glob" }

func (t *GlobTool) Description() string {
	return "Find files by glob pattern (e.g. **/*.go). Returns matching paths sorted by modification time, newest first."
}

func (t *GlobTool) Parameters() provider.ParameterSchema {
	return provider.ParameterSchema{
		Type: "object",
		Properties: map[string]provider.PropertySchema{
			"pattern": {Type: "string", Description: "Glob pattern (supports ** for recursive matches)"},
			"path":    {Type: "string", Description: "Directory to search (default: workspace root)"},
		},
		Required: []string{"pattern"},
	}
}

func (t *GlobTool) Execute(_ context.Context, params map[string]any) (string, error) {
	pattern, ok := params["pattern"].(string)
	if !ok || pattern == "" {
		return "", fmt.Errorf("missing pattern")
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

	re := compileGlob(pattern)

	type hit struct {
		path    string
		modTime int64
	}
	const maxResults = 500
	matches := make([]hit, 0, 32)

	_ = filepath.WalkDir(safe, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !matchGlob(re, pattern, path, safe) {
			return nil
		}
		var mt int64
		if info, ierr := d.Info(); ierr == nil {
			mt = info.ModTime().UnixNano()
		}
		matches = append(matches, hit{path: displayPath(path, t.Workspace), modTime: mt})
		if len(matches) >= maxResults {
			return fs.SkipAll
		}
		return nil
	})

	if len(matches) == 0 {
		return "No files matched.", nil
	}
	sort.SliceStable(matches, func(i, j int) bool { return matches[i].modTime > matches[j].modTime })

	var sb strings.Builder
	for _, m := range matches {
		sb.WriteString(m.path + "\n")
	}
	return sb.String(), nil
}

// compileGlob converts a glob pattern into a regular expression supporting
// `**` (any depth, including separators), `*` (within a path segment), `?`
// (single char) and `[...]` character classes.
func compileGlob(pattern string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				i++
				if i+1 < len(pattern) && pattern[i+1] == '/' {
					i++
					b.WriteString("(.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '[':
			j := i + 1
			for j < len(pattern) && pattern[j] != ']' {
				j++
			}
			if j < len(pattern) {
				b.WriteString(pattern[i : j+1])
				i = j
			} else {
				b.WriteString("\\[")
			}
		default:
			if strings.ContainsRune(`.+()|^$\\{}`, rune(c)) {
				b.WriteByte('\\')
			}
			b.WriteByte(c)
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return regexp.MustCompile(regexp.QuoteMeta(pattern))
	}
	return re
}

// matchGlob matches path against pattern/re. Patterns without a separator match
// against the basename; otherwise against the path relative to root.
func matchGlob(re *regexp.Regexp, pattern, path, root string) bool {
	target := path
	if !strings.Contains(pattern, "/") {
		target = filepath.Base(path)
	} else if root != "" {
		if absRoot, err := filepath.Abs(root); err == nil {
			if rel, err := filepath.Rel(absRoot, path); err == nil {
				target = filepath.ToSlash(rel)
			}
		}
	}
	return re.MatchString(target)
}
