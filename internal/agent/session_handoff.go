package agent

import (
	"context"
	"log/slog"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanosec/internal/session"
)

// minHandoffMessages is the smallest conversation (in LLM-facing messages) for
// which a split bothers writing a fresh summary.
const minHandoffMessages = 4

// splitReason records why a session segment was closed.
type splitReason int

const (
	// splitOnCompaction closes a segment because its context was compacted: the
	// summary is the only state that crosses, so the new segment starts from the
	// basic prompts plus that summary.
	splitOnCompaction splitReason = iota
	// splitOnReset is an explicit /new. The old segment stays readable (and
	// scrollable through the version chain) but no context crosses.
	splitOnReset
)

// splitSession archives the current session and starts a new versioned segment,
// reporting the switch so the client can follow the new id.
//
// Compaction is the only automatic boundary: the count/turn cap that used to
// split sessions is gone, because it fired on rows rather than on context
// pressure. Carrying the summary across is what keeps a split from acting like
// a fresh conversation — without it the new segment started from nothing, which
// is the "build agent forgot the plan the plan agent just produced" failure.
func (a *Agent) splitSession(ctx context.Context, sess *session.Session, baseKey string, msg *nanoprotocol.InboundMessage, reason splitReason) *session.Session {
	slog.Info("session lifecycle: archiving current session",
		"session", sess.ID, "reason", reason,
		"messages", sess.MessageCount(), "conversation", sess.ConversationCount())
	summary := ""
	if reason == splitOnCompaction {
		if a.summarizer != nil && sess.GetSummary() == "" && sess.ConversationCount() >= minHandoffMessages {
			a.summarizer.summarizeUpTo(ctx, sess, len(sess.ConversationMessages()))
		}
		summary = sess.GetSummary()
	}
	if err := a.sessions.ArchiveSession(sess.ID); err != nil {
		slog.Warn("failed to archive session", "session", sess.ID, "error", err)
	}
	// The archived segment can never serve another turn (resolveActiveSession
	// redirects its id forward), so drop its head snapshot. The new segment gets
	// its own — built from the files as they are now, which is what makes a
	// compaction boundary re-read AGENT.md and MEMORY.md.
	a.forgetHeadSnapshot(sess.ID)
	prevID := sess.ID
	newSess := a.sessions.NewSession(baseKey)
	// The title travels with the conversation. The portal groups segments by
	// chat id and shows the newest row, so without this the entry would be
	// renamed from the new segment's first message at every compaction.
	for _, key := range []string{titleMetaKey, titleCountMetaKey, titleProvisionalKey} {
		if v := sess.GetMeta(key); v != "" {
			newSess.SetMeta(key, v)
		}
	}
	if summary != "" {
		// GetSummary drives the "[Previous conversation summary]" context
		// injection; the meta key is the explicit handoff marker clients read.
		newSess.SetSummary(summary)
		newSess.SetMeta(handoffSummaryMetaKey, summary)
	}
	newSess.SetMeta(handoffFromMetaKey, prevID)
	// A compaction handoff is still the same conversation, so the model keeps its
	// bearings (active mode + the plan artefact it was pointed at). /new drops
	// this deliberately: it asks for a clean context.
	if reason == splitOnCompaction {
		for _, key := range []string{sessionAgentMetaKey, sessionModeNoteMetaKey, lastPlanPathMetaKey} {
			if v := sess.GetMeta(key); v != "" {
				newSess.SetMeta(key, v)
			}
		}
	}
	msg.SessionID = newSess.ID
	// Refresh the status target and post the switch to THIS message's chat: the
	// notice belongs to the conversation that was split, and the shared
	// lastMessage can hold another one by now.
	a.rememberMessage(msg)
	state := "applied"
	if reason == splitOnReset {
		state = "reset"
	}
	a.emitStatusTo(messageRefOf(msg), "session_switch", state)
	slog.Info("session lifecycle: started new session",
		"session", newSess.ID, "from", prevID, "handoff", summary != "")
	return newSess
}

// resolveActiveSession maps an inbound message's session id onto the live
// segment of its conversation. A client holding an archived id — a stale tab, a
// retry that raced a compaction — is moved forward along the version chain, so
// a turn can never append to an archived transcript (the portal collapses those
// into the previous version, so the writes would be invisible).
func (a *Agent) resolveActiveSession(msg *nanoprotocol.InboundMessage) *session.Session {
	sess := a.sessions.GetOrCreate(msg.SessionID)
	if sess.GetMeta("_archived") != "true" {
		return sess
	}
	baseKey, _ := session.ParseSessionKey(sess.ID)
	active := a.sessions.ActiveVersion(baseKey)
	if active.ID == sess.ID {
		return sess
	}
	slog.Info("session lifecycle: resumed segment is archived, redirecting",
		"from", sess.ID, "to", active.ID)
	msg.SessionID = active.ID
	a.rememberMessage(msg)
	a.emitStatusTo(messageRefOf(msg), "session_switch", "redirected")
	return active
}

// noteSessionAgent records the session agent used for this turn and refreshes
// the ephemeral mode note when it changes. The note is injected into the LLM
// context (never persisted to the transcript), so a plan -> build switch
// re-anchors the model on the active mode and on the plan artefact the previous
// mode produced.
func (a *Agent) noteSessionAgent(sess *session.Session, spec ResolvedAgent) {
	if spec.ID == "" || sess.GetMeta(sessionAgentMetaKey) == spec.ID {
		return
	}
	sess.SetMeta(sessionAgentMetaKey, spec.ID)
	note := "Mode: " + spec.ID + "."
	if plan := sess.GetMeta(lastPlanPathMetaKey); plan != "" {
		note = "Mode: " + spec.ID + ". A plan artefact produced in plan mode is at " +
			plan + " — read it before starting work and follow it unless the user says otherwise."
	}
	sess.SetMeta(sessionModeNoteMetaKey, note)
}
