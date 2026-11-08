// Terminal sessions for the agent container. The hypervisor spawns interactive
// bash processes (one per terminal) under a PTY. The two paths are deliberate
// and differ: the shell's working directory (its starting point) is the agent's
// own workspace, while HOME is the container user's home. So 'cd' with no
// argument lands in the user home, not in the agent workspace.
//
// This is the terminal's contract only. The agent process itself keeps a
// different HOME (main.go points it at the agent root so tool state stays
// per-agent); do not "unify" the two.
//
// Sessions live as long as the bash process (or the container); output is
// buffered in a bounded ring so a reconnecting browser can replay the visible
// screen. Exactly one output consumer exists (the bus tunnel), so each session
// has a single goroutine pumping PTY output into the ring buffer and, when a
// tunnel is attached, into tunnel_stream_output frames.
package hypervisor

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"gitee.com/kobeyoung81/nanoprotocol"
	"github.com/creack/pty"
	"github.com/google/uuid"

	"gitee.com/kobeyoung81/nanosec/internal/workspace"
)

// terminalRingSize bounds the replay buffer per terminal session.
const terminalRingSize = 64 << 10

// maxTerminalsPerAgent caps concurrent sessions per agent (oldest reaped).
const maxTerminalsPerAgent = 5

// currentUser resolves the runtime identity. It is a package variable so tests
// can simulate a machine where /etc/passwd cannot be read — the one failure
// terminalHome cannot provoke for real.
var currentUser = user.Current

// fallbackHome is the home the entrypoint would have created for the container's
// runtime identity: /home/<AGENT_USERNAME>, with "nanosec" when unset. The
// platform sanitizes AGENT_USERNAME upstream (the entrypoint re-sanitizes with
// the same rules), so it is used as-is.
func fallbackHome(username string) string {
	if username == "" {
		username = "nanosec"
	}
	return "/home/" + username
}

// terminalHome returns the container user's home directory. The agent binary
// is CGO-disabled, so os/user.Current() resolves via /etc/passwd — immune to
// the process-global HOME override (per-agent) that main.go applies, which is
// exactly why it is preferred over os.UserHomeDir() here.
//
// There is deliberately no os.UserHomeDir() fallback: that would return $HOME,
// which for an agent process is the agent home — precisely what this contract
// forbids. If /etc/passwd cannot be read, fall back to the entrypoint's naming
// rule and say so loudly rather than silently pointing the shell into an agent
// home.
func terminalHome() string {
	if u, err := currentUser(); err == nil && u.HomeDir != "" {
		return u.HomeDir
	}
	home := fallbackHome(os.Getenv("AGENT_USERNAME"))
	slog.Warn("cannot resolve the runtime user's home from /etc/passwd; using the entrypoint's naming rule",
		"home", home)
	return home
}

var errTerminalNotFound = errors.New("terminal not found")

// withEnv returns env with key set to value, replacing any existing entry for
// that key instead of appending a second one. Appending a duplicate is not a
// reliable override: consumers disagree on which entry wins (glibc getenv
// returns the first match, bash's environment import keeps the last), so the
// shell may end up with the value we meant to replace.
func withEnv(env []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, prefix) {
			out = append(out, kv)
		}
	}
	return append(out, prefix+value)
}

// TerminalSession is one live bash session under a PTY.
type TerminalSession struct {
	ID        string
	AgentID   string
	CreatedAt time.Time

	cmd    *exec.Cmd
	pty    *os.File
	ring   *boundedBuffer
	modes  modeTracker // DEC private-mode state for replay restore prefixes
	once   sync.Once
	closed chan struct{}
	sizeMu sync.Mutex
	cols   int
	rows   int
}

// TerminalInfo is the surface exposed to the portal for tab restore.
type TerminalInfo struct {
	TerminalID string `json:"terminal_id"`
	AgentID    string `json:"agent_id"`
	CreatedAt  int64  `json:"created_at"` // unix seconds
}

// Attachment is returned to callers of TerminalManager.Open.
type Attachment struct {
	TerminalID string `json:"terminal_id"`
	Replay     []byte `json:"replay"`
}

