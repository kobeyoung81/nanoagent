package tool

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"gitee.com/kobeyoung81/nanosec/internal/provider"
)

// readOnlyCommand is one whitelist entry: the leading words a shell segment
// must start with — when Flags is non-nil, the only "-" flags that entry
// tolerates, and when BannedFlags is non-nil the flags it must never carry (a
// filter that can write to a file, e.g. `sort -o` or `sed -i`). Group is the
// category quoted in refusal messages, so the model can correct itself in one
// step instead of trying variations.
type readOnlyCommand struct {
	Words       []string
	Flags       []string
	BannedFlags []string
	Group       string
}

// Refusal-message categories.
const (
	groupInspect = "inspect files"
	groupSearch  = "search"
	groupFilter  = "text filters"
	groupGit     = "git read-only subcommands"
	groupVerify  = "verify/build checks"
	groupChange  = "change directory inside the workspace"
)

// readOnlyWhitelist is the whole surface of ReadOnlyShellTool. Anything not
// listed here is refused before a process starts, which is what makes this tool
// a whitelist: the denylist in ShellTool stays where it is and is not consulted.
//
// Only side-effect-free commands belong here. Interpreters (python3, node, sh),
// network clients (curl, wget, ssh, scp), database clients (mysql, psql,
// redis-cli), container/control-plane clients (docker, kubectl), and anything
// that can execute or write on its own (xargs, awk, env VAR=x cmd, tee, patch,
// install) are deliberately absent: the tool's contract is that it cannot
// modify files and cannot send anything out.
var readOnlyWhitelist = []readOnlyCommand{
	// Inspect files.
	{Words: []string{"ls"}, Group: groupInspect},
	{Words: []string{"cat"}, Group: groupInspect},
	{Words: []string{"head"}, Group: groupInspect},
	{Words: []string{"tail"}, Group: groupInspect},
	{Words: []string{"wc"}, Group: groupInspect},
	{Words: []string{"stat"}, Group: groupInspect},
	{Words: []string{"du"}, Group: groupInspect},
	{Words: []string{"df"}, Group: groupInspect},
	{Words: []string{"file"}, Group: groupInspect},
	{Words: []string{"which"}, Group: groupInspect},
	{Words: []string{"pwd"}, Group: groupInspect},
	{Words: []string{"tree"}, Group: groupInspect},
	{Words: []string{"basename"}, Group: groupInspect},
	{Words: []string{"dirname"}, Group: groupInspect},
	{Words: []string{"realpath"}, Group: groupInspect},
	{Words: []string{"md5sum"}, Group: groupInspect},
	{Words: []string{"sha1sum"}, Group: groupInspect},
	{Words: []string{"sha256sum"}, Group: groupInspect},
	{Words: []string{"cmp"}, Group: groupInspect},
	{Words: []string{"diff"}, Group: groupInspect},
	{Words: []string{"nl"}, Group: groupInspect},
	{Words: []string{"column"}, Group: groupInspect},
	{Words: []string{"seq"}, Group: groupInspect},
	{Words: []string{"printf"}, Group: groupInspect},
	{Words: []string{"echo"}, Group: groupInspect},
	{Words: []string{"id"}, Group: groupInspect},
	{Words: []string{"uname"}, Group: groupInspect},
	{Words: []string{"nproc"}, Group: groupInspect},
	// `date -s` sets the clock, so the flags are restricted rather than the
	// command.
	{Words: []string{"date"}, BannedFlags: []string{"-s", "--set"}, Group: groupInspect},

	// Search.
	{Words: []string{"grep"}, Group: groupSearch},
	{Words: []string{"rg"}, Group: groupSearch},
	{Words: []string{"find"}, Group: groupSearch},

	// Text filters. They consume stdin or files and write to stdout: the only
	// write channel a filter has is its own output flag, which BannedFlags
	// blocks.
	{Words: []string{"sort"}, BannedFlags: []string{"-o", "--output"}, Group: groupFilter},
	{Words: []string{"uniq"}, Group: groupFilter},
	{Words: []string{"cut"}, Group: groupFilter},
	{Words: []string{"tr"}, Group: groupFilter},
	{Words: []string{"paste"}, Group: groupFilter},
	{Words: []string{"join"}, Group: groupFilter},
	{Words: []string{"comm"}, Group: groupFilter},
	{Words: []string{"sed"}, Flags: []string{"-n", "-E", "-r", "-e"}, BannedFlags: []string{"-i", "--in-place"}, Group: groupFilter},
	{Words: []string{"jq"}, BannedFlags: []string{"-i", "--in-place"}, Group: groupFilter},

	// Git, read-only subcommands only. `branch` and `remote` get a flag
	// allowlist because the same subcommand also deletes/renames/mutates;
	// `worktree` and `submodule` are listed per subcommand for the same reason.
	{Words: []string{"git", "status"}, Group: groupGit},
	{Words: []string{"git", "diff"}, Group: groupGit},
	{Words: []string{"git", "log"}, Group: groupGit},
	{Words: []string{"git", "show"}, Group: groupGit},
	{Words: []string{"git", "ls-files"}, Group: groupGit},
	{Words: []string{"git", "ls-tree"}, Group: groupGit},
	{Words: []string{"git", "describe"}, Group: groupGit},
	{Words: []string{"git", "rev-parse"}, Group: groupGit},
	{Words: []string{"git", "rev-list"}, Group: groupGit},
	{Words: []string{"git", "blame"}, Group: groupGit},
	{Words: []string{"git", "shortlog"}, Group: groupGit},
	{Words: []string{"git", "for-each-ref"}, Group: groupGit},
	{Words: []string{"git", "cat-file"}, Group: groupGit},
	{Words: []string{"git", "diff-tree"}, Group: groupGit},
	{Words: []string{"git", "merge-base"}, Group: groupGit},
	{Words: []string{"git", "name-rev"}, Group: groupGit},
	{Words: []string{"git", "count-objects"}, Group: groupGit},
	{Words: []string{"git", "grep"}, Group: groupGit},
	{Words: []string{"git", "worktree", "list"}, Group: groupGit},
	{Words: []string{"git", "submodule", "status"}, Group: groupGit},
	{Words: []string{"git", "config", "--get"}, Group: groupGit},
	{Words: []string{"git", "branch"}, Flags: []string{
		"-a", "-r", "-v", "-vv", "--all", "--remotes", "--verbose", "--list",
		"--show-current", "--contains", "--merged", "--no-merged", "--sort",
		"--format", "--points-at",
	}, Group: groupGit},
	{Words: []string{"git", "remote", "-v"}, Group: groupGit},
	{Words: []string{"git", "remote", "show"}, Group: groupGit},

	// Verify. These may write build caches or execute test code (see the tool
	// description); they never edit tracked files through this tool because
	// output flags are refused below.
	{Words: []string{"go", "build"}, Group: groupVerify},
	{Words: []string{"go", "vet"}, Group: groupVerify},
	{Words: []string{"go", "test"}, Group: groupVerify},
	{Words: []string{"go", "list"}, Group: groupVerify},
	{Words: []string{"go", "env"}, Group: groupVerify},
	{Words: []string{"go", "version"}, Group: groupVerify},
	{Words: []string{"make", "-n"}, Group: groupVerify},
}

