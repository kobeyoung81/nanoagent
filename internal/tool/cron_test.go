package tool

import (
	"context"
	"strings"
	"testing"

	"gitee.com/kobeyoung81/nanosec/internal/cron"
)

// mockCronScheduler implements CronScheduler for tests.
type mockCronScheduler struct {
	jobs    []*cron.Job
	removed []string
}

func (m *mockCronScheduler) AddJob(name string, sched cron.Schedule, payload cron.Payload, deleteAfterRun bool) (*cron.Job, error) {
	job := &cron.Job{ID: "test-id", Name: name, Enabled: true, Schedule: sched, Payload: payload}
	m.jobs = append(m.jobs, job)
	return job, nil
}

func (m *mockCronScheduler) RemoveJob(id string) bool {
	m.removed = append(m.removed, id)
	for i, j := range m.jobs {
		if j.ID == id {
			m.jobs = append(m.jobs[:i], m.jobs[i+1:]...)
			return true
		}
	}
	return false
}

func (m *mockCronScheduler) ListJobs() []*cron.Job { return m.jobs }

func TestCronTool_Name(t *testing.T) {
	ct := NewCronTool(&mockCronScheduler{})
	if ct.Name() != "cron" {
		t.Errorf("unexpected name: %s", ct.Name())
	}
}

func TestCronTool_Add_MissingMessage(t *testing.T) {
	ct := NewCronTool(&mockCronScheduler{})
	ct.SetContext("cli", "user1")
	_, err := ct.Execute(context.Background(), map[string]any{
		"action": "add",
	})
	if err == nil {
		t.Error("expected error for missing message")
	}
}

func TestCronTool_Add_NoContext(t *testing.T) {
	ct := NewCronTool(&mockCronScheduler{})
	// No SetContext called — channel/chatID are empty
	_, err := ct.Execute(context.Background(), map[string]any{
		"action":        "add",
		"message":       "hello",
		"every_seconds": float64(60),
	})
	if err == nil {
		t.Error("expected error when no session context set")
	}
}

func TestCronTool_Add_NoSchedule(t *testing.T) {
	ct := NewCronTool(&mockCronScheduler{})
	ct.SetContext("cli", "user1")
	_, err := ct.Execute(context.Background(), map[string]any{
		"action":  "add",
		"message": "remind me",
	})
	if err == nil {
		t.Error("expected error when neither every_seconds nor cron_expr provided")
	}
}