// TerminalManager owns all terminal sessions for the container's user. It is
// referenced by both the tunnel (stream frames) and the HTTP bridge (REST
// list/attach/kill).
type TerminalManager struct {
	mu       sync.Mutex
	dataRoot string
	sessions map[string]*TerminalSession // terminal_id → session

	// homeName resolves an agent id to its home folder name (registry-backed),
	// used to build the shell's starting directory (the agent's own workspace
	// under <dataRoot>/agents). It is deliberately not used for HOME. Defaults
	// to the agent id.
	homeName func(agentID string) string

	// send, when non-nil, writes a tunnel frame back to the bus. It is
	// (re)installed on every tunnel connect and cleared on disconnect so
	// sessions keep running across tunnel blips.
	send func(nanoprotocol.Message)
}

func NewTerminalManager(dataRoot string) *TerminalManager {
	return &TerminalManager{
		dataRoot: dataRoot,
		sessions: make(map[string]*TerminalSession),
		homeName: func(agentID string) string { return agentID },
	}
}

// SetHomeResolver wires the agentID -> home folder name lookup used to build
// the shell's starting directory (the agent's own workspace). It does not
// affect HOME.
func (m *TerminalManager) SetHomeResolver(fn func(agentID string) string) {
	if fn != nil {
		m.homeName = fn
	}
}

// SetSender (re)wires the output channel to the currently connected tunnel.
func (m *TerminalManager) SetSender(send func(nanoprotocol.Message)) {
	m.mu.Lock()
	m.send = send
	m.mu.Unlock()
}

// List returns the live sessions for an agent, oldest first.
func (m *TerminalManager) List(agentID string) []TerminalInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []TerminalInfo
	for _, s := range m.sessions {
		if s.AgentID != agentID {
			continue
		}
		out = append(out, TerminalInfo{
			TerminalID: s.ID,
			AgentID:    s.AgentID,
			CreatedAt:  s.CreatedAt.Unix(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out
}

// Get fetches a session by terminal id.
func (m *TerminalManager) Get(terminalID string) (*TerminalSession, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[terminalID]
	return s, ok
}

// Open attaches to an existing session (optionally replaying its ring buffer)
// or spawns a new bash session for the agent. If the agent is already at the
// session cap the oldest session is reaped to make room.
func (m *TerminalManager) Open(agentID string, cols, rows int, terminalID string, replay bool) (*Attachment, error) {
	if terminalID != "" {
		m.mu.Lock()
		s, ok := m.sessions[terminalID]
		m.mu.Unlock()
		if ok && s.AgentID == agentID {
			if cols > 0 && rows > 0 {
				_ = applySize(s, cols, rows)
			}
			if !replay {
				return &Attachment{TerminalID: s.ID}, nil
			}
			// Re-arm the terminal's DEC mode state (alt screen, mouse tracking,
			// bracketed paste...) before replaying the ring tail. Without this, a
			// replay into a fresh normal-buffer terminal paints TUI frames into the
			// wrong buffer when the arming sequence scrolled out of the ring, and
			// the app's later mode resets become no-ops.
			return &Attachment{TerminalID: s.ID, Replay: s.withRestorePrefix(s.ring.Snapshot())}, nil
		}
	}

	m.mu.Lock()
	count := 0
	for _, s := range m.sessions {
		if s.AgentID == agentID {
			count++
		}
	}
	if count >= maxTerminalsPerAgent {
		// Reap the oldest session for this agent to make room.
		var oldest *TerminalSession
		for _, s := range m.sessions {
			if s.AgentID == agentID && (oldest == nil || s.CreatedAt.Before(oldest.CreatedAt)) {
				oldest = s
			}
		}
		m.mu.Unlock()
		if oldest != nil {
			m.Kill(oldest.ID)
		}
		m.mu.Lock()
	}

	id := uuid.NewString()
	dir := workspace.NewNamed(agentID, m.homeName(agentID), m.dataRoot).AgentWorkspace()
	sess, err := m.spawn(id, agentID, dir, cols, rows)
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	m.sessions[id] = sess
	m.mu.Unlock()

	go m.pump(sess)
	return &Attachment{TerminalID: sess.ID}, nil
}

// spawn starts an interactive bash under a PTY. caller holds m.mu.
func (m *TerminalManager) spawn(id, agentID, dir string, cols, rows int) (*TerminalSession, error) {
	// Never create a PTY at a degenerate size (a 2x1 PTY crashes TUI apps like
	// opencode on first layout); fall back to the default when the request is
	// missing or too small.
	cols, rows = spawnSize(cols, rows)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir workspace: %w", err)
	}

	cmd := exec.Command("bash", "-i")
	cmd.Dir = dir
	// Build the environment by replacement, never by appending a duplicate key.
	env := os.Environ()
	env = withEnv(env, "TERM", "xterm-256color")
	env = withEnv(env, "COLORTERM", "truecolor")
	// HOME is the container user's home (/home/<username>: the runtime identity's
	// passwd home, which hosts the skel .bashrc/.profile seating the shell
	// prompt, ls --color=auto and LS_COLORS). The agent process itself keeps a
	// different HOME — main.go points it at the agent root so tool state stays
	// per-agent — and the two are deliberately separate: this is the user-level
	// channel, that is the agent-level one. The working directory is the agent's
	// own workspace, so 'cd ~' lands in the user home.
	if home := terminalHome(); home != "" {
		env = withEnv(env, "HOME", home)
	} else {
		env = withEnv(env, "HOME", dir)
	}
	cmd.Env = env

	f, err := pty.StartWithAttrs(cmd, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)}, &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
	})
	if err != nil {
		return nil, fmt.Errorf("start bash: %w", err)
	}

	return &TerminalSession{
		ID:        id,
		AgentID:   agentID,
		CreatedAt: time.Now(),
		cmd:       cmd,
		pty:       f,
		ring:      newBoundedBuffer(terminalRingSize),
		modes:     *newModeTracker(),
		closed:    make(chan struct{}),
		cols:      cols,
		rows:      rows,
	}, nil
}

