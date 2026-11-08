package agent

import (
	"context"
	"strings"
	"testing"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanosec/internal/provider"
	"gitee.com/kobeyoung81/nanosec/internal/question"
	"gitee.com/kobeyoung81/nanosec/internal/session"
	"gitee.com/kobeyoung81/nanosec/internal/tool"
)

// scheduledMessage mirrors what the scheduler injects (cron.InboundFor): the
// task's base key as the session id, no user behind it, and the task name in
// metadata.
func scheduledMessage(sessionID, content string) *nanoprotocol.InboundMessage {
	return &nanoprotocol.InboundMessage{
		UserID:    "u",
		AgentID:   "agent-1",
		SessionID: sessionID,
		Channel:   "web",
		ChatID:    "task-1",
		SenderID:  "cron",
		Content:   content,
		Metadata: map[string]any{
			"source":     "cron",
			"cron_job":   "job-1",
			"cron_name":  "server patrol",
			"agent_mode": "build",
		},
	}
}

func newScheduledAgent(t *testing.T, tmpDir string, reg *tool.Registry, resp func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error)) (*Agent, *session.Store) {
	t.Helper()
	sessStore, err := session.NewStore(tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	mockProvider := provider.NewMock()
	mockProvider.ResponseFunc = resp
	ag, err := New(Config{UserID: "u", MaxIterations: 5, SessionDir: tmpDir}, &MockBusClient{}, mockProvider, reg)
	if err != nil {
		t.Fatal(err)
	}
	ag.sessions = sessStore
	return ag, sessStore
}

// A queue message addressed by base key (the task's conversation) must land in
// that conversation and nothing else: runs accumulate there instead of being
// appended to the conversation the task was requested from.
func TestScheduledRun_OpensItsOwnConversation(t *testing.T) {
	reg := tool.NewRegistry()
	ag, store := newScheduledAgent(t, t.TempDir(), reg, func(context.Context, provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "checked"}, nil
	})

	if err := ag.processMessage(context.Background(), scheduledMessage("web:task-1", "check the disks")); err != nil {
		t.Fatalf("processMessage: %v", err)
	}

	sess := store.GetOrCreate("web:task-1")
	if sess.MessageCount() == 0 {
		t.Fatal("the scheduled message did not land in the task's conversation")
	}
	if got := sess.GetMeta("_title"); got != "server patrol" {
		t.Errorf("task conversation title = %q, want the task name", got)
	}
	if other := store.GetOrCreate("web:user-chat"); other.MessageCount() != 0 {
		t.Error("the run leaked into another conversation")
	}
}

// Once the task's conversation has been compacted, the archived segment is no
// longer where runs belong: the base key has to resolve onto the live segment.
func TestScheduledRun_FollowsTheLiveSegment(t *testing.T) {
	reg := tool.NewRegistry()
	ag, store := newScheduledAgent(t, t.TempDir(), reg, func(context.Context, provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "ok"}, nil
	})

	store.GetOrCreate("web:task-1")
	if err := store.ArchiveSession("web:task-1"); err != nil {
		t.Fatal(err)
	}
	live := store.NewSession("web:task-1")

	msg := scheduledMessage("web:task-1", "check the disks")
	sess := ag.resolveActiveSession(msg)

	if sess.ID != live.ID {
		t.Errorf("resolved session = %q, want the live segment %q", sess.ID, live.ID)
	}
	if msg.SessionID != live.ID {
		t.Errorf("message session = %q, want %q", msg.SessionID, live.ID)
	}
}

