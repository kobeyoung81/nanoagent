package tool

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestShellTool_BoundsHugeOutput pins the memory guard: a command that prints
// megabytes must come back as a bounded head+tail, not as everything.
func TestShellTool_BoundsHugeOutput(t *testing.T) {
	st := NewShellTool(30*time.Second, "")
	out, err := st.Execute(context.Background(), map[string]any{
		"command": "yes abcdefghij | head -c 3000000",
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(out) > 20000 {
		t.Errorf("output is %d bytes — not bounded", len(out))
	}
	if !strings.HasPrefix(out, "abcdefghij") {
		t.Errorf("the head should be kept, got: %q", out[:min(40, len(out))])
	}
	if !strings.Contains(out, "bytes omitted") {
		t.Errorf("expected an omission marker, got: %q", out[:min(80, len(out))])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestDefaultDenyPatterns_BlocksSharedspaceRemoval(t *testing.T) {
	cases := []string{
		"rm -rf sharedspace",
		"rm -rf ~/sharedspace",
		"rm -rf /root/.nanosec/sharedspace",
		"rmdir sharedspace",
		"unlink sharedspace",
		"mv sharedspace /tmp",
	}
	for _, cmd := range cases {
		blocked := false
		for _, dp := range defaultDenyPatterns {
			if dp.Pattern.MatchString(cmd) {
				blocked = true
				break
			}
		}
		if !blocked {
			t.Errorf("expected command to be blocked by deny patterns: %q", cmd)
		}
	}
}

// TestDefaultDenyPatterns_FormatIsNotAFalsePositive pins the boundary of the
// disk-format pattern: a display flag that merely contains the word (--format=,
// --pretty=format:) is a normal read-only invocation, while a real format
// command word is still blocked.
func TestDefaultDenyPatterns_FormatIsNotAFalsePositive(t *testing.T) {
	allowed := []string{
		"git log --format=%h",
		"git log --pretty=format:'%h %s'",
		"git for-each-ref --format='%(refname)'",
		"go test -run TestFormat ./...",
	}
	for _, cmd := range allowed {
		for _, dp := range defaultDenyPatterns {
			if dp.Pattern.MatchString(cmd) {
				t.Errorf("command should not be blocked by %q: %q", dp.Name, cmd)
			}
		}
	}

	blocked := []string{
		"format C:",
		"mkfs /dev/sda1",
		"mkfs.ext4 /dev/sda1",
		"diskpart",
		"sudo format C:",
		`bash -c "format C:"`,
		// Residual (pre-existing) false positive: the word standing alone as an
		// argument is indistinguishable from a command word here. readonly_bash
		// has no denylist, so a search for the literal still has a route.
		"grep -rn format .",
	}
	for _, cmd := range blocked {
		matched := false
		for _, dp := range defaultDenyPatterns {
			if dp.Pattern.MatchString(cmd) {
				matched = true
				break
			}
		}
		if !matched {
			t.Errorf("expected command to be blocked by deny patterns: %q", cmd)
		}
	}
}

func TestDefaultDenyPatterns_AllowsLegitCommands(t *testing.T) {
	cases := []string{
		"ls -la",
		"cat file.txt",
		"mkdir -p project/sharedspace-cache",
		"cp -r sharedspace notes.md",
	}
	for _, cmd := range cases {
		for _, dp := range defaultDenyPatterns {
			if dp.Pattern.MatchString(cmd) {
				t.Errorf("command should not be blocked by %q: %q", dp.Name, cmd)
			}
		}
	}
}