// pump reads PTY output into the ring buffer and forwards output frames to the
// tunnel. It ends when the bash process exits or the PTY is closed.
func (m *TerminalManager) pump(s *TerminalSession) {
	buf := make([]byte, 4096)
	for {
		n, err := s.pty.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			// Track DEC private modes (alt screen, mouse, bracketed paste...)
			// so a replay into a fresh terminal can restore them later.
			s.modes.Scan(chunk)
			s.ring.Write(chunk)
			m.sendOutput(s, chunk)
		}
		if err != nil {
			if err != io.EOF {
				slog.Debug("terminal read ended", "terminal_id", s.ID, "error", err)
			}
			break
		}
	}
	m.finish(s, "exit")
}

// withRestorePrefix prepends the tracker's mode-restore prefix to a replay
// snapshot. The tracker is fed by the pump goroutine; reading its state here
// races only against mode flips carried by output the pump is about to send, so
// the value may lag by one chunk. That is acceptable: the restore exists to fix
// gross desyncs, not to be a byte-exact transcript.
func (s *TerminalSession) withRestorePrefix(replay []byte) []byte {
	prefix := s.modes.restorePrefix()
	if len(prefix) == 0 {
		return replay
	}
	out := make([]byte, 0, len(prefix)+len(replay))
	out = append(out, prefix...)
	out = append(out, replay...)
	return out
}

// sendOutput forwards one chunk of PTY output to the current tunnel, if any.
func (m *TerminalManager) sendOutput(s *TerminalSession, data []byte) {
	m.mu.Lock()
	send := m.send
	m.mu.Unlock()
	if send == nil {
		return
	}
	out, err := nanoprotocol.NewMessage(nanoprotocol.TypeTunnelStreamOutput, nanoprotocol.TunnelStreamOutput{
		StreamID: s.ID,
		Data:     data,
	})
	if err == nil {
		send(out)
	}
}

// finish tears a session down exactly once: closes the PTY, removes it from the
// registry and reports tunnel_stream_closed to the bus.
func (m *TerminalManager) finish(s *TerminalSession, reason string) {
	s.once.Do(func() {
		m.mu.Lock()
		delete(m.sessions, s.ID)
		send := m.send
		m.mu.Unlock()

		_ = s.pty.Close()

		if send != nil {
			closed, err := nanoprotocol.NewMessage(nanoprotocol.TypeTunnelStreamClosed, nanoprotocol.TunnelStreamClosed{
				StreamID: s.ID,
				Reason:   reason,
			})
			if err == nil {
				send(closed)
			}
		}
		close(s.closed)
		slog.Info("terminal session closed", "terminal_id", s.ID, "reason", reason)
	})
}

