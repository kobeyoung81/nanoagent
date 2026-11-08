package cron

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"gitee.com/kobeyoung81/nanoprotocol"
)

type fakePoster struct {
	msgs []nanoprotocol.InboundMessage
	err  error
}

func (f *fakePoster) PostInbound(msg nanoprotocol.InboundMessage) error {
	f.msgs = append(f.msgs, msg)
	return f.err
}

func taskJob() *Job {
	return &Job{
		ID:   "abcd1234",
		Name: "server patrol",
		Payload: Payload{
			Message: "check disk usage",
			Channel: "web",
			ChatID:  TaskChatPrefix + "00112233",
		},
	}
}

// The session id must be the task's base key: a version suffix here would pin
// every run to the segment that existed when the task was created, so a task
// whose conversation was compacted would keep writing to an archived segment.
func TestInboundFor_UsesBaseKeyWithoutVersion(t *testing.T) {
	now := time.Unix(1700000000, 0)
	msg := InboundFor("user-1", "agent-1", "build", taskJob(), now)

	if got, want := msg.SessionID, "web:"+TaskChatPrefix+"00112233"; got != want {
		t.Errorf("SessionID = %q, want %q", got, want)
	}
	if strings.Contains(msg.SessionID, ":v") {
		t.Errorf("SessionID %q carries a version suffix", msg.SessionID)
	}
}

// Every agent of one user shares a container: an injected message without an
// agent id can be claimed by a sibling agent's poll loop.
func TestInboundFor_AddressesTheOwningAgent(t *testing.T) {
	msg := InboundFor("user-1", "agent-1", "build", taskJob(), time.Now())

	if msg.AgentID != "agent-1" {
		t.Errorf("AgentID = %q, want agent-1", msg.AgentID)
	}
	if msg.UserID != "user-1" {
		t.Errorf("UserID = %q, want user-1", msg.UserID)
	}
	if msg.Channel != "web" || msg.ChatID != TaskChatPrefix+"00112233" {
		t.Errorf("channel/chat = %q/%q", msg.Channel, msg.ChatID)
	}
	if msg.SenderID != "cron" {
		t.Errorf("SenderID = %q, want cron", msg.SenderID)
	}
	if msg.MessageID == "" {
		t.Error("MessageID must be set")
	}
}

// The turn must know it is unattended (tools refuse to ask) and which task it
// belongs to (the conversation is named after it).
func TestInboundFor_CarriesCronMetadataAndFraming(t *testing.T) {
	msg := InboundFor("user-1", "agent-1", "build", taskJob(), time.Now())

	if got := msg.Metadata["source"]; got != "cron" {
		t.Errorf("metadata source = %v, want cron", got)
	}
	if got := msg.Metadata["cron_job"]; got != "abcd1234" {
		t.Errorf("metadata cron_job = %v, want abcd1234", got)
	}
	if got := msg.Metadata["cron_name"]; got != "server patrol" {
		t.Errorf("metadata cron_name = %v, want server patrol", got)
	}
	if !strings.Contains(msg.Content, "check disk usage") {
		t.Errorf("content lost the instruction: %q", msg.Content)
	}
	if !strings.Contains(msg.Content, "server patrol") || !strings.Contains(msg.Content, "do not ask questions") {
		t.Errorf("content is missing the unattended framing: %q", msg.Content)
	}
}

func TestNewTaskChatID_IsPrefixedAndUnique(t *testing.T) {
	a, b := NewTaskChatID(), NewTaskChatID()

	if !strings.HasPrefix(a, TaskChatPrefix) || !strings.HasPrefix(b, TaskChatPrefix) {
		t.Errorf("chat ids must start with %q: %q %q", TaskChatPrefix, a, b)
	}
	if a == b {
		t.Errorf("chat ids must differ, both were %q", a)
	}
}

// The callback must inject, not broadcast: a fired job starts a turn by putting
// a message on the inbound queue.
func TestInjector_PostsInbound(t *testing.T) {
	poster := &fakePoster{}
	job := taskJob()

	Injector(poster, "user-1", "agent-1", func(*Job) string { return "build" })(context.Background(), job)

	if len(poster.msgs) != 1 {
		t.Fatalf("expected 1 inbound message, got %d", len(poster.msgs))
	}
	if poster.msgs[0].SessionID != "web:"+TaskChatPrefix+"00112233" {
		t.Errorf("injected session = %q", poster.msgs[0].SessionID)
	}
}

// The injected message must say which mode the turn runs in: without it the
// agent falls back to its default session agent, which for a conversation-
// oriented agent is plan — and a task that only produces plans cannot run.
func TestInboundFor_CarriesTheResolvedMode(t *testing.T) {
	msg := InboundFor("user-1", "agent-1", "build", taskJob(), time.Now())

	if got := msg.Metadata["agent_mode"]; got != "build" {
		t.Errorf("metadata agent_mode = %v, want build", got)
	}
}

// Resolution runs most-specific first, and an unusable value at any level falls
// through instead of leaving the task unable to run.
func TestTaskMode_ResolvesMostSpecificValidValue(t *testing.T) {
	job := func(mode string) *Job {
		j := taskJob()
		j.Payload.Mode = mode
		return j
	}

	cases := []struct {
		name            string
		jobMode         string
		agentSetting    string
		platformDefault string
		want            string
	}{
		{"task wins", "plan", "build", "build", "plan"},
		{"agent setting wins over platform", "", "plan", "build", "plan"},
		{"platform default used", "", "", "plan", "plan"},
		{"nothing configured falls back to build", "", "", "", "build"},
		{"invalid task mode falls through", "turbo", "plan", "build", "plan"},
		{"invalid agent setting falls through", "", "turbo", "plan", "plan"},
		{"invalid everywhere falls back to build", "turbo", "nope", "", "build"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := TaskMode(job(tc.jobMode), tc.agentSetting, tc.platformDefault); got != tc.want {
				t.Fatalf("TaskMode = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestInjector_UsesTheResolvedMode(t *testing.T) {
	poster := &fakePoster{}
	job := taskJob()

	Injector(poster, "user-1", "agent-1", func(*Job) string { return "plan" })(context.Background(), job)

	if len(poster.msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(poster.msgs))
	}
	if got := poster.msgs[0].Metadata["agent_mode"]; got != "plan" {
		t.Errorf("metadata agent_mode = %v, want plan", got)
	}
}

func TestInjector_SurvivesPostFailure(t *testing.T) {
	poster := &fakePoster{err: errors.New("bus down")}

	Injector(poster, "user-1", "agent-1", func(*Job) string { return "build" })(context.Background(), taskJob())

	if len(poster.msgs) != 1 {
		t.Errorf("the attempt should still have been made, got %d", len(poster.msgs))
	}
}