// readOnlyBannedTokens are arguments that turn an otherwise read-only command
// into a writer: output files, and the destructive find actions. Matched by
// whole token after quotes are stripped, so `find . '-delete'` cannot smuggle a
// delete past the check, while find's harmless "-executable" test stays a
// different token and is not a false positive.
var readOnlyBannedTokens = []string{
	"--output", "-delete", "-exec", "-execdir", "-ok", "-okdir",
	"-fprint", "-fprint0", "-fls",
}

// readOnlySubstitutions are the shell constructs that would let a whitelisted
// command smuggle an arbitrary second command (the class of bypass that broke
// Claude Code's read-only allowlist, CVE-2025-66032).
var readOnlySubstitutions = []struct{ token, reason string }{
	{"$(", "command substitution"},
	{"`", "command substitution"},
	{"<(", "process substitution"},
	{">", "output redirection"},
	{"<", "input redirection"},
}

// sedWriteScript matches the two sed commands that write to a file — the `w`
// script command (`sed -n 'w out'`, `/re/w out`) and its s///w flag form
// (`sed 's/a/b/w out'`) — plus `e`, which executes a command. sed is whitelisted
// for reading and its `-i` is refused, so this closes the remaining write
// channel. The leading class covers a script command that follows an address
// (`1w out`, `$e ls`) or a separator, and the match requires a delimiter after
// the letter so a pattern such as /error/p is not a false positive.
var sedWriteScript = regexp.MustCompile(`(^|[;[:space:]/0-9$])(w|e)([[:space:]]|$)`)

