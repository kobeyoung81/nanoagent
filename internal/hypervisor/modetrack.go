// DEC private-mode tracking for terminal replay.
//
// A terminal session's audio/visual output streams through the PTY pump; the
// ring buffer keeps a bounded tail for re-attach replay. When a TUI (opencode,
// vim, htop...) is launched, the bytes that arm its alternate screen and mouse
// tracking (CSI ? 1049 h, CSI ? 1003 h, ...) live inside the ring. A browser
// reconnect or page reload replays that tail into a FRESH xterm.js instance
// sitting in the normal buffer — if the arming sequence scrolled out of the
// ring, the replayed frames paint into the wrong buffer and the app's later
// mode resets become no-ops (stale TUI frame stuck on exit, dead mouse,
// bracketed-paste misbehavior).
//
// modeTracker incrementally scans the output stream so we always know the
// terminal's current DEC private-mode state at the moment a replay snapshot is
// taken, and can emit a restore prefix that re-arms the new terminal before the
// ring bytes are replayed.
package hypervisor

import (
	"strconv"
	"strings"
	"sync"
)

// vtState is a minimal incremental scan state for the sequences we care about.
type vtState int

const (
	vtGround vtState = iota // normal text (bytes ignored)
	vtEsc                   // saw ESC, expect the next control char
	vtCsi                   // inside CSI parameters (ESC [ <params> <final>)
	vtString                // inside OSC/DCS-string payload (until BEL or ST)
)

// emitModes is the whitelist of private modes a replay prefix may re-enable.
//
// Modes that are dangerous or meaningless to re-emit are deliberately excluded:
//   - 1047/1048/1049 have cursor-save / buffer-switch semantics and are handled
//     specially (the alternate-screen bit becomes a single ESC[?1049h).
//   - 3 (DECCOLM) resizes/clears the screen and is not safe to re-apply.
//   - 6 (DECOM origin mode) and the numerics like 38/42/67 (Tektronix) and the
//     cursor/region modes 69/104-107... are left for the replayed app to
//     re-establish from the buffer state it replays.
var emitModes = []uint{1000, 1002, 1003, 1004, 1005, 1006, 1015, 1016, 12, 25, 2004}

// modeTracker repurposes a small state machine over every PTY output byte. It
// keeps no buffered payload — the state persists across Scan calls, so a CSI
// or OSC sequence split across PTY reads is handled without copying.
type modeTracker struct {
	mu      sync.Mutex // guards set and the scan state across the pump goroutine
	state   vtState
	private bool          // current CSI parameters started with '?'
	hasInt  bool          // current CSI contained intermediate bytes (=/space/$...)
	params  []uint        // numeric parameters of the current CSI
	cur     uint          // accumulator for the parameter being parsed
	set     map[uint]bool // currently-enabled private modes
}

func newModeTracker() *modeTracker {
	return &modeTracker{state: vtGround, set: make(map[uint]bool)}
}

// Scan advances the tracker over one chunk of PTY output. It is cheap enough
// to run synchronously on the hot output path (one lock per chunk).
func (t *modeTracker) Scan(p []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, b := range p {
		t.byte(b)
	}
}

func (t *modeTracker) byte(b byte) {
	switch t.state {
	case vtGround:
		if b == '\x1b' {
			t.state = vtEsc
		}
	case vtEsc:
		switch b {
		case '[':
			t.beginCSI()
		case ']', '^', '_', 'P', 'X': // OSC/SOS/PM/APC/DCS string payloads
			t.state = vtString
		default:
			// Other two-char sequences (ESC 7/8/M/c/>, charset ESC ( B...)
			// are not tracked.
			t.state = vtGround
		}
	case vtCsi:
		switch {
		case b == '?':
			t.private = true
		case b == ';':
			t.pushParam()
		case b >= '0' && b <= '9':
			t.cur = t.cur*10 + uint(b-'0')
		case b >= 0x20 && b <= 0x2f:
			// Intermediate bytes (space, $, ", !...) — e.g. DECSCUSR
			// "CSI 2 SP q", DECSTBM "$r". Never a private-mode set/reset.
			t.hasInt = true
		case b >= 0x40 && b <= 0x7e:
			// Final byte. Only pure private-mode set/reset frames count; the
			// accumulated parameter is pushed first (a bare "CSI ? h" then
			// yields a no-op parameter 0).
			t.pushParam()
			t.finish(b)
			t.state = vtGround
		default:
			// Subparameter ':' and anything else make the frame opaque.
			t.hasInt = true
		}
	case vtString:
		switch b {
		case '\a': // BEL terminates OSC
			t.state = vtGround
		case '\x1b': // possible ST (ESC \) - handled by the esc branch below
			t.state = vtEsc // note: if this is ST, '\' will fold back to ground
		}
	}
}

func (t *modeTracker) beginCSI() {
	t.state = vtCsi
	t.private = false
	t.hasInt = false
	t.params = t.params[:0]
	t.cur = 0
}

func (t *modeTracker) pushParam() {
	t.params = append(t.params, t.cur)
	t.cur = 0
}

// finish applies a completed private-mode frame (CSI ? Ps;... h/l).
func (t *modeTracker) finish(final byte) {
	if !t.private || t.hasInt {
		return
	}
	if t.set == nil {
		t.set = make(map[uint]bool)
	}
	enabled := final == 'h'
	vals := t.params
	if len(vals) == 0 {
		vals = []uint{0}
	}
	for _, m := range vals {
		if m == 0 {
			continue // parameter 0 is a no-op in DEC private modes
		}
		if enabled {
			t.set[m] = true
		} else {
			delete(t.set, m)
		}
	}
}

// inAlt reports whether the app is currently on the alternate screen. Caller
// must hold t.mu.
func (t *modeTracker) inAltLocked() bool {
	return t.set[1047] || t.set[1049]
}

// restorePrefix returns the byte string that re-arms a fresh terminal to the
// mode state the replay is about to draw, or nil when nothing is set. The
// alternate screen is entered first (it clears, so the replayed frames repaint
// it); the remaining whitelisted modes follow in a single CSI frame.
func (t *modeTracker) restorePrefix() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.restorePrefixLocked()
}

func (t *modeTracker) restorePrefixLocked() []byte {
	var out []byte
	if t.inAltLocked() {
		out = append(out, "\x1b[?1049h"...)
	}
	var set []uint
	for _, m := range emitModes {
		if t.set[m] {
			set = append(set, m)
		}
	}
	if len(set) == 0 {
		return out
	}
	out = append(out, "\x1b[?"...)
	var sb strings.Builder
	for i, m := range set {
		if i > 0 {
			sb.WriteByte(';')
		}
		sb.WriteString(strconv.FormatUint(uint64(m), 10))
	}
	out = append(out, sb.String()...)
	out = append(out, 'h')
	return out
}
