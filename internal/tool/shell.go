package tool

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"gitee.com/kobeyoung81/nanosec/internal/provider"
)

// denyPattern pairs a compiled regexp with a human-readable name.
type denyPattern struct {
	Name    string
	Pattern *regexp.Regexp
}

// defaultDenyPatterns contains patterns that block dangerous shell commands.
var defaultDenyPatterns = []denyPattern{
	{"rm -rf", regexp.MustCompile(`(?i)\brm\s+-[rf]{1,2}\b`)},
	{"del /f or /q", regexp.MustCompile(`(?i)\bdel\s+/[fq]\b`)},
	{"rmdir /s", regexp.MustCompile(`(?i)\brmdir\s+/s\b`)},
	// The command word must stand on its own — `--format=%h` and
	// `--pretty=format:%h` are display flags, not a disk format — while the
	// mkfs.<fstype> spelling (mkfs.ext4) stays covered. The leading class covers
	// the start of the line, a shell separator, whitespace and a quote.
	{"disk format", regexp.MustCompile(`(?i)(^|[\s;&|"'])(format|diskpart|mkfs(\.[a-z0-9]+)?)([\s;]|$)`)},
	{"dd raw write", regexp.MustCompile(`(?i)\bdd\s+if=`)},
	{"block device redirect", regexp.MustCompile(`>\s*/dev/sd`)},
	{"system power", regexp.MustCompile(`(?i)\b(shutdown|reboot|poweroff)\b`)},
	{"fork bomb", regexp.MustCompile(`:\(\)\s*\{.*\};\s*:`)},
	{"find delete from root", regexp.MustCompile(`(?i)\bfind\s+/\s+[^;&|]*\s+-delete\b`)},
	{"shred", regexp.MustCompile(`(?i)\bshred\b`)},
	{"recursive chmod of root", regexp.MustCompile(`(?i)\bchmod\s+-R\s+777\s+/\b`)},
	{"recursive chown of root", regexp.MustCompile(`(?i)\bchown\s+-R\s+\S+\s+/\b`)},
	{"move root", regexp.MustCompile(`(?i)\bmv\s+/\s`)},
	{"proc sys redirect", regexp.MustCompile(`>\s*/proc/sys`)},
	{"etc credential write", regexp.MustCompile(`>\s*/etc/(passwd|shadow|sudoers)`)},
	{"wipe home data root", regexp.MustCompile(`(?i)\brm\s+-rf\s+(~/.nanosec|/root/.nanosec)\b`)},
	{"remove sharedspace link", regexp.MustCompile(`(?i)\b(rm|rmdir|unlink)\s+-?[rfd]*\s*(~/)?sharedspace\b`)},
	{"remove sharedspace dir", regexp.MustCompile(`(?i)\b(rm|rmdir)\s+-?[rfd]*\s*[^;\n]*\b/root/\.nanosec/sharedspace\b`)},
	{"move sharedspace", regexp.MustCompile(`(?i)\bmv\s+[^;\n]*\bsharedspace\b`)},
}

// ShellTool allows executing shell commands.
type ShellTool struct {
	Timeout         time.Duration
	Workspace       string
	SharedWorkspace string
	DenyPatterns    []denyPattern
}

func NewShellTool(timeout time.Duration, workspace string) *ShellTool {
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	return &ShellTool{Timeout: timeout, Workspace: workspace, DenyPatterns: defaultDenyPatterns}
}

// WithSharedWorkspace returns a copy of the tool with a shared workspace set.
func (t *ShellTool) WithSharedWorkspace(shared string) *ShellTool {
	c := *t
	c.SharedWorkspace = shared
	return &c
}

func (t *ShellTool) Name() string {
	return "bash"
}

func (t *ShellTool) Description() string {
	return "Execute a bash command. Use this for system operations, file manipulation, or running scripts. The command runs in a non-interactive shell."
}

