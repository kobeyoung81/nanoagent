package tool

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func checkAllowed(t *testing.T, cmd string) {
	t.Helper()
	if err := CheckReadOnlyCommand(cmd, ""); err != nil {
		t.Errorf("expected %q to be allowed, got: %v", cmd, err)
	}
}

func checkRefused(t *testing.T, cmd, workspace string) {
	t.Helper()
	err := CheckReadOnlyCommand(cmd, workspace)
	if err == nil {
		t.Errorf("expected %q to be refused", cmd)
		return
	}
	if !strings.HasPrefix(err.Error(), "command blocked: read-only mode:") {
		t.Errorf("unexpected error for %q: %v", cmd, err)
	}
}

func TestCheckReadOnlyCommand_AllowsWhitelisted(t *testing.T) {
	cases := []string{
		"git status",
		"git diff --stat",
		"git log --oneline -5",
		"git log | head -5",
		"git show HEAD -- internal/agent/loop.go",
		"git branch --show-current",
		"git branch -a",
		"git remote -v",
		"git config --get user.email",
		"git rev-parse --abbrev-ref HEAD",
		"ls -la",
		"cat go.mod | wc -l",
		"grep -rn 'func Foo' .",
		"rg TODO",
		"find . -name '*.go' -type f",
		"go test ./...",
		"go vet ./internal/...",
		"go list ./...",
		"make -n build",
		// Widened surface (2026-10-16): filters, more inspect commands, more
		// read-only git subcommands, cd, and quote-aware parsing.
		"grep -rn x f | sort | uniq -c",
		"grep -rn -e 'a>b' -e 'a;b' f",
		"sed -n '1,5p' f",
		"sed -n -e '1,5p' -e '10,20p' f",
		"jq . x.json",
		"jq '.[] | .name' x.json | head -5",
		"sort -u f | head -20",
		"cut -d, -f1 f",
		"cat f | tr -d ' '",
		"paste a b",
		"diff -u a b | head -20",
		"sha256sum f",
		"basename /a/b/c",
		"realpath .",
		"date +%s",
		"printf '%s\\n' x",
		"echo done",
		"id -u",
		"df -h .",
		"git log --pretty='%h %s'",
		"git rev-list --count HEAD",
		"git for-each-ref --format='%(refname)'",
		"git cat-file -p HEAD:go.mod",
		"git worktree list",
		"git submodule status",
		"cat 'a>b'",
		"cd nanosec && go test ./... -short",
		"cd . && ls",
	}
	for _, cmd := range cases {
		checkAllowed(t, cmd)
	}
}

func TestCheckReadOnlyCommand_RefusesWritesAndEscapes(t *testing.T) {
	cases := []string{
		// Not whitelisted at all.
		"rm -rf .",
		"git push",
		"git commit -m x",
		"git checkout -b x",
		"git stash",
		"git fetch",
		"curl -o f http://example.com",
		"wget http://example.com",
		"go run .",
		"bash -c 'rm f'",
		"sh -c ls",
		"python3 -c 'open(\"f\",\"w\")'",
		"node -e 'x'",
		"mysql -e 'drop table x'",
		"sudo ls",
		"eval ls",
		"source ./env.sh",
		"sed -i s/a/b/ f",
		"awk '{print > \"f\"}' x",
		"tee f",
		"cp a b",
		"mv a b",
		"mkdir d",
		"touch f",
		"make",
		"FOO=bar ls",
		"xargs ls",
		"env FOO=bar ls",
		// Write flags on allowed commands.
		"git branch -D feature",
		"git branch -m old new",
		"git remote add origin git@gitee.com:x/y.git",
		"git remote set-url origin x",
		"git diff --output=z",
		"go build -o /tmp/x .",
		"go test -o /tmp/x .",
		"find . -name x -delete",
		"find . -name x -exec rm {} ;",
		"find . -name x -fprint out.txt",
		// Filters that would write through their own flags or sed commands.
		"sort -o out f",
		"sort --output=out f",
		"sed -i.bak s/a/b/ f",
		"sed --in-place s/a/b/ f",
		"sed -n 'w out' f",
		"sed 's/a/b/w out' f",
		"sed '1e touch /tmp/x' f",
		"jq -i . f",
		"date -s '2020-01-01'",
		// Quotes do not smuggle a banned token past the check.
		"find . '-delete'",
		"find . \"-exec\" rm {} ;",
		// Shell constructs that smuggle a second command.
		"cat a > b",
		"echo x >> y",
		"ls < input",
		"ls; rm -rf x",
		"ls && rm -rf x",
		"ls || rm -rf x",
		"cat x | tee y",
		"ls & rm -rf x",
		"ls\nrm -rf x",
		"echo $(rm -rf /)",
		"echo `id`",
		"cat <(rm -rf /)",
		"",
		"   ",
	}
	for _, cmd := range cases {
		checkRefused(t, cmd, "")
	}
}