func TestCronTool_Add_EverySeconds(t *testing.T) {
	mock := &mockCronScheduler{}
	ct := NewCronTool(mock)
	ct.SetContext("cli", "user1")

	result, err := ct.Execute(context.Background(), map[string]any{
		"action":        "add",
		"message":       "stand up time",
		"every_seconds": float64(3600),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mock.jobs) != 1 {
		t.Fatalf("expected 1 job added, got %d", len(mock.jobs))
	}
	if mock.jobs[0].Schedule.Kind != cron.ScheduleKindEvery {
		t.Errorf("expected ScheduleKindEvery, got %s", mock.jobs[0].Schedule.Kind)
	}
	if result == "" {
		t.Error("expected non-empty result")
	}
}

// A task must own a conversation of its own: the chat id is minted per task
// rather than borrowed from the session the task was requested in, otherwise
// every run would be appended to the user's own conversation.
func TestCronTool_Add_TaskGetsItsOwnConversation(t *testing.T) {
	mock := &mockCronScheduler{}
	ct := NewCronTool(mock)
	ct.SetContext("web", "chat-of-the-user")

	if _, err := ct.Execute(context.Background(), map[string]any{
		"action":        "add",
		"message":       "check the backups",
		"every_seconds": float64(86400),
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	payload := mock.jobs[0].Payload
	if payload.Channel != "web" {
		t.Errorf("channel = %q, want web", payload.Channel)
	}
	if payload.ChatID == "chat-of-the-user" {
		t.Error("the task reused the requesting conversation")
	}
	if !strings.HasPrefix(payload.ChatID, cron.TaskChatPrefix) {
		t.Errorf("chat id = %q, want prefix %q", payload.ChatID, cron.TaskChatPrefix)
	}
	if payload.Message != "check the backups" {
		t.Errorf("message = %q", payload.Message)
	}
}

// Two tasks started from the same conversation must not share one.
func TestCronTool_Add_TwoTasksGetDifferentConversations(t *testing.T) {
	mock := &mockCronScheduler{}
	ct := NewCronTool(mock)
	ct.SetContext("web", "chat-of-the-user")

	for _, msg := range []string{"task one", "task two"} {
		if _, err := ct.Execute(context.Background(), map[string]any{
			"action":        "add",
			"message":       msg,
			"every_seconds": float64(60),
		}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if mock.jobs[0].Payload.ChatID == mock.jobs[1].Payload.ChatID {
		t.Errorf("both tasks got chat id %q", mock.jobs[0].Payload.ChatID)
	}
}

func TestCronTool_Add_CronExpr(t *testing.T) {
	mock := &mockCronScheduler{}
	ct := NewCronTool(mock)
	ct.SetContext("wecom", "group1")

	_, err := ct.Execute(context.Background(), map[string]any{
		"action":    "add",
		"message":   "daily standup",
		"cron_expr": "0 9 * * 1-5",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mock.jobs[0].Schedule.Expr != "0 9 * * 1-5" {
		t.Errorf("unexpected cron expr: %s", mock.jobs[0].Schedule.Expr)
	}
}

func TestCronTool_List_Empty(t *testing.T) {
	ct := NewCronTool(&mockCronScheduler{})
	result, err := ct.Execute(context.Background(), map[string]any{"action": "list"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "No scheduled jobs." {
		t.Errorf("unexpected result: %s", result)
	}
}

func TestCronTool_List_WithJobs(t *testing.T) {
	mock := &mockCronScheduler{}
	ms := int64(60_000)
	mock.jobs = []*cron.Job{
		{ID: "abc", Name: "test job", Enabled: true, Schedule: cron.Schedule{Kind: cron.ScheduleKindEvery, EveryMS: &ms}},
	}
	ct := NewCronTool(mock)
	result, err := ct.Execute(context.Background(), map[string]any{"action": "list"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == "No scheduled jobs." {
		t.Error("expected non-empty job list")
	}
}

func TestCronTool_Remove_MissingID(t *testing.T) {
	ct := NewCronTool(&mockCronScheduler{})
	_, err := ct.Execute(context.Background(), map[string]any{"action": "remove"})
	if err == nil {
		t.Error("expected error for missing job_id")
	}
}

func TestCronTool_Remove_Found(t *testing.T) {
	mock := &mockCronScheduler{}
	ms := int64(60_000)
	mock.jobs = []*cron.Job{
		{ID: "del-me", Name: "job", Enabled: true, Schedule: cron.Schedule{Kind: cron.ScheduleKindEvery, EveryMS: &ms}},
	}
	ct := NewCronTool(mock)
	result, err := ct.Execute(context.Background(), map[string]any{
		"action": "remove",
		"job_id": "del-me",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mock.jobs) != 0 {
		t.Error("expected job to be removed from mock")
	}
	if result == "" {
		t.Error("expected non-empty result")
	}
}

func TestCronTool_UnknownAction(t *testing.T) {
	ct := NewCronTool(&mockCronScheduler{})
	_, err := ct.Execute(context.Background(), map[string]any{"action": "explode"})
	if err == nil {
		t.Error("expected error for unknown action")
	}
}

func TestCronTool_SetContext(t *testing.T) {
	ct := NewCronTool(&mockCronScheduler{})
	ct.SetContext("telegram", "chat42")
	ct.mu.RLock()
	ch, id := ct.channel, ct.chatID
	ct.mu.RUnlock()
	if ch != "telegram" || id != "chat42" {
		t.Errorf("unexpected context: channel=%s chatID=%s", ch, id)
	}
}

// The mode is optional; when given it must be one the platform runs, and it
// travels with the task so the run does not inherit a conversation default.
func TestCronTool_Add_Mode(t *testing.T) {
	mock := &mockCronScheduler{}
	ct := NewCronTool(mock)
	ct.SetContext("web", "chat-of-the-user")

	if _, err := ct.Execute(context.Background(), map[string]any{
		"action":        "add",
		"message":       "patrol",
		"every_seconds": float64(60),
		"mode":          "plan",
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := mock.jobs[0].Payload.Mode; got != "plan" {
		t.Errorf("payload mode = %q, want plan", got)
	}

	// Omitted: the task follows the agent/platform setting.
	if _, err := ct.Execute(context.Background(), map[string]any{
		"action":        "add",
		"message":       "patrol",
		"every_seconds": float64(60),
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := mock.jobs[1].Payload.Mode; got != "" {
		t.Errorf("payload mode = %q, want empty (inherit)", got)
	}

	// Anything else is rejected instead of silently running in another mode.
	if _, err := ct.Execute(context.Background(), map[string]any{
		"action":        "add",
		"message":       "patrol",
		"every_seconds": float64(60),
		"mode":          "turbo",
	}); err == nil {
		t.Error("expected an error for an unknown mode")
	}
}
