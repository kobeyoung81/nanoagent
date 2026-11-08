package hypervisor

import (
	"bytes"
	"errors"
	"os/user"
	"regexp"
	"strings"
	"testing"
	"time"

	"gitee.com/kobeyoung81/nanosec/internal/workspace"
)

// newTestTerminals builds a manager rooted at a temp dir.
func newTestTerminals(t *testing.T) *TerminalManager {
	t.Helper()
	return NewTerminalManager(t.TempDir())
}

// waitTerminalCount polls until a terminal appears/disappears (bash boot races).
func waitTerminalCount(t *testing.T, m *TerminalManager, agentID string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(m.List(agentID)) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("terminal count for agent %s = %d, want %d", agentID, len(m.List(agentID)), want)
}

func TestTerminalOpenAndReplay(t *testing.T) {
	m := newTestTerminals(t)

	att, err := m.Open("agent-1", 80, 24, "", false)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if att.TerminalID == "" {
		t.Fatal("expected a terminal id")
	}
	waitTerminalCount(t, m, "agent-1", 1)

	// Capture output into the ring by attaching a background reader.
	done := make(chan struct{})
	go func() {
		_ = m.Input(att.TerminalID, []byte("printf 'hello-tty-123\n'\n"))
		close(done)
	}()
	// consume ring until the marker appears
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		snap := string(m.GetSnapshot(att.TerminalID))
		if strings.Contains(snap, "hello-tty-123") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	<-done

	// A second attach with replay returns the buffered output.
	att2, err := m.Open("agent-1", 80, 24, att.TerminalID, true)
	if err != nil {
		t.Fatalf("reattach: %v", err)
	}
	if att2.TerminalID != att.TerminalID {
		t.Fatalf("reattach id %q, want %q", att2.TerminalID, att.TerminalID)
	}
	if !strings.Contains(string(att2.Replay), "hello-tty-123") {
		t.Fatalf("replay missing marker: %q", att2.Replay)
	}
}

