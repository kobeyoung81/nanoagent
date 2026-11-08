package hypervisor

import (
	"bytes"
	"strings"
	"testing"
)

func TestModeTrackerBasics(t *testing.T) {
	tr := newModeTracker()
	// opencode-style arming: alt screen + mouse modes + bracketed paste.
	tr.Scan([]byte("\x1b[?1049h"))
	tr.Scan([]byte("\x1b[?1002h\x1b[?1006h"))
	tr.Scan([]byte("\x1b[?2004h")) // bracketed paste
	if !tr.inAltLocked() {
		t.Fatal("expected alt screen active")
	}
	prefix := tr.restorePrefix()
	// 1049 must be first; the whitelist then emits 1002;1006;2004 in order.
	want := "\x1b[?1049h\x1b[?1002;1006;2004h"
	if string(prefix) != want {
		t.Fatalf("restorePrefix = %q, want %q", prefix, want)
	}

	// Teardown flips modes again.
	tr.Scan([]byte("\x1b[?1049l\x1b[?1002l\x1b[?1006l\x1b[?2004l"))
	if tr.inAltLocked() {
		t.Fatal("expected alt screen off after 1049l")
	}
	if got := tr.restorePrefix(); len(got) != 0 {
		t.Fatalf("expected empty prefix after teardown, got %q", got)
	}
}

func TestModeTrackerSplitSequences(t *testing.T) {
	tr := newModeTracker()
	// A CSI split across arbitrary chunk boundaries must still be tracked.
	for _, c := range []string{
		"\x1b[?10", "49h\x1b[?", "1003", "h",
	} {
		tr.Scan([]byte(c))
	}
	if !tr.inAltLocked() {
		t.Fatal("split 1049h not detected")
	}
	if !tr.set[1003] {
		t.Fatal("split 1003h not detected")
	}
}

func TestModeTrackerOSCPayloadIgnored(t *testing.T) {
	tr := newModeTracker()
	// OSC 52 clipboard payload contains "ESC[?..."-shaped text that must NOT
	// change mode state; BEL closes the OSC.
	tr.Scan([]byte("\x1b]52;c;aGVsbG8gZXNjWz8xMDA1aA==\x07"))
	// A real mode set after the OSC still applies.
	tr.Scan([]byte("\x1b[?2004h"))
	if !tr.set[2004] {
		t.Fatal("mode after OSC not tracked")
	}
	if tr.set[1005] {
		t.Fatal("OSC payload text was misread as a mode set")
	}

	// ST (ESC \) terminates an OSC the same way and must not leak.
	tr2 := newModeTracker()
	tr2.Scan([]byte("\x1b]0;terminals title\x1b\\\x1b[?25l"))
	if tr2.set[25] {
		t.Fatal("ST-terminated OSC leaked a mode")
	}
	if tr2.inAltLocked() {
		t.Fatal("ST-terminated OSC leaked alt screen")
	}
}

func TestModeTrackerMasqueradeAndExclusions(t *testing.T) {
	tr := newModeTracker()
	// Intermediate frames (DECSCUSR "CSI 2 SP q", DECSTBM "CSI 3;$r") must not
	// set/clear private modes even though they contain digits.
	tr.Scan([]byte("\x1b[2 q"))
	tr.Scan([]byte("\x1b[3;$r"))
	if len(tr.set) != 0 {
		t.Fatalf("masquerading CSI mutated modes: %v", tr.set)
	}

	// 1047h counts as alt; 1047l clears it.
	tr.Scan([]byte("\x1b[?1047h"))
	if !tr.inAltLocked() {
		t.Fatal("1047h should count as alt")
	}
	tr.Scan([]byte("\x1b[?1047l"))
	if tr.inAltLocked() {
		t.Fatal("1047l should clear alt")
	}

	// Origin mode (6) is tracked but never re-emitted; 1000 is.
	tr.Scan([]byte("\x1b[?6h\x1b[?1000h"))
	prefix := string(tr.restorePrefix())
	if want := "\x1b[?1000h"; prefix != want {
		t.Fatalf("prefix = %q, want %q", prefix, want)
	}
}

func TestModeTrackerEmpty(t *testing.T) {
	tr := newModeTracker()
	// Plain shell output must produce no prefix.
	tr.Scan([]byte("echo hello\r\nkobeyoung81@nanosec:~$ "))
	if got := tr.restorePrefix(); len(got) != 0 {
		t.Fatalf("plain output produced prefix %q", got)
	}
}

func TestRestorePrefixOrdering(t *testing.T) {
	tr := newModeTracker()
	tr.Scan([]byte("\x1b[?1000h\x1b[?25h\x1b[?1049h"))
	prefix := tr.restorePrefix()
	// alt first, then whitelist modes in a fixed order (1000 before 25).
	if !bytes.HasPrefix(prefix, []byte("\x1b[?1049h\x1b[?1000")) {
		t.Fatalf("bad prefix ordering: %q", prefix)
	}
	last := strings.LastIndex(string(prefix), ";25h")
	if pos := bytes.Index(prefix, []byte("1000")); last < pos {
		t.Fatalf("25 must come after 1000 in prefix %q", prefix)
	}
}

func TestModeTrackerConcurrent(t *testing.T) {
	tr := newModeTracker()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 10000; i++ {
			tr.Scan([]byte("\x1b[?1049h\x1b[?1002h"))
		}
	}()
	for i := 0; i < 10000; i++ {
		_ = tr.restorePrefix()
	}
	<-done
}