// ReadOnlyShellTool executes whitelisted read-only shell commands. It is the
// read-only counterpart of ShellTool: ShellTool subtracts a denylist from
// everything bash can do, this tool admits only what readOnlyWhitelist names.
type ReadOnlyShellTool struct {
	Timeout   time.Duration
	Workspace string
}

// NewReadOnlyShellTool creates a read-only shell tool rooted at workspace.
func NewReadOnlyShellTool(timeout time.Duration, workspace string) *ReadOnlyShellTool {
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	return &ReadOnlyShellTool{Timeout: timeout, Workspace: workspace}
}

func (t *ReadOnlyShellTool) Name() string {
	return "readonly_bash"
}

func (t *ReadOnlyShellTool) Description() string {
	return `Execute a read-only shell command for inspection and verification.

Whitelisted commands (anything else is refused before it runs):
- inspect files: ls, cat, head, tail, wc, stat, du, df, file, which, pwd, tree,
  basename, dirname, realpath, md5sum, sha1sum, sha256sum, cmp, diff, nl, column,
  seq, printf, echo, id, uname, nproc, date
- search: grep, rg, find (destructive actions such as -delete/-exec are refused)
- text filters, usable in pipelines: sort, uniq, cut, tr, paste, join, comm,
  sed (no -i; no w/e script commands), jq (no -i)
- git, read-only subcommands: status, diff, log, show, ls-files, ls-tree,
  rev-parse, rev-list, describe, blame, shortlog, for-each-ref, cat-file,
  diff-tree, merge-base, name-rev, count-objects, grep, worktree list,
  submodule status, branch (listing flags only), remote (-v or show),
  config --get. The form "git -C <dir> <subcommand>" is accepted when <dir> is
  inside the workspace.
- verify/build checks: go build, go vet, go test, go list, go env, go version,
  make -n
- "cd <dir>" is accepted only for a directory inside the workspace, so a
  compound command can start with cd.

Refused: redirection (> >> <), command substitution ($(...), backticks),
process substitution, interpreters and network/database clients (python3, node,
curl, ssh, mysql, docker, ...), output flags (--output, go build/test -o,
sort -o) and the sed/jq in-place flags. The verify commands may write build
caches or run test code; they do not edit tracked files. This tool cannot modify
files — use bash when a change is actually needed.`
}

func (t *ReadOnlyShellTool) Parameters() provider.ParameterSchema {
	return provider.ParameterSchema{
		Type: "object",
		Properties: map[string]provider.PropertySchema{
			"command": {
				Type:        "string",
				Description: "The read-only bash command (e.g. 'git status', 'grep -rn foo . | sort | uniq -c')",
			},
		},
		Required: []string{"command"},
	}
}

func (t *ReadOnlyShellTool) Execute(ctx context.Context, params map[string]any) (string, error) {
	cmdStr, ok := params["command"].(string)
	if !ok {
		return "", fmt.Errorf("missing or invalid 'command' parameter")
	}

	if err := CheckReadOnlyCommand(cmdStr, t.Workspace); err != nil {
		return "", err
	}

	return execShell(ctx, cmdStr, t.Workspace, t.Timeout)
}

// CheckReadOnlyCommand reports whether cmd is composed solely of whitelisted,
// side-effect-free invocations. It is deliberately conservative: whatever it
// cannot prove safe is refused, and the error it returns is written to be
// relayed to the model as-is. workspace bounds `cd` and `git -C` (empty = no
// bound).
func CheckReadOnlyCommand(cmd, workspace string) error {
	if strings.TrimSpace(cmd) == "" {
		return fmt.Errorf("command blocked: read-only mode: empty command")
	}
	// Substitutions and redirections only count outside quotes: `grep -e 'a>b'`
	// searches for a literal rather than redirecting.
	bare := stripQuoted(cmd)
	for _, sub := range readOnlySubstitutions {
		if strings.Contains(bare, sub.token) {
			return fmt.Errorf("command blocked: read-only mode: %s (%q) is not allowed", sub.reason, sub.token)
		}
	}
	for _, seg := range splitShellSegments(cmd) {
		if err := checkReadOnlySegment(seg, workspace); err != nil {
			return err
		}
	}
	return nil
}

