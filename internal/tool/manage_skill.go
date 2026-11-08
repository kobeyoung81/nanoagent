package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanosec/internal/provider"
)

// SkillSaver persists user-scoped skills to the bus. Implemented by the
// nanoprotocol client.
type SkillSaver interface {
	SaveSkill(req nanoprotocol.SkillSaveRequest) (*nanoprotocol.SkillDef, error)
	DeleteSkill(id, name, userID string) error
}

const (
	manageSkillMaxFiles = 50
	manageSkillMaxBytes = 1 << 20 // 1 MiB total
)

// ManageSkillTool lets an agent author user-scoped skills with validation.
type ManageSkillTool struct {
	saver   SkillSaver
	userID  string
	agentID string
	// workspace and sharedWorkspace bound files_dir reads, exactly like the
	// file tools. Both empty means "not restricted" (the same agent setting
	// that lifts the restriction for read/write/list).
	workspace       string
	sharedWorkspace string
	// skillsDir is where the hypervisor materializes enabled skills. An update
	// reads it to merge and to notice files the caller left out; it is empty
	// when the agent has no skills directory configured.
	skillsDir string
}

// NewManageSkillTool creates a skill authoring tool for one agent.
func NewManageSkillTool(saver SkillSaver, userID, agentID, workspace, sharedWorkspace, skillsDir string) *ManageSkillTool {
	return &ManageSkillTool{
		saver:           saver,
		userID:          userID,
		agentID:         agentID,
		workspace:       workspace,
		sharedWorkspace: sharedWorkspace,
		skillsDir:       skillsDir,
	}
}

func (t *ManageSkillTool) Name() string { return "manage_skill" }

func (t *ManageSkillTool) Description() string {
	return "Create, update, or delete a user-scoped skill (a SKILL.md package). " +
		"Use it to save a reusable procedure you discovered. Files is a map of " +
		"relative path to content; it must include SKILL.md. Files_dir names a " +
		"directory to read the whole package from instead — prefer it unless you " +
		"are changing one line. Platform skills cannot be modified and names must " +
		"be unique. Updating replaces the whole package: files you leave out are " +
		"deleted unless you pass merge=true (keep the installed ones) or " +
		"allow_remove=true. Deletion only works for skills your user authored."
}

func (t *ManageSkillTool) Parameters() provider.ParameterSchema {
	return provider.ParameterSchema{
		Type: "object",
		Properties: map[string]provider.PropertySchema{
			"action": {Type: "string", Description: `"create" | "update" | "delete"`},
			"name":   {Type: "string", Description: "Skill name (filesystem-safe)"},
			"description": {
				Type:        "string",
				Description: "Short description of what the skill does",
			},
			"files": {
				Type: "object",
				Description: "Map of relative path -> content; must include SKILL.md. " +
					"Prefer files_dir for anything but a one-line edit.",
			},
			"files_dir": {
				Type: "string",
				Description: "Directory to read the package from, relative to the workspace; " +
					"its tree becomes the skill's files. Use this instead of inlining file contents.",
			},
			"merge": {
				Type: "boolean",
				Description: "Update only: keep files the installed skill has and this call does " +
					"not mention. Requires the skill to be installed in this agent; not usable " +
					"when renaming. Combining it with allow_remove behaves as a plain replace.",
			},
			"allow_remove": {
				Type: "boolean",
				Description: "Update only: allow dropping files the installed skill has. " +
					"Without it, an update that omits installed files is rejected.",
			},
			"id": {Type: "string", Description: "Skill id (optional for delete)"},
		},
		Required: []string{"action"},
	}
}