// TestCheckReadOnlyCommand_BoundsWorkspace pins the two path-bounded forms: `cd`
// and `git -C` may only point inside the workspace.
func TestCheckReadOnlyCommand_BoundsWorkspace(t *testing.T) {
	ws := t.TempDir()

	for _, cmd := range []string{
		"cd " + ws + " && ls",
		"cd . && ls",
		"cd " + filepath.Join(ws, "sub") + " && git status",
		"git -C " + ws + " log --oneline -1",
		"git -C " + ws + " status",
	} {
		if err := CheckReadOnlyCommand(cmd, ws); err != nil {
			t.Errorf("expected %q to be allowed inside the workspace, got: %v", cmd, err)
		}
	}

	for _, cmd := range []string{
		"cd /etc && ls",
		"cd ..",
		"cd ../..",
		"cd",
		"cd a b",
		"git -C /etc log",
		"git -C " + filepath.Dir(ws) + " log",
		"cd /etc; ls",
	} {
		if err := CheckReadOnlyCommand(cmd, ws); err == nil {
			t.Errorf("expected %q to be refused outside the workspace", cmd)
		}
	}
}

// TestReadOnlyShellTool_RefusesBeforeExecuting proves the guard runs before the
// process starts: a refused command must not touch the filesystem.
func TestReadOnlyShellTool_RefusesBeforeExecuting(t *testing.T) {
	dir := t.TempDir()
	tool := NewReadOnlyShellTool(0, dir)

	_, err := tool.Execute(context.Background(), map[string]any{"command": "rm -rf " + dir + "/x"})
	if err == nil {
		t.Fatal("expected the write command to be refused")
	}
	if !strings.Contains(err.Error(), "not a whitelisted read-only command") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestReadOnlyShellTool_ExecutesWhitelisted(t *testing.T) {
	dir := t.TempDir()
	tool := NewReadOnlyShellTool(0, dir)

	out, err := tool.Execute(context.Background(), map[string]any{"command": "pwd && ls | wc -l"})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, dir) {
		t.Errorf("expected output to mention the workspace %q, got: %q", dir, out)
	}
}

// TestReadOnlyShellTool_CdThenFilter checks the widened surface end to end: a
// compound command that cds inside the workspace and pipes through a filter.
func TestReadOnlyShellTool_CdThenFilter(t *testing.T) {
	dir := t.TempDir()
	tool := NewReadOnlyShellTool(0, dir)

	out, err := tool.Execute(context.Background(),
		map[string]any{"command": "cd " + dir + " && printf 'b\\na\\nb\\n' | sort | uniq -c"})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, "2 b") || !strings.Contains(out, "1 a") {
		t.Errorf("expected sorted counts, got: %q", out)
	}
}

func TestReadOnlyShellTool_Name(t *testing.T) {
	if got := NewReadOnlyShellTool(0, "").Name(); got != "readonly_bash" {
		t.Errorf("Name() = %q, want readonly_bash", got)
	}
}