// Input writes PTY input (keystrokes) for a session.
func (m *TerminalManager) Input(terminalID string, data []byte) error {
	s, ok := m.Get(terminalID)
	if !ok {
		return errTerminalNotFound
	}
	_, err := s.pty.Write(data)
	return err
}

// PTY window-size bounds. Requests below the minimum are refused rather than
// clamped: a 2x1 TIOCSWINSZ (emitted by a browser terminal whose canvas is
// detached during panel re-parenting) is known to panic TUI apps like opencode
// during their initial layout, which is the very case this guard exists to
// prevent. Oversized requests are safely clamped down.
const (
	termMinCols = 10
	termMinRows = 4
	termMaxCols = 1000
	termMaxRows = 500
	termDefCols = 80
	termDefRows = 24
)

// sanitizeSize clamps a requested PTY size to the supported range. The bool is
// false when the request is degenerate (below the minimum) and must be ignored.
func sanitizeSize(cols, rows int) (int, int, bool) {
	if cols < termMinCols || rows < termMinRows {
		return cols, rows, false
	}
	if cols > termMaxCols {
		cols = termMaxCols
	}
	if rows > termMaxRows {
		rows = termMaxRows
	}
	return cols, rows, true
}

// spawnSize normalizes the size used to create a PTY, falling back to the
// default when the request is missing or degenerate so a session is never born
// with a crash-inducing size.
func spawnSize(cols, rows int) (int, int) {
	cols, rows, ok := sanitizeSize(cols, rows)
	if !ok {
		return termDefCols, termDefRows
	}
	return cols, rows
}

// applySize sets the PTY window size, skipping degenerate requests and no-op
// changes so a TUI does not receive a crash-inducing or gratuitous SIGWINCH.
func applySize(s *TerminalSession, cols, rows int) error {
	cols, rows, ok := sanitizeSize(cols, rows)
	if !ok {
		slog.Debug("terminal resize ignored (degenerate)", "terminal_id", s.ID, "cols", cols, "rows", rows)
		return nil
	}
	s.sizeMu.Lock()
	unchanged := s.cols == cols && s.rows == rows
	s.cols, s.rows = cols, rows
	s.sizeMu.Unlock()
	if unchanged {
		return nil
	}
	slog.Debug("terminal resize", "terminal_id", s.ID, "cols", cols, "rows", rows)
	return pty.Setsize(s.pty, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
}

// Resize updates the PTY window size for a session.
func (m *TerminalManager) Resize(terminalID string, cols, rows int) error {
	s, ok := m.Get(terminalID)
	if !ok {
		return errTerminalNotFound
	}
	return applySize(s, cols, rows)
}

// Kill terminates a session's bash process group and closes its PTY.
func (m *TerminalManager) Kill(terminalID string) error {
	m.mu.Lock()
	s, ok := m.sessions[terminalID]
	m.mu.Unlock()
	if !ok {
		return errTerminalNotFound
	}
	_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
	_ = s.pty.Close()
	return nil
}

// boundedBuffer is a fixed-size circular byte buffer for terminal replay.
type boundedBuffer struct {
	mu    sync.Mutex
	buf   []byte
	start int // offset of the oldest byte
	len   int // bytes held
	cap   int
}

func newBoundedBuffer(size int) *boundedBuffer {
	return &boundedBuffer{buf: make([]byte, size), cap: size}
}

func (b *boundedBuffer) Write(p []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for len(p) > 0 {
		n := min(len(p), b.cap)
		if n == 0 {
			break
		}
		// Drop as many oldest bytes as necessary to fit p.
		over := b.len + n - b.cap
		if over > 0 {
			b.start = (b.start + over) % b.cap
			b.len -= over
		}
		for i := 0; i < n; i++ {
			b.buf[(b.start+b.len)%b.cap] = p[i]
			b.len++
		}
		p = p[n:]
	}
}

func (b *boundedBuffer) Snapshot() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]byte, 0, b.len)
	for i := 0; i < b.len; i++ {
		out = append(out, b.buf[(b.start+i)%b.cap])
	}
	return out
}
