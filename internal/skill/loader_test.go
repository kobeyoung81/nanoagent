package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSkill(t *testing.T, dir, name, content string) {
	t.Helper()
	d := filepath.Join(dir, name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoader_List_Workspace(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "skills")
	writeSkill(t, dir, "weather", "---\nname: weather\ndescription: Get the weather\n---\n\nUse wttr.in")
	writeSkill(t, dir, "other", "---\nname: other\ndescription: Other\n---\n\nbody")

	skills := NewLoader(dir).List()
	if len(skills) != 2 {
		t.Fatalf("expected 2 skills, got %d", len(skills))
	}
	names := map[string]bool{}
	for _, s := range skills {
		names[s.Name] = true
		if s.Source != "workspace" {
			t.Errorf("%s: source = %q, want workspace", s.Name, s.Source)
		}
	}
	if !names["weather"] || !names["other"] {
		t.Fatalf("unexpected names: %v", names)
	}
}

func TestLoader_List_MissingDirIsEmpty(t *testing.T) {
	if got := NewLoader(filepath.Join(t.TempDir(), "nope")).List(); len(got) != 0 {
		t.Fatalf("expected empty list for missing dir, got %d", len(got))
	}
}

func TestLoader_Load(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "skills")
	writeSkill(t, dir, "weather", "---\nname: weather\ndescription: Get the weather\n---\n\nUse wttr.in")

	body := NewLoader(dir).Load("weather")
	if body == "" {
		t.Fatal("expected non-empty body")
	}
	if strings.HasPrefix(body, "---") {
		t.Error("expected frontmatter to be stripped")
	}
	if !strings.Contains(body, "wttr.in") {
		t.Errorf("unexpected body: %q", body)
	}
}

func TestLoader_Load_NotFound(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "skills")
	writeSkill(t, dir, "weather", "---\nname: weather\n---\n\nbody")
	if body := NewLoader(dir).Load("nonexistent"); body != "" {
		t.Errorf("expected empty string for unknown skill, got %q", body)
	}
}

func TestLoader_Load_RejectsPathTraversal(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "skills")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if body := NewLoader(dir).Load("../secret"); body != "" {
		t.Error("path traversal must not load files outside the skills dir")
	}
}

func TestLoader_IgnoresSupportingFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "skills")
	writeSkill(t, dir, "officecli-docx", "---\nname: officecli-docx\ndescription: docx\n---\n\nUse docx.")
	if err := os.WriteFile(filepath.Join(dir, "officecli-docx", "creating.md"), []byte("create"), 0o644); err != nil {
		t.Fatal(err)
	}

	skills := NewLoader(dir).List()
	count := 0
	for _, s := range skills {
		if s.Name == "officecli-docx" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected 1 officecli-docx skill, got %d", count)
	}
}

func TestLoader_BuildSummary(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "skills")
	writeSkill(t, dir, "weather", "---\nname: weather\ndescription: Get the weather\n---\n\nbody")

	summary := NewLoader(dir).BuildSummary()
	if !strings.Contains(summary, "<skills>") {
		t.Error("expected XML <skills> tag")
	}
	if !strings.Contains(summary, "weather") {
		t.Error("expected weather to appear in summary")
	}
}

func TestLoader_GetAlwaysSkills(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "skills")
	writeSkill(t, dir, "pinned", "---\nname: pinned\ndescription: d\nmetadata: {\"nanobot\":{\"always\":true}}\n---\n\nbody")
	writeSkill(t, dir, "plain", "---\nname: plain\ndescription: d\n---\n\nbody")

	always := NewLoader(dir).GetAlwaysSkills()
	if len(always) != 1 || always[0] != "pinned" {
		t.Fatalf("expected [pinned], got %v", always)
	}
}

func TestParseFrontmatter_WithFrontmatter(t *testing.T) {
	content := "---\nname: test\ndescription: A test skill\n---\n\n# Content\n\nBody here."
	m, body := parseFrontmatter(content)

	if m.name != "test" {
		t.Errorf("expected name 'test', got %q", m.name)
	}
	if m.description != "A test skill" {
		t.Errorf("expected description 'A test skill', got %q", m.description)
	}
	if !strings.Contains(body, "Body here.") {
		t.Errorf("expected body to contain 'Body here.', got %q", body)
	}
	if strings.HasPrefix(body, "---") {
		t.Error("expected frontmatter to be stripped from body")
	}
}

func TestParseFrontmatter_NoFrontmatter(t *testing.T) {
	content := "# Just content\n\nNo frontmatter here."
	m, body := parseFrontmatter(content)

	if m.name != "" || m.description != "" {
		t.Error("expected empty meta for content with no frontmatter")
	}
	if body != content {
		t.Errorf("expected body to equal original content, got %q", body)
	}
}

func TestParseFrontmatter_WithMetadataJSON(t *testing.T) {
	content := "---\nname: gh\ndescription: GitHub\nmetadata: {\"nanobot\":{\"requires\":{\"bins\":[\"gh\"]}}}\n---\n\nBody"
	m, _ := parseFrontmatter(content)

	if len(m.requireBins) != 1 || m.requireBins[0] != "gh" {
		t.Errorf("expected requireBins=[gh], got %v", m.requireBins)
	}
}

func TestRegistry_Get_Cached(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "skills")
	writeSkill(t, dir, "weather", "---\nname: weather\n---\n\nbody")
	reg := NewRegistry(NewLoader(dir))

	body1 := reg.Get("weather")
	if body1 == "" {
		t.Fatal("expected non-empty body")
	}
	if body2 := reg.Get("weather"); body1 != body2 {
		t.Error("expected cached result to match first load")
	}
}

func TestRegistry_BuildSummary(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "skills")
	writeSkill(t, dir, "weather", "---\nname: weather\ndescription: d\n---\n\nbody")
	summary := NewRegistry(NewLoader(dir)).BuildSummary()
	if !strings.Contains(summary, "<skills>") {
		t.Error("expected XML summary")
	}
}

func TestXmlEscape(t *testing.T) {
	cases := []struct {
		in, out string
	}{
		{"hello", "hello"},
		{"a & b", "a &amp; b"},
		{"<tag>", "&lt;tag&gt;"},
	}
	for _, c := range cases {
		if got := xmlEscape(c.in); got != c.out {
			t.Errorf("xmlEscape(%q) = %q, want %q", c.in, got, c.out)
		}
	}
}