// stripQuoted returns cmd with the contents of quoted spans removed, so token
// checks see only what the shell would treat as syntax.
func stripQuoted(cmd string) string {
	var b strings.Builder
	b.Grow(len(cmd))
	quote := byte(0)
	escaped := false
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch {
		case escaped:
			escaped = false
		case quote == '\'':
			if c == '\'' {
				quote = 0
			}
		case quote == '"':
			if c == '\\' {
				escaped = true
			} else if c == '"' {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '\\':
			escaped = true
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// splitShellSegments breaks a command line on the shell's control operators, so
// each pipeline/sequence stage is checked on its own: "git status; rm -rf ."
// yields two segments and the second one fails. Operators inside quotes are
// literal text and split nothing (so `grep -e 'a;b' f` stays one segment).
func splitShellSegments(cmd string) []string {
	var segs []string
	var cur strings.Builder
	flush := func() {
		segs = append(segs, cur.String())
		cur.Reset()
	}
	quote := byte(0)
	escaped := false
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		if escaped {
			cur.WriteByte(c)
			escaped = false
			continue
		}
		if quote == '\'' {
			cur.WriteByte(c)
			if c == '\'' {
				quote = 0
			}
			continue
		}
		if quote == '"' {
			cur.WriteByte(c)
			if c == '\\' {
				escaped = true
			} else if c == '"' {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
			cur.WriteByte(c)
		case '\\':
			escaped = true
			cur.WriteByte(c)
		case '\n', ';':
			flush()
		case '&':
			flush()
			if i+1 < len(cmd) && cmd[i+1] == '&' {
				i++
			}
		case '|':
			flush()
			if i+1 < len(cmd) && (cmd[i+1] == '|' || cmd[i+1] == '&') {
				i++
			}
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return segs
}

// splitSegmentFields splits one segment into words the way the shell would
// present them: quotes group and are removed, so `grep -e 'a b' f` is three
// words and `find . '-delete'` still exposes -delete as its own token.
func splitSegmentFields(seg string) []string {
	var (
		fields  []string
		cur     strings.Builder
		quote   byte
		escaped bool
	)
	flush := func() {
		if cur.Len() > 0 {
			fields = append(fields, cur.String())
			cur.Reset()
		}
	}
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		switch {
		case escaped:
			cur.WriteByte(c)
			escaped = false
		case quote == '\'':
			if c == '\'' {
				quote = 0
			} else {
				cur.WriteByte(c)
			}
		case quote == '"':
			if c == '\\' {
				escaped = true
			} else if c == '"' {
				quote = 0
			} else {
				cur.WriteByte(c)
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '\\':
			escaped = true
		case c == ' ' || c == '\t' || c == '\r':
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return fields
}

func checkReadOnlySegment(seg, workspace string) error {
	words := splitSegmentFields(seg)
	if len(words) == 0 {
		return nil
	}

	// `cd` is special-cased: it is a shell builtin that only moves where the
	// rest of the line runs, so it is allowed as long as the target stays inside
	// the workspace.
	if words[0] == "cd" {
		return checkReadOnlyCd(words, workspace)
	}

	// `git -C <dir> <subcommand>` is accepted when <dir> is inside the
	// workspace; the -C pairs are removed before the subcommand is matched.
	words, err := stripGitDirectory(words, workspace)
	if err != nil {
		return err
	}

	entry, ok := matchReadOnlyPrefix(words)
	if !ok {
		return fmt.Errorf("command blocked: read-only mode: %q is not a whitelisted read-only command (allowed: %s; see the tool description)",
			words[0], allowedGroups())
	}

	for _, w := range words {
		base := w
		if i := strings.IndexByte(base, '='); i > 0 {
			base = base[:i]
		}
		for _, banned := range readOnlyBannedTokens {
			if base == banned {
				return fmt.Errorf("command blocked: read-only mode: %s is not allowed", banned)
			}
		}
		for _, banned := range entry.BannedFlags {
			if base == banned || strings.HasPrefix(base, banned+"=") {
				return fmt.Errorf("command blocked: read-only mode: %s writes a file and is not allowed for %q",
					base, strings.Join(entry.Words, " "))
			}
		}
		if entry.Flags != nil && strings.HasPrefix(w, "-") && !containsString(entry.Flags, w) {
			return fmt.Errorf("command blocked: read-only mode: flag %q is not allowed for %q",
				w, strings.Join(entry.Words, " "))
		}
	}

	if entry.Words[0] == "sed" && sedScriptWrites(words) {
		return fmt.Errorf("command blocked: read-only mode: a sed w/e script command writes or executes and is not allowed")
	}

	return checkReadOnlyOutputFlags(words)
}

// allowedGroups summarises what the whitelist covers, so a refusal tells the
// model which kind of command to reach for instead of leaving it to guess.
func allowedGroups() string {
	return strings.Join([]string{
		groupInspect, groupSearch, groupFilter, groupGit, groupVerify, groupChange,
	}, "; ")
}

// checkReadOnlyCd allows `cd <dir>` only inside the workspace (a bare `cd`
// would leave the workspace entirely).
func checkReadOnlyCd(words []string, workspace string) error {
	args := words[1:]
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) != 1 {
		return fmt.Errorf("command blocked: read-only mode: cd needs exactly one directory (the shell already starts in the workspace)")
	}
	if workspace == "" {
		return nil
	}
	if _, err := safePathIn(workspace, "", args[0]); err != nil {
		return fmt.Errorf("command blocked: read-only mode: cd %q is outside the workspace", args[0])
	}
	return nil
}

// stripGitDirectory removes `git -C <dir>` (and the attached `-C<dir>` form)
// from a segment, refusing a directory outside the workspace. The remaining
// words are matched against the git whitelist as usual.
func stripGitDirectory(words []string, workspace string) ([]string, error) {
	if len(words) == 0 || words[0] != "git" {
		return words, nil
	}
	out := []string{"git"}
	for i := 1; i < len(words); i++ {
		w := words[i]
		var dir string
		switch {
		case w == "-C":
			if i+1 >= len(words) {
				return words, nil // malformed: the prefix match refuses it
			}
			dir, i = words[i+1], i+1
		case strings.HasPrefix(w, "-C") && len(w) > 2:
			dir = w[2:]
		default:
			out = append(out, w)
			continue
		}
		if workspace != "" {
			if _, err := safePathIn(workspace, "", dir); err != nil {
				return nil, fmt.Errorf("command blocked: read-only mode: git -C %q is outside the workspace", dir)
			}
		}
	}
	return out, nil
}

// sedScriptWrites reports whether a sed script argument carries a w (write) or e
// (execute) command. Only script arguments are inspected — the values of
// -e/--expression, or the first non-flag argument when -e is absent — so a file
// name that happens to contain "w" or "e" is not a false positive.
func sedScriptWrites(words []string) bool {
	var scripts []string
	seenExpression := false
	for i := 1; i < len(words); i++ {
		w := words[i]
		if w == "-e" || w == "--expression" {
			if i+1 < len(words) {
				scripts = append(scripts, words[i+1])
				i++
			}
			seenExpression = true
			continue
		}
		if !seenExpression && !strings.HasPrefix(w, "-") {
			scripts = append(scripts, w)
			seenExpression = true
		}
	}
	for _, s := range scripts {
		if sedWriteScript.MatchString(s) {
			return true
		}
	}
	return false
}

// checkReadOnlyOutputFlags refuses the output flags that would make a
// whitelisted verifier write a binary (possibly outside the workspace).
func checkReadOnlyOutputFlags(words []string) error {
	if len(words) < 2 || words[0] != "go" || (words[1] != "build" && words[1] != "test") {
		return nil
	}
	for _, w := range words[2:] {
		if w == "-o" || strings.HasPrefix(w, "-o=") {
			return fmt.Errorf("command blocked: read-only mode: %q writes an output file", w)
		}
	}
	return nil
}

// matchReadOnlyPrefix returns the longest whitelist entry whose words are a
// prefix of the segment words.
func matchReadOnlyPrefix(words []string) (readOnlyCommand, bool) {
	var best readOnlyCommand
	found := false
	for _, e := range readOnlyWhitelist {
		if len(words) < len(e.Words) || !equalWords(words[:len(e.Words)], e.Words) {
			continue
		}
		if !found || len(e.Words) > len(best.Words) {
			best, found = e, true
		}
	}
	return best, found
}

func equalWords(a, b []string) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
