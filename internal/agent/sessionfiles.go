package agent

import (
	"strings"

	"gitee.com/kobeyoung81/nanosec/internal/provider"
	"gitee.com/kobeyoung81/nanosec/internal/session"
)

// Session files (AGENT.md, bootstrap, memory, skills) reach the model in two
// places, and the split is what makes the prompt cache work:
//
//   - a *frozen snapshot* per session segment, rendered as one system message per
//     file at the head. It never changes while the segment lives, so the cached
//     prefix stays valid for the whole conversation;
//   - a *tail update* appended (and persisted) when a file's content moved after
//     the snapshot. A row written once is part of the prefix for every later
//     call; an ephemeral tail would be re-paid on every call, because the
//     previous request's last message is not reusable.
//
// The comparison state is the transcript itself: every segment and every update
// carries a "[name][hash]" marker, and the newest marker per file is found by
// scanning the session's messages backwards. Nothing extra has to be stored.
//
// The files are read in exactly two places: when a segment is built (its id is
// new, so a fresh segment — including the one a compaction hands off to — always
// re-reads them) and at a compaction *inside* a segment, where the floor drops
// the tail updates the head still disagrees with (refreshHeadSnapshot). Every
// other file change is delivered as a tail update.

// headSnapshot is the frozen head of one session segment.
type headSnapshot struct {
	segments []HeadSegment
	messages []provider.Message
}

// headSnapshotFor returns the segment's frozen head, building it on first use.
func (a *Agent) headSnapshotFor(sess *session.Session) headSnapshot {
	if a.ctxBuilder == nil {
		return headSnapshot{}
	}
	a.headMu.Lock()
	defer a.headMu.Unlock()
	if a.heads == nil {
		a.heads = map[string]headSnapshot{}
	}
	if snap, ok := a.heads[sess.ID]; ok {
		return snap
	}
	snap := a.buildHeadSnapshot()
	a.heads[sess.ID] = snap
	return snap
}

// buildHeadSnapshot reads the session files and renders them as the head's
// per-file system messages.
func (a *Agent) buildHeadSnapshot() headSnapshot {
	segs := a.ctxBuilder.HeadSegments()
	msgs := make([]provider.Message, 0, len(segs))
	for _, s := range segs {
		msgs = append(msgs, provider.Message{
			Role:    "system",
			Content: HeadMarker(s.Name, s.Hash) + "\n" + s.Text,
		})
	}
	return headSnapshot{segments: segs, messages: msgs}
}

// refreshHeadSnapshot re-reads the session files into the segment's frozen head
// and reports which files moved. Compaction is the one moment the head is allowed
// to change mid-segment: the summary and the compaction floor have just
// invalidated the cached prefix anyway, and the tail update rows that carried a
// file's new content in earlier turns now sit *below* that floor — the head would
// still show the old text, so the file's newest content would be missing from the
// request altogether. (The next turn splits into a new segment and re-reads the
// files too, but that is a turn away; see splitSession.)
//
// Rebuilding is free when nothing moved: the bytes are identical, so the provider
// still sees the same cached prefix. That is also why the snapshot is only
// replaced when the diff is non-empty.
func (a *Agent) refreshHeadSnapshot(sess *session.Session) []string {
	if a.ctxBuilder == nil {
		return nil
	}
	old := a.headSnapshotFor(sess)
	oldHash := make(map[string]string, len(old.segments))
	for _, s := range old.segments {
		oldHash[s.Name] = s.Hash
	}

	fresh := a.buildHeadSnapshot()
	inFresh := make(map[string]bool, len(fresh.segments))
	var moved []string
	for _, s := range fresh.segments {
		inFresh[s.Name] = true
		if oldHash[s.Name] != s.Hash {
			moved = append(moved, s.Name)
		}
	}
	// A file can also leave the window (a daily note ages out), which changes the
	// head just as much as a moved one.
	for _, s := range old.segments {
		if !inFresh[s.Name] {
			moved = append(moved, s.Name+" (gone)")
		}
	}
	if len(moved) == 0 {
		return nil
	}

	a.headMu.Lock()
	if a.heads == nil {
		a.heads = map[string]headSnapshot{}
	}
	a.heads[sess.ID] = fresh
	a.headMu.Unlock()
	return moved
}

// forgetHeadSnapshot drops a segment's snapshot: the caller has archived it (see
// splitSession), so no later turn can use it and the cached copy of the session
// files would only keep growing. The replacement segment has an id of its own, so
// its head is read fresh from the files.
func (a *Agent) forgetHeadSnapshot(sessionID string) {
	a.headMu.Lock()
	delete(a.heads, sessionID)
	a.headMu.Unlock()
}

// lastMarkerHash scans messages backwards for the newest marker of one file and
// returns the hash it carries ("" when the file has no marker yet).
func lastMarkerHash(msgs []provider.Message, name string) string {
	prefix := "[" + name + "]["
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "system" {
			continue
		}
		idx := strings.Index(msgs[i].Content, prefix)
		if idx < 0 {
			continue
		}
		rest := msgs[i].Content[idx+len(prefix):]
		if end := strings.Index(rest, "]"); end > 0 {
			return rest[:end]
		}
	}
	return ""
}

