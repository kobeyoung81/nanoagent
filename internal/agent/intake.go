package agent

import (
	"strings"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanosec/internal/session"
	"gitee.com/kobeyoung81/nanosec/internal/tool"
)

// intakeDraftMetadataKey is where the shell names the goal draft a turn fills.
//
// The intake turn is an ordinary conversation turn (the blocking question tool
// must work: somebody is looking at the screen), so it cannot carry `source:
// goal`. A metadata key instead binds the draft to the turn: the tools then
// write into it without the model having to carry an id it read in a message.
const intakeDraftMetadataKey = "goal_draft"

// intakeContextOf reads the draft binding a turn carries. The zero value means
// "not an intake turn", and every write refuses rather than guessing.
func intakeContextOf(msg *nanoprotocol.InboundMessage) tool.IntakeContext {
	if msg == nil || msg.Metadata == nil {
		return tool.IntakeContext{}
	}
	id, _ := msg.Metadata[intakeDraftMetadataKey].(string)
	id = strings.TrimSpace(id)
	if id == "" {
		return tool.IntakeContext{}
	}
	base, _ := session.ParseSessionKey(msg.SessionID)
	return tool.IntakeContext{DraftID: id, SessionKey: base}
}