func (t *ShellTool) Parameters() provider.ParameterSchema {
	return provider.ParameterSchema{
		Type: "object",
		Properties: map[string]provider.PropertySchema{
			"command": {
				Type:        "string",
				Description: "The bash command to execute (e.g., 'ls -la', 'cat file.txt')",
			},
		},
		Required: []string{"command"},
	}
}

func (t *ShellTool) Execute(ctx context.Context, params map[string]any) (string, error) {
	cmdStr, ok := params["command"].(string)
	if !ok {
		return "", fmt.Errorf("missing or invalid 'command' parameter")
	}

	// Check command against deny patterns
	for _, dp := range t.DenyPatterns {
		if dp.Pattern.MatchString(cmdStr) {
			return "", fmt.Errorf("command blocked: matches dangerous pattern '%s'", dp.Name)
		}
	}

	return execShell(ctx, cmdStr, t.Workspace, t.Timeout)
}

// shellOutputHeadBytes / shellOutputTailBytes bound what one command may hand
// back. The head is what a `cat` wants, the tail is what a failing test or a log
// wants, and 7 KB + 7 KB fits inside the default 16000-byte tool-result cap, so
// neither end is cut off by the global truncation. Before this bound a command
// could grow the agent's memory without limit (`cat` of a gigabyte file did
// exactly that) while the model still only ever saw the first 16 KB.
const (
	shellOutputHeadBytes = 7000
	shellOutputTailBytes = 7000
)

// boundedWriter keeps a stream's first shellOutputHeadBytes and its last
// shellOutputTailBytes, counting what it dropped in between.
type boundedWriter struct {
	head bytes.Buffer
	tail []byte
	omit int
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	n := len(p)
	if room := shellOutputHeadBytes - w.head.Len(); room > 0 {
		if len(p) <= room {
			w.head.Write(p)
			return n, nil
		}
		w.head.Write(p[:room])
		p = p[room:]
	}
	w.tail = append(w.tail, p...)
	if extra := len(w.tail) - shellOutputTailBytes; extra > 0 {
		w.tail = w.tail[extra:]
		w.omit += extra
	}
	return n, nil
}

// String renders the kept head, an omission marker and the kept tail.
func (w *boundedWriter) String() string {
	if w.omit == 0 {
		return w.head.String() + string(w.tail)
	}
	return w.head.String() + fmt.Sprintf("\n[... %d bytes omitted ...]\n", w.omit) + string(w.tail)
}

// execShell runs one bash command with the given timeout. The command runs in
// its own process group so the timeout can kill the whole tree: killing only
// the direct child (bash) leaves grandchildren (e.g. apk, tail) alive, and they
// keep the output pipe open, which makes Wait() block forever and the agent
// loop hang. Policy checks (denylist or read-only whitelist) belong to the
// calling tool and must run before this.
func execShell(ctx context.Context, cmdStr, workspace string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", "-c", cmdStr)
	if timeout > 0 {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}

	if workspace != "" {
		absWorkspace, err := filepath.Abs(workspace)
		if err != nil {
			return "", fmt.Errorf("invalid workspace path: %w", err)
		}
		cmd.Dir = absWorkspace
	}

	var stdout, stderr boundedWriter
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start command: %w", err)
	}

	// Watchdog: when the context deadline fires, SIGKILL the entire process
	// group (children included) so Wait() returns instead of blocking forever.
	done := make(chan struct{})
	if timeout > 0 {
		go func() {
			select {
			case <-ctx.Done():
				if cmd.Process != nil {
					_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				}
			case <-done:
			}
		}()
	}

	err := cmd.Wait()
	close(done)

	output := stdout.String()
	if errOut := stderr.String(); errOut != "" {
		if output != "" {
			output += "\n"
		}
		output += errOut
	}

	result := output
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return result, fmt.Errorf("command timed out after %s", timeout)
		}
		return result, fmt.Errorf("command failed: %w", err)
	}

	return result, nil
}