func (t *ManageSkillTool) Execute(_ context.Context, params map[string]any) (string, error) {
	if t.userID == "" {
		return "", fmt.Errorf("skill authoring is unavailable: no user context")
	}
	action, _ := params["action"].(string)
	name, _ := params["name"].(string)
	switch action {
	case "delete":
		id, _ := params["id"].(string)
		if id == "" && strings.TrimSpace(name) == "" {
			return "", fmt.Errorf("delete requires id or name")
		}
		if err := t.saver.DeleteSkill(id, name, t.userID); err != nil {
			return "", err
		}
		return fmt.Sprintf("Skill %q deleted.", name), nil
	case "create", "update":
		if !validSkillName(name) {
			return "", fmt.Errorf("invalid skill name %q: use letters, digits, '-' or '_'", name)
		}
		submitted, err := t.collectSubmitted(params)
		if err != nil {
			return "", err
		}
		if len(submitted) == 0 {
			return "", fmt.Errorf("files or files_dir is required")
		}
		merge := paramBool(params, "merge")
		allowRemove := paramBool(params, "allow_remove")

		// The final package is decided here, once, so the four combinations of
		// merge/allow_remove cannot each drift into their own rule:
		//   merge                     -> installed files + submitted (submitted wins)
		//   merge + allow_remove      -> submitted (a plain replace)
		//   plain + allow_remove      -> submitted
		//   plain                     -> submitted, unless that would drop files
		final := submitted
		if action == "update" && merge && !allowRemove {
			base, ok := t.installed(name)
			if !ok {
				return "", fmt.Errorf("merge needs %q installed in this agent, and it is not there (it may be disabled or renamed); submit the complete package instead", name)
			}
			final = make(map[string]string, len(base)+len(submitted))
			for path, content := range base {
				final[path] = content
			}
			for path, content := range submitted {
				final[path] = content
			}
		}
		// A merge may leave SKILL.md in the baseline, so the package is only
		// complete here — after the merge, never before it.
		if err := validateSkillPackage(final); err != nil {
			return "", err
		}
		if action == "update" && !merge && !allowRemove {
			if base, ok := t.installed(name); ok {
				if missing := missingPaths(base, final); len(missing) > 0 {
					return "", fmt.Errorf("update would remove %d file(s) the installed skill still has: %s; include them (or use files_dir), pass merge=true to keep them, or allow_remove=true to drop them",
						len(missing), strings.Join(missing, ", "))
				}
			}
		}

		id, _ := params["id"].(string)
		desc, _ := params["description"].(string)
		def, err := t.saver.SaveSkill(nanoprotocol.SkillSaveRequest{
			ID:          id,
			UserID:      t.userID,
			Name:        name,
			Description: desc,
			Files:       final,
			Origin:      "agent",
			CreatedBy:   t.agentID,
		})
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Skill %q saved (id=%s, files=%d). It is enabled for this agent.", name, def.ID, len(def.Files)), nil
	default:
		return "", fmt.Errorf("unknown action %q (use create, update, or delete)", action)
	}
}

// normalizeFiles accepts either a JSON object or a JSON string of an object.
func normalizeFiles(v any) (map[string]string, error) {
	switch raw := v.(type) {
	case nil:
		return nil, fmt.Errorf("files is required")
	case map[string]any:
		out := make(map[string]string, len(raw))
		for k, val := range raw {
			s, ok := val.(string)
			if !ok {
				return nil, fmt.Errorf("file %q content must be a string", k)
			}
			out[k] = s
		}
		return out, nil
	case string:
		var m map[string]string
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			return nil, fmt.Errorf("files must be an object of path -> content")
		}
		return m, nil
	default:
		return nil, fmt.Errorf("files must be an object of path -> content")
	}
}

// paramBool reads a boolean argument; a missing or mistyped value is false,
// which keeps the safe (rejecting) behaviour for merge=false/allow_remove=false.
func paramBool(params map[string]any, key string) bool {
	switch v := params[key].(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(strings.TrimSpace(v), "true")
	}
	return false
}

// collectSubmitted reads the package this call carries: files_dir first (its
// tree is the package), then files on top (a one-file edit wins over the copy
// in the directory).
func (t *ManageSkillTool) collectSubmitted(params map[string]any) (map[string]string, error) {
	out := map[string]string{}
	if dir, _ := params["files_dir"].(string); strings.TrimSpace(dir) != "" {
		fromDir, err := t.readPackageDir(strings.TrimSpace(dir))
		if err != nil {
			return nil, err
		}
		for path, content := range fromDir {
			out[path] = content
		}
	}
	if raw, ok := params["files"]; ok && raw != nil {
		inline, err := normalizeFiles(raw)
		if err != nil {
			return nil, err
		}
		for path, content := range inline {
			out[path] = content
		}
	}
	return out, nil
}