// appendedSuffix returns the text a file gained since prev, when now is prev plus
// an append.
func appendedSuffix(prev, now string) (string, bool) {
	if prev == "" || !strings.HasPrefix(now, prev) {
		return "", false
	}
	return now[len(prev):], true
}

// fileUpdateMessages returns the tail injections due right now: one per file
// whose current content is not already in the head snapshot, and has not already
// been reported by the newest marker. They are appended to the session (and so
// persisted) before the turn's user message.
func (a *Agent) fileUpdateMessages(sess *session.Session, snap headSnapshot) []provider.Message {
	if a.ctxBuilder == nil {
		return nil
	}
	snapshotHash := make(map[string]string, len(snap.segments))
	snapshotText := make(map[string]string, len(snap.segments))
	for _, s := range snap.segments {
		snapshotHash[s.Name] = s.Hash
		snapshotText[s.Name] = s.Text
	}

	var out []provider.Message
	for _, seg := range a.ctxBuilder.HeadSegments() {
		// The head already carries this content: nothing to report.
		if hash, ok := snapshotHash[seg.Name]; ok && hash == seg.Hash {
			continue
		}
		// A previous turn already reported exactly this content.
		if lastMarkerHash(sess.Messages, seg.Name) == seg.Hash {
			continue
		}
		body, note := seg.Text, ""
		if prev, ok := snapshotText[seg.Name]; ok && seg.AppendOnly {
			if suffix, appended := appendedSuffix(prev, seg.Text); appended {
				body = suffix
			} else {
				note = "（以下为最新全文）"
			}
		} else if !seg.AppendOnly {
			note = "（以下为最新全文）"
		}
		out = append(out, provider.Message{
			Role:    "system",
			Content: "[更新" + seg.Name + "]" + HeadMarker(seg.Name, seg.Hash) + "\n" + note + body,
		})
	}
	return out
}

// modeNotePrefix marks the per-turn mode note. It is a constant because the
// wiring test looks for it in a real turn's transcript.
const modeNotePrefix = "【模式状态】"

// modeNoteBody renders the per-turn mode note. The text is configurable (the bus
// folds the platform's mode-switch prompt into the definition settings); the
// built-in line is the fallback. It is appended on *every* turn: measured, a note
// left behind by a later user turn stops steering the model back to the mode.
func (a *Agent) modeNoteBody(sess *session.Session, spec ResolvedAgent) string {
	tmpl := strings.TrimSpace(spec.Settings["mode_note"])
	planPath := sess.GetMeta(lastPlanPathMetaKey)
	if tmpl == "" {
		if planPath == "" {
			return modeNotePrefix + "Mode: " + spec.ID + "."
		}
		return modeNotePrefix + "Mode: " + spec.ID + ". A plan artefact produced in plan mode is at " +
			planPath + " — read it before starting work and follow it unless the user says otherwise."
	}
	body := strings.ReplaceAll(tmpl, "{mode}", spec.ID)
	if strings.Contains(body, "{tools}") {
		body = strings.ReplaceAll(body, "{tools}", a.modeToolList(spec))
	}
	if planPath != "" {
		body = strings.ReplaceAll(body, "{plan_path}", planPath)
	} else {
		// No plan artefact yet: drop the lines that talk about one instead of
		// leaving a placeholder in the prompt.
		var kept []string
		for _, line := range strings.Split(body, "\n") {
			if strings.Contains(line, "{plan_path}") {
				continue
			}
			kept = append(kept, line)
		}
		body = strings.Join(kept, "\n")
	}
	return modeNotePrefix + strings.TrimSpace(body)
}

// modeToolList renders the tools the active mode may call and the ones it will
// refuse, for the {tools} placeholder in the mode-switch prompt. The list comes
// from the same two judgements the executor makes (definition gating and
// permission rules), which is what keeps the prompt from promising a tool the
// mode would reject.
func (a *Agent) modeToolList(spec ResolvedAgent) string {
	allowed, denied := a.modeExecutableTools(spec)
	if len(allowed) == 0 && len(denied) == 0 {
		return "(none)"
	}
	out := strings.Join(allowed, ", ")
	if len(denied) > 0 {
		out += " (refused in this mode: " + strings.Join(denied, ", ") + ")"
	}
	return out
}

// injectSessionState appends this turn's tail injections to the session: the file
// updates first, then the mode note, both as ordinary persisted system rows. They
// therefore sit at the end of this turn's request — after the user message, which
// is added before this runs — and become part of the cached prefix for every later
// turn. (A compaction inside a turn re-reads the files into the head instead, and
// its floor drops the earlier updates: see refreshHeadSnapshot.)
func (a *Agent) injectSessionState(sess *session.Session, spec ResolvedAgent) {
	snap := a.headSnapshotFor(sess)
	for _, m := range a.fileUpdateMessages(sess, snap) {
		sess.AddMessage(m)
	}
	if note := a.modeNoteBody(sess, spec); note != "" {
		// The language note rides along with the mode note: it has to be the last
		// system message before the question, and a second row would only repeat
		// the same position.
		if line := languageNote(a.config.Language); line != "" {
			note += "\n" + line
		}
		sess.AddMessage(provider.Message{Role: "system", Content: note})
	}
}