// modeAgent builds an agent whose own default session mode is plan, i.e. the
// configuration that made tasks unable to act before the mode travelled with
// the message.
func modeAgent(t *testing.T, tmpDir string) (*Agent, *session.Store) {
	t.Helper()
	sessStore, err := session.NewStore(tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	mockProvider := provider.NewMock()
	mockProvider.ResponseFunc = func(context.Context, provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "did it"}, nil
	}
	ag, err := New(Config{
		UserID: "u", MaxIterations: 5, SessionDir: tmpDir,
		Agents: map[string]ResolvedAgent{
			"build": {ID: "build", Output: nanoprotocol.OutputStream, SessionBinding: nanoprotocol.SessionBindingShared, Strategy: nanoprotocol.StrategyReact},
			"plan":  {ID: "plan", Output: nanoprotocol.OutputStream, SessionBinding: nanoprotocol.SessionBindingShared, Strategy: nanoprotocol.StrategyReact},
		},
		SessionAgentIDs: []string{"plan", "build"},
		DefaultAgent:    "plan",
	}, &MockBusClient{}, mockProvider, tool.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	ag.sessions = sessStore
	return ag, sessStore
}

// The mode a scheduled run executes in comes from the message, so an agent
// whose own default is plan still runs its tasks in build.
func TestScheduledRun_UsesTheModeFromTheMessage(t *testing.T) {
	ag, store := modeAgent(t, t.TempDir())

	if err := ag.processMessage(context.Background(), scheduledMessage("web:task-1", "check the disks")); err != nil {
		t.Fatalf("processMessage: %v", err)
	}

	if got := store.GetOrCreate("web:task-1").GetMeta(sessionAgentMetaKey); got != "build" {
		t.Errorf("the run executed as %q, want build", got)
	}
}

// An ordinary conversation still follows the agent's own default mode: the
// message-level mode only exists because a task sets it.
func TestOrdinaryMessageStillUsesTheAgentsDefaultMode(t *testing.T) {
	ag, store := modeAgent(t, t.TempDir())

	msg := &nanoprotocol.InboundMessage{UserID: "u", SessionID: "web:chat", Channel: "web", ChatID: "chat", Content: "hello"}
	if err := ag.processMessage(context.Background(), msg); err != nil {
		t.Fatalf("processMessage: %v", err)
	}

	if got := store.GetOrCreate("web:chat").GetMeta(sessionAgentMetaKey); got != "plan" {
		t.Errorf("the conversation executed as %q, want the agent default plan", got)
	}
}

// A scheduled run has nobody to answer a question; the tool must refuse instead
// of blocking the turn (and the conversation's lock) forever.
func TestScheduledRun_QuestionToolRefuses(t *testing.T) {
	reg := tool.NewRegistry()
	reg.Register(tool.NewQuestionTool(question.NewBroker()))

	step := 0
	ag, store := newScheduledAgent(t, t.TempDir(), reg, func(context.Context, provider.ChatRequest) (*provider.ChatResponse, error) {
		step++
		if step == 1 {
			return &provider.ChatResponse{ToolCalls: []provider.ToolCall{{
				ID:        "call-1",
				Name:      "question",
				Arguments: `{"questions":[{"question":"which disk?"}]}`,
			}}}, nil
		}
		return &provider.ChatResponse{Content: "reported instead"}, nil
	})

	if err := ag.processMessage(context.Background(), scheduledMessage("web:task-2", "ask before you act")); err != nil {
		t.Fatalf("processMessage: %v", err)
	}

	var refusal bool
	for _, m := range store.GetOrCreate("web:task-2").GetMessages() {
		if m.Role == "tool" && strings.Contains(m.Content, "no user is available") {
			refusal = true
		}
	}
	if !refusal {
		t.Error("the question tool did not refuse in a scheduled (unattended) run")
	}
}

// A turn that a person started must not be marked unattended; the question tool
// keeps its blocking behaviour there.
func TestIsScheduledTurn_OnlySchedulerMessages(t *testing.T) {
	ordinary := &nanoprotocol.InboundMessage{SessionID: "web:c", Content: "hi"}
	if isScheduledTurn(ordinary) {
		t.Error("an ordinary message must not be treated as unattended")
	}
	if got := scheduledTaskName(ordinary); got != "" {
		t.Errorf("ordinary message carried a task name %q", got)
	}
	if !isScheduledTurn(scheduledMessage("web:task-1", "go")) {
		t.Error("a scheduler message must be treated as unattended")
	}
	if got := scheduledTaskName(scheduledMessage("web:task-1", "go")); got != "server patrol" {
		t.Errorf("task name = %q", got)
	}
}