// readPackageDir turns a workspace directory into a package. The tree is the
// package: there is no include/exclude syntax, so the caller decides what goes
// in by deciding what to write. Dot entries (including .git) are skipped so an
// editor or repository file cannot end up in a skill, and symlinks are not
// followed — a link could point outside the workspace the guard just checked.
func (t *ManageSkillTool) readPackageDir(dir string) (map[string]string, error) {
	root, err := safePathIn(t.workspace, t.sharedWorkspace, dir)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("files_dir %q: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("files_dir %q is not a directory", dir)
	}

	out := map[string]string{}
	files, total := 0, 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path != root && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", filepath.ToSlash(rel), err)
		}
		files++
		total += len(content)
		if files > manageSkillMaxFiles {
			return fmt.Errorf("files_dir %q has more than %d files", dir, manageSkillMaxFiles)
		}
		if total > manageSkillMaxBytes {
			return fmt.Errorf("files_dir %q exceeds %d bytes", dir, manageSkillMaxBytes)
		}
		out[filepath.ToSlash(rel)] = string(content)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// installed reads the skill as this agent currently has it on disk. The
// hypervisor materializes every enabled skill into t.skillsDir, so an update
// can see what it would delete (and a merge has a baseline) without asking the
// bus. A missing directory means "not installed here": the caller decides
// whether that is a hard error (merge) or a reason to skip the check (update).
func (t *ManageSkillTool) installed(name string) (map[string]string, bool) {
	if t.skillsDir == "" {
		return nil, false
	}
	root := filepath.Join(t.skillsDir, name)
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return nil, false
	}
	out := map[string]string{}
	total := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.Type()&os.ModeSymlink != 0 || d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		total += len(content)
		if total > manageSkillMaxBytes {
			return fmt.Errorf("installed skill exceeds %d bytes", manageSkillMaxBytes)
		}
		out[filepath.ToSlash(rel)] = string(content)
		return nil
	})
	if err != nil {
		return nil, false
	}
	return out, true
}

// missingPaths lists the files base has that final would delete, sorted so the
// error message is stable between runs.
func missingPaths(base, final map[string]string) []string {
	var missing []string
	for path := range base {
		if _, ok := final[path]; !ok {
			missing = append(missing, path)
		}
	}
	sort.Strings(missing)
	return missing
}

// validateSkillPackage enforces path safety, the SKILL.md requirement, and
// size/count quotas.
func validateSkillPackage(files map[string]string) error {
	if len(files) == 0 {
		return fmt.Errorf("skill package is empty")
	}
	if len(files) > manageSkillMaxFiles {
		return fmt.Errorf("too many files (%d > %d)", len(files), manageSkillMaxFiles)
	}
	total := 0
	hasSkill := false
	paths := make([]string, 0, len(files))
	for p, content := range files {
		clean := strings.TrimSpace(p)
		if clean == "" || strings.HasPrefix(clean, "/") || strings.Contains(clean, "..") {
			return fmt.Errorf("unsafe file path %q", p)
		}
		if strings.HasPrefix(clean, ".git") || strings.Contains(clean, "\x00") {
			return fmt.Errorf("disallowed file path %q", p)
		}
		if clean == "SKILL.md" || strings.HasSuffix(clean, "/SKILL.md") {
			hasSkill = true
		}
		total += len(content)
		paths = append(paths, clean)
	}
	if !hasSkill {
		return fmt.Errorf("skill package must include SKILL.md")
	}
	if total > manageSkillMaxBytes {
		return fmt.Errorf("skill package too large (%d > %d bytes)", total, manageSkillMaxBytes)
	}
	sort.Strings(paths)
	return nil
}

// validSkillName mirrors the bus-side validation.
func validSkillName(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 96 {
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
