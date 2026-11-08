package cron

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanosec/internal/messageid"

	"github.com/google/uuid"
)

// TaskChatPrefix marks the chat ID of a task's own conversation. A task's
// session base key is "<channel>:<chatID>", so every run of one task appends to
// the same conversation instead of mixing into the conversation that created
// the task.
const TaskChatPrefix = "task-"

// NewTaskChatID mints the chat ID naming one task's conversation.
func NewTaskChatID() string {
	return TaskChatPrefix + uuid.New().String()[:8]
}

// TaskModeNames are the agent modes a task may run in.
var TaskModeNames = map[string]bool{"build": true, "plan": true}

// TaskMode resolves the mode a task's turn runs in, most specific first:
// the task's own mode, then the agent's setting, then the platform default.
// An unusable value at any level falls through to the next one, and the last
// resort is build — a task nobody configured must still be able to act.
func TaskMode(job *Job, agentSetting, platformDefault string) string {
	if job != nil && TaskModeNames[job.Payload.Mode] {
		return job.Payload.Mode
	}
	if TaskModeNames[agentSetting] {
		return agentSetting
	}
	if TaskModeNames[platformDefault] {
		return platformDefault
	}
	return "build"
}

// TaskFraming tells the model that this turn was started by a scheduler rather
// than by a person: nothing is waiting for an answer, so it must execute and
// report instead of asking a question back.
func TaskFraming(name string) string {
	return fmt.Sprintf(
		"(Sent automatically by the scheduled task %q. No user is waiting for this run: "+
			"execute the task now, report the result in this conversation, and do not ask questions.)",
		name,
	)
}

// InboundFor maps a fired job onto the inbound message that starts its turn.
//
// The session id is the task's base key (no ":vN" suffix): the version is
// resolved against the live segment when the agent picks the message up, so a
// task that has been running for a while keeps appending to its own
// conversation. AgentID must be set — every agent of one user shares a
// container, and an unaddressed queued message could be claimed by a sibling.
func InboundFor(userID, agentID, mode string, job *Job, now time.Time) nanoprotocol.InboundMessage {
	return nanoprotocol.InboundMessage{
		UserID:    userID,
		AgentID:   agentID,
		SessionID: job.Payload.Channel + ":" + job.Payload.ChatID,
		Channel:   job.Payload.Channel,
		ChatID:    job.Payload.ChatID,
		SenderID:  "cron",
		MessageID: messageid.New(),
		Content:   job.Payload.Message + "\n\n" + TaskFraming(job.Name),
		Metadata: map[string]any{
			"source":    "cron",
			"cron_job":  job.ID,
			"cron_name": job.Name,
			// The mode travels with the message: resolveSession reads it before
			// falling back to the agent's default mode, which is what keeps a
			// task from inheriting a conversation-oriented default (e.g. plan).
			"agent_mode": mode,
		},
		Timestamp: now,
	}
}

// InboundPoster is the slice of the bus client the injector needs.
type InboundPoster interface {
	PostInbound(msg nanoprotocol.InboundMessage) error
}

// Injector builds the scheduler callback for one agent. Firing a job puts a
// message on the agent's inbound queue, which is the only path that starts a
// turn (agent poll loop → ClaimInbound); the scheduler never talks to the
// channel itself.
func Injector(client InboundPoster, userID, agentID string, resolveMode func(*Job) string) JobCallback {
	return func(ctx context.Context, job *Job) {
		_ = ctx
		msg := InboundFor(userID, agentID, resolveMode(job), job, time.Now())
		if err := client.PostInbound(msg); err != nil {
			slog.Error("cron: failed to inject task message", "job", job.ID, "error", err)
			return
		}
		slog.Info("cron: task message queued",
			"job", job.ID, "session", msg.SessionID, "agent", agentID)
	}
}