func TestTerminalInputEcho(t *testing.T) {
	m := newTestTerminals(t)
	att, err := m.Open("agent-2", 80, 24, "", false)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	waitTerminalCount(t, m, "agent-2", 1)

	// Shell echoes input back; run a command that prints a unique token.
	_token := "echotoken-a7d9"
	if err := m.Input(att.TerminalID, []byte("echo "+_token+"\n")); err != nil {
		t.Fatalf("input: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(string(m.GetSnapshot(att.TerminalID)), _token) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("expected input echo in terminal output")
}

// TestTerminalReplayRestoresModes verifies a replay taken while a TUI is on the
// alternate screen starts with the mode-restore prefix followed by the ring
// bytes; after the app tears down the alt screen the same replay has no prefix.
func TestTerminalReplayRestoresModes(t *testing.T) {
	m := newTestTerminals(t)
	att, err := m.Open("agent-9", 80, 24, "", false)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Get some real PTY output into the ring, like an app frame would.
	if err := m.Input(att.TerminalID, []byte("printf 'MARKER-REPLAY'\n")); err != nil {
		t.Fatalf("input: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(string(m.GetSnapshot(att.TerminalID)), "MARKER-REPLAY") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Simulate a TUI arming the alt screen + mouse modes (this is what the pump
	// would have seen in the PTY output).
	s, ok := m.Get(att.TerminalID)
	if !ok {
		t.Fatal("session missing")
	}
	s.modes.Scan([]byte("\x1b[?1049h\x1b[?1002h\x1b[?1006h\x1b[?2004h"))

	att2, err := m.Open("agent-9", 80, 24, att.TerminalID, true)
	if err != nil {
		t.Fatalf("reattach: %v", err)
	}
	if !bytes.HasPrefix(att2.Replay, []byte("\x1b[?1049h\x1b[?1002;1006;2004h")) {
		t.Fatalf("replay missing mode-restore prefix: %q", att2.Replay)
	}
	if !bytes.Contains(att2.Replay, []byte("MARKER-REPLAY")) {
		t.Fatalf("replay missing ring content: %q", att2.Replay)
	}

	// App exits cleanly: alt screen off, mouse off.
	s.modes.Scan([]byte("\x1b[?1049l\x1b[?1002l\x1b[?1006l\x1b[?2004l"))
	att3, err := m.Open("agent-9", 80, 24, att.TerminalID, true)
	if err != nil {
		t.Fatalf("reattach after teardown: %v", err)
	}
	if bytes.HasPrefix(att3.Replay, []byte("\x1b[?1049h")) {
		t.Fatalf("replay after teardown still restores alt screen: %q", att3.Replay)
	}
}

func TestTerminalKillCloses(t *testing.T) {
	m := newTestTerminals(t)
	att, err := m.Open("agent-3", 80, 24, "", false)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	waitTerminalCount(t, m, "agent-3", 1)

	s, ok := m.Get(att.TerminalID)
	if !ok {
		t.Fatal("session missing")
	}

	// Kill process group; bash must exit and the session reaped.
	if err := m.Kill(att.TerminalID); err != nil {
		t.Fatalf("kill: %v", err)
	}
	select {
	case <-s.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("session not closed after kill")
	}
	waitTerminalCount(t, m, "agent-3", 0)
}

func TestTerminalExitReaps(t *testing.T) {
	m := newTestTerminals(t)
	att, err := m.Open("agent-4", 80, 24, "", false)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	waitTerminalCount(t, m, "agent-4", 1)

	// Typed "exit" ends bash cleanly and the pump reaps the session.
	if err := m.Input(att.TerminalID, []byte("exit\n")); err != nil {
		t.Fatalf("input: %v", err)
	}
	waitTerminalCount(t, m, "agent-4", 0)
}

func TestTerminalCapLruReap(t *testing.T) {
	m := newTestTerminals(t)
	for i := 0; i < maxTerminalsPerAgent; i++ {
		_, err := m.Open("agent-5", 80, 24, "", false)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
	}
	waitTerminalCount(t, m, "agent-5", maxTerminalsPerAgent)

	// Opening one more reaps the oldest so the cap holds.
	if _, err := m.Open("agent-5", 80, 24, "", false); err != nil {
		t.Fatalf("open over cap: %v", err)
	}
	waitTerminalCount(t, m, "agent-5", maxTerminalsPerAgent)
}

func TestTerminalResize(t *testing.T) {
	m := newTestTerminals(t)
	att, err := m.Open("agent-6", 120, 40, "", false)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	waitTerminalCount(t, m, "agent-6", 1)

	if err := m.Resize(att.TerminalID, 160, 50); err != nil {
		t.Fatalf("resize: %v", err)
	}

	// Verify via stty inside the session. `stty size` prints rows then columns.
	if err := m.Input(att.TerminalID, []byte("stty size\n")); err != nil {
		t.Fatalf("input: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if regexp.MustCompile(`50\s+160`).MatchString(string(m.GetSnapshot(att.TerminalID))) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("expected \"50 160\" from stty size")
}

func TestTerminalResizeBounds(t *testing.T) {
	m := newTestTerminals(t)
	att, err := m.Open("agent-7", 100, 30, "", false)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	waitTerminalCount(t, m, "agent-7", 1)

	// Degenerate sizes are ignored (no TIOCSWINSZ with 0/negative).
	if err := m.Resize(att.TerminalID, 0, 0); err != nil {
		t.Fatalf("resize 0x0: %v", err)
	}
	if err := m.Resize(att.TerminalID, -1, -1); err != nil {
		t.Fatalf("resize negative: %v", err)
	}

	// Oversized requests are clamped to sane bounds but still applied.
	if err := m.Resize(att.TerminalID, 5000, 5000); err != nil {
		t.Fatalf("resize clamp: %v", err)
	}

	// Degenerate sizes (the 2x1 floor that crashes TUI apps like opencode, and
	// any below-minimum request) must be ignored, leaving the last good size.
	for _, sz := range [][2]int{{2, 1}, {1, 1}, {9, 3}, {80, 1}} {
		if err := m.Resize(att.TerminalID, sz[0], sz[1]); err != nil {
			t.Fatalf("resize %dx%d: %v", sz[0], sz[1], err)
		}
	}

	if err := m.Input(att.TerminalID, []byte("stty size\n")); err != nil {
		t.Fatalf("input: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if regexp.MustCompile(`500\s+1000`).MatchString(string(m.GetSnapshot(att.TerminalID))) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("expected \"500 1000\" from stty size after clamp")
}

// TestTerminalSpawnRejectsDegenerateSize verifies a PTY is never created at a
// degenerate size: a small/zero request falls back to the default.
func TestTerminalSpawnRejectsDegenerateSize(t *testing.T) {
	m := newTestTerminals(t)
	att, err := m.Open("agent-8", 2, 1, "", false)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	waitTerminalCount(t, m, "agent-8", 1)
	defer m.Kill(att.TerminalID)

	if err := m.Input(att.TerminalID, []byte("stty size\n")); err != nil {
		t.Fatalf("input: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if regexp.MustCompile(`24\s+80`).MatchString(string(m.GetSnapshot(att.TerminalID))) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected default \"24 80\" from stty size, got %q", m.GetSnapshot(att.TerminalID))
}

func TestBoundedBuffer(t *testing.T) {
	b := newBoundedBuffer(8)
	payload := []byte("abcdefghijklmnop")
	b.Write(payload)
	if got := b.Snapshot(); string(got) != string(payload[len(payload)-8:]) {
		t.Fatalf("snapshot = %q, want last 8 bytes", got)
	}

	b.Write([]byte("!@"))
	if got := b.Snapshot(); string(got) != "klmnop!@" {
		t.Fatalf("snapshot after second write = %q, want \"klmnop!@\"", got)
	}
}

func TestTerminalInputRejectsUnknown(t *testing.T) {
	m := newTestTerminals(t)
	if err := m.Input("missing", []byte("x")); err == nil {
		t.Fatal("expected error for unknown terminal")
	}
	if err := m.Resize("missing", 1, 1); err == nil {
		t.Fatal("expected error for unknown terminal")
	}
	if err := m.Kill("missing"); err == nil {
		t.Fatal("expected error for unknown terminal")
	}
}

// GetSnapshot is a test helper to read the current ring buffer.
func (m *TerminalManager) GetSnapshot(terminalID string) []byte {
	s, ok := m.Get(terminalID)
	if !ok {
		return nil
	}
	return s.ring.Snapshot()
}

func TestTerminalHome(t *testing.T) {
	home := terminalHome()
	if home == "" {
		t.Fatal("terminalHome() returned empty")
	}
	if strings.Contains(home, "/agents/") {
		t.Errorf("terminalHome() = %q, must not point into an agent home", home)
	}
}

// TestTerminalHomeFallback pins the branch taken when /etc/passwd cannot be
// read: the shell must still get the container user's home, never the agent
// home (which is what an os.UserHomeDir() fallback would have produced).
func TestTerminalHomeFallback(t *testing.T) {
	orig := currentUser
	currentUser = func() (*user.User, error) { return nil, errors.New("no passwd entry") }
	defer func() { currentUser = orig }()

	t.Setenv("AGENT_USERNAME", "kobeyoung81")
	got := terminalHome()
	if got != "/home/kobeyoung81" {
		t.Errorf("terminalHome() = %q, want /home/kobeyoung81", got)
	}
	if strings.Contains(got, "/agents/") {
		t.Errorf("fallback must not point into an agent home, got %q", got)
	}

	t.Setenv("AGENT_USERNAME", "")
	if got := terminalHome(); got != "/home/nanosec" {
		t.Errorf("terminalHome() with empty AGENT_USERNAME = %q, want /home/nanosec", got)
	}
}

func TestTerminalOpenUsesUserHomeEnv(t *testing.T) {
	m := newTestTerminals(t)
	att, err := m.Open("agent-1", 80, 24, "", false)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Kill(att.TerminalID)

	home := terminalHome()
	if err := m.Input(att.TerminalID, []byte("printf 'HOME=%s\\n' \"$HOME\"\r")); err != nil {
		t.Fatal(err)
	}
	// Poll for the concrete value, not the field name: the ring buffer also
	// holds the terminal's echo of the typed command, which contains "HOME=".
	want := "HOME=" + home
	deadline := time.Now().Add(5 * time.Second)
	var out string
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		att2, err := m.Open("agent-1", 80, 24, att.TerminalID, true)
		if err != nil {
			t.Fatal(err)
		}
		out = string(att2.Replay)
		if strings.Contains(out, want) {
			break
		}
	}
	if !strings.Contains(out, want) {
		t.Errorf("spawned shell HOME not %q in output %q", home, out)
	}
}

// TestTerminalOpenStartsInAgentWorkspace pins the other half of the terminal
// path contract: HOME is the user home, but the shell starts in the agent's
// own workspace (the agent home folder under <dataRoot>/agents). cd with no
// arguments therefore lands in the user home, not here.
func TestTerminalOpenStartsInAgentWorkspace(t *testing.T) {
	m := newTestTerminals(t)
	att, err := m.Open("agent-1", 80, 24, "", false)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Kill(att.TerminalID)
	waitTerminalCount(t, m, "agent-1", 1)

	want := workspace.NewNamed("agent-1", m.homeName("agent-1"), m.dataRoot).AgentWorkspace()
	if err := m.Input(att.TerminalID, []byte("printf 'PWD=%s\\n' \"$PWD\"\r")); err != nil {
		t.Fatal(err)
	}

	// Same caveat as TestTerminalOpenUsesUserHomeEnv: poll for the value, since
	// the echo of the command itself contains the "PWD=" prefix.
	marker := "PWD=" + want
	deadline := time.Now().Add(5 * time.Second)
	var out string
	for time.Now().Before(deadline) {
		out = string(m.GetSnapshot(att.TerminalID))
		if strings.Contains(out, marker) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(out, marker) {
		t.Errorf("spawned shell cwd not %q in output %q", want, out)
	}
}

func TestWithEnvReplacesRatherThanAppends(t *testing.T) {
	env := []string{"PATH=/bin", "HOME=/old", "HOME=/duplicate"}
	got := withEnv(env, "HOME", "/new")

	var homes []string
	for _, kv := range got {
		if strings.HasPrefix(kv, "HOME=") {
			homes = append(homes, kv)
		}
	}
	if len(homes) != 1 || homes[0] != "HOME=/new" {
		t.Fatalf("HOME entries = %v, want exactly [HOME=/new]", homes)
	}
	// The duplicate HOME is collapsed, so the two non-HOME entries plus the
	// single replacement make two.
	if len(got) != 2 {
		t.Errorf("env length = %d, want 2 (%v)", len(got), got)
	}
	if got[0] != "PATH=/bin" {
		t.Errorf("unrelated entry dropped or reordered: %v", got)
	}
}

func TestWithEnvAppendsWhenKeyAbsent(t *testing.T) {
	got := withEnv([]string{"PATH=/bin"}, "TERM", "xterm-256color")
	if len(got) != 2 || got[1] != "TERM=xterm-256color" {
		t.Fatalf("got %v, want [PATH=/bin TERM=xterm-256color]", got)
	}
}
