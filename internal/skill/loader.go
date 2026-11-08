// Package skill provides loading and management of agent skill definitions.
// Skills are markdown files (SKILL.md) that teach the agent how to use
// specific tools or perform certain tasks. The loader reads the per-agent
// skills directory, which the hypervisor populates from the platform catalog;
// there is no bundled or global fallback.
package skill

import (
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Skill holds metadata and availability info for a single skill.
type Skill struct {
	Name        string
	Description string
	Path        string // file path under the per-agent skills directory
	Source      string // always "workspace"
	Available   bool   // true if all requirements are met
}

// meta holds parsed frontmatter fields.
type meta struct {
	name        string
	description string
	always      bool
	requireBins []string
	requireEnv  []string
}

var fmRE = regexp.MustCompile(`(?s)^---\n(.*?)\n---\n?`)

// Loader loads skill definitions from the per-agent skills directory.
type Loader struct {
	skillsDir string
}

// NewLoader creates a Loader for the configured per-agent skills directory.
func NewLoader(skillsDir string) *Loader {
	return &Loader{skillsDir: skillsDir}
}

// List returns all skills found in the skills directory.
func (l *Loader) List() []Skill {
	seen := map[string]bool{}
	var skills []Skill
	if l.skillsDir == "" {
		return skills
	}

	entries, err := os.ReadDir(l.skillsDir)
	if err != nil {
		return skills
	}
	for _, e := range entries {
		if !e.IsDir() || seen[e.Name()] {
			continue
		}
		name := e.Name()
		path := filepath.Join(l.skillsDir, name, "SKILL.md")
		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		m, _ := parseFrontmatter(string(content))
		if m.name == "" {
			m.name = name
		}
		skills = append(skills, Skill{
			Name:        name,
			Description: m.description,
			Path:        path,
			Source:      "workspace",
			Available:   checkReqs(m),
		})
		seen[name] = true
	}
	return skills
}

// Load returns the body (frontmatter stripped) of a skill by name.
// Returns empty string if the skill is not found.
func (l *Loader) Load(name string) string {
	if !validSkillName(name) {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(l.skillsDir, name, "SKILL.md"))
	if err != nil {
		return ""
	}
	_, body := parseFrontmatter(string(data))
	return body
}

// LoadForContext returns the combined content of the named skills, formatted
// for injection into the system prompt.
func (l *Loader) LoadForContext(names []string) string {
	var parts []string
	for _, name := range names {
		body := l.Load(name)
		if body != "" {
			parts = append(parts, "### Skill: "+name+"\n\n"+body)
		}
	}
	return strings.Join(parts, "\n\n---\n\n")
}

// BuildSummary returns an XML-formatted summary of all available skills.
func (l *Loader) BuildSummary() string {
	skills := l.List()
	if len(skills) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("<skills>\n")
	for _, s := range skills {
		avail := "false"
		if s.Available {
			avail = "true"
		}
		sb.WriteString("  <skill available=\"" + avail + "\">\n")
		sb.WriteString("    <name>" + xmlEscape(s.Name) + "</name>\n")
		sb.WriteString("    <description>" + xmlEscape(s.Description) + "</description>\n")
		sb.WriteString("    <location>" + xmlEscape(s.Path) + "</location>\n")
		sb.WriteString("  </skill>\n")
	}
	sb.WriteString("</skills>")
	return sb.String()
}

// Detail is the fully resolved content of a single skill, used by the `skill`
// tool to load a skill on demand (progressive disclosure).
type Detail struct {
	Name        string
	Description string
	Body        string   // SKILL.md body with frontmatter stripped
	BaseDir     string   // directory containing SKILL.md
	Files       []string // sibling files relative to BaseDir (excluding SKILL.md), capped
}

// LoadDetail resolves a skill by name, returning its body, base directory and a
// sampled list of sibling files so relative references (scripts/, references/)
// can be resolved by the model.
func (l *Loader) LoadDetail(name string) (Detail, bool) {
	if !validSkillName(name) {
		return Detail{}, false
	}
	skillDir := filepath.Join(l.skillsDir, name)
	data, err := os.ReadFile(filepath.Join(skillDir, "SKILL.md"))
	if err != nil {
		return Detail{}, false
	}
	m, body := parseFrontmatter(string(data))
	if m.name == "" {
		m.name = name
	}
	return Detail{
		Name:        name,
		Description: m.description,
		Body:        body,
		BaseDir:     skillDir,
		Files:       walkFiles(os.DirFS(skillDir), detailFileLimit),
	}, true
}

// detailFileLimit caps how many sibling files are surfaced with a loaded skill.
const detailFileLimit = 10

// walkFiles collects up to limit regular-file paths (relative, slash-separated)
// under fsys, excluding the root SKILL.md, sorted for stable output.
func walkFiles(fsys fs.FS, limit int) []string {
	var files []string
	_ = fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if path == "SKILL.md" {
			return nil
		}
		if len(files) >= limit {
			return fs.SkipAll
		}
		files = append(files, filepath.ToSlash(path))
		return nil
	})
	sort.Strings(files)
	return files
}

// GetAlwaysSkills returns names of "always=true" available skills.
func (l *Loader) GetAlwaysSkills() []string {
	var result []string
	for _, s := range l.List() {
		if !s.Available {
			continue
		}
		if l.loadMeta(s.Name).always {
			result = append(result, s.Name)
		}
	}
	return result
}

// loadMeta reads and parses the frontmatter meta for a skill by name.
func (l *Loader) loadMeta(name string) meta {
	if !validSkillName(name) {
		return meta{}
	}
	data, err := os.ReadFile(filepath.Join(l.skillsDir, name, "SKILL.md"))
	if err != nil {
		return meta{}
	}
	m, _ := parseFrontmatter(string(data))
	return m
}

// validSkillName rejects names that could escape the skills directory.
func validSkillName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

// parseFrontmatter splits content into meta and body.
func parseFrontmatter(content string) (meta, string) {
	var m meta
	match := fmRE.FindStringSubmatch(content)
	if match == nil {
		return m, content
	}
	body := strings.TrimSpace(content[len(match[0]):])

	// Simple line-by-line YAML parsing
	raw := map[string]string{}
	for _, line := range strings.Split(match[1], "\n") {
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.Trim(strings.TrimSpace(line[idx+1:]), `"'`)
		raw[key] = val
	}

	m.name = raw["name"]
	m.description = raw["description"]

	if raw["metadata"] != "" {
		var data struct {
			Nanobot struct {
				Always   bool `json:"always"`
				Requires struct {
					Bins []string `json:"bins"`
					Env  []string `json:"env"`
				} `json:"requires"`
			} `json:"nanobot"`
		}
		if err := json.Unmarshal([]byte(raw["metadata"]), &data); err == nil {
			m.always = data.Nanobot.Always
			m.requireBins = data.Nanobot.Requires.Bins
			m.requireEnv = data.Nanobot.Requires.Env
		}
	}

	return m, body
}

// checkReqs returns true if all required binaries and env vars are present.
func checkReqs(m meta) bool {
	for _, bin := range m.requireBins {
		if _, err := exec.LookPath(bin); err != nil {
			return false
		}
	}
	for _, env := range m.requireEnv {
		if os.Getenv(env) == "" {
			return false
		}
	}
	return true
}

func xmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}
