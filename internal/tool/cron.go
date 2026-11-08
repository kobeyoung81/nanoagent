package tool

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"gitee.com/kobeyoung81/nanosec/internal/cron"
	"gitee.com/kobeyoung81/nanosec/internal/provider"
)

// CronScheduler is the interface CronTool needs from the cron service.
type CronScheduler interface {
	AddJob(name string, sched cron.Schedule, payload cron.Payload, deleteAfterRun bool) (*cron.Job, error)
	RemoveJob(id string) bool
	ListJobs() []*cron.Job
}

// CronTool lets the agent schedule recurring tasks that start a turn of their
// own when they fire.
type CronTool struct {
	svc     CronScheduler
	mu      sync.RWMutex
	channel string
	chatID  string
}

// NewCronTool creates a CronTool backed by the given scheduler.
func NewCronTool(svc CronScheduler) *CronTool {
	return &CronTool{svc: svc}
}

// SetContext stores the current session context. The channel is what a new
// task's conversation is created on; the current chat ID is only used to check
// that the tool is being called from a real session.
func (t *CronTool) SetContext(channel, chatID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.channel = channel
	t.chatID = chatID
}

func (t *CronTool) Name() string { return "cron" }

func (t *CronTool) Description() string {
	return `Schedule recurring tasks. A task starts a real turn for you in its own ` +
		`conversation (one conversation per task) and runs unattended: no user is ` +
		`waiting, so execute it and report instead of asking questions. ` +
		`Actions: "add", "list", "remove".`
}

func (t *CronTool) Parameters() provider.ParameterSchema {
	return provider.ParameterSchema{
		Type: "object",
		Properties: map[string]provider.PropertySchema{
			"action": {
				Type:        "string",
				Description: `Action to perform: "add", "list", or "remove"`,
			},
			"message": {
				Type:        "string",
				Description: "What the task must do when it fires; this text starts the turn, so write it as an instruction",
			},
			"mode": {
				Type:        "string",
				Description: `Agent mode the task's turns run in: "build" (default, can act) or "plan"`,
			},
			"every_seconds": {
				Type:        "number",
				Description: "Interval in seconds for a recurring task (mutually exclusive with cron_expr)",
			},
			"cron_expr": {
				Type:        "string",
				Description: `Cron expression, e.g. "0 9 * * *" for 9 am daily (mutually exclusive with every_seconds)`,
			},
			"job_id": {
				Type:        "string",
				Description: "Job ID to remove (required for remove)",
			},
		},
		Required: []string{"action"},
	}
}

func (t *CronTool) Execute(_ context.Context, params map[string]any) (string, error) {
	action, _ := params["action"].(string)
	switch action {
	case "add":
		return t.add(params)
	case "list":
		return t.list()
	case "remove":
		return t.remove(params)
	default:
		return "", fmt.Errorf("unknown cron action %q; valid: add, list, remove", action)
	}
}

func (t *CronTool) add(params map[string]any) (string, error) {
	message, _ := params["message"].(string)
	if message == "" {
		return "", fmt.Errorf("message is required for cron add")
	}

	t.mu.RLock()
	channel, chatID := t.channel, t.chatID
	t.mu.RUnlock()

	if channel == "" || chatID == "" {
		return "", fmt.Errorf("no session context; cannot schedule delivery")
	}

	var sched cron.Schedule
	if evSecs, ok := params["every_seconds"].(float64); ok && evSecs > 0 {
		ms := int64(evSecs * 1000)
		sched = cron.Schedule{Kind: cron.ScheduleKindEvery, EveryMS: &ms}
	} else if expr, ok := params["cron_expr"].(string); ok && expr != "" {
		sched = cron.Schedule{Kind: cron.ScheduleKindCron, Expr: expr}
	} else {
		return "", fmt.Errorf("either every_seconds or cron_expr is required for cron add")
	}

	// The mode is optional: without it the task runs in the mode the agent (or
	// the platform) is configured for, which defaults to build.
	mode, _ := params["mode"].(string)
	if mode != "" && !cron.TaskModeNames[mode] {
		return "", fmt.Errorf("unknown mode %q; valid: build, plan", mode)
	}

	name := message
	if utf8.RuneCountInString(name) > 40 {
		name = string([]rune(name)[:40])
	}
	// The task owns a conversation of its own: naming it at creation time keeps
	// every run in one place, so the conversation is not polluted by (and does
	// not pollute) the one the task was requested from.
	payload := cron.Payload{
		Message: message,
		Channel: channel,
		ChatID:  cron.NewTaskChatID(),
		Mode:    mode,
	}
	job, err := t.svc.AddJob(name, sched, payload, false)
	if err != nil {
		return "", fmt.Errorf("schedule job: %w", err)
	}
	return fmt.Sprintf("Scheduled task '%s' (id: %s, conversation: %s:%s). It will start a turn of its own when it fires.",
		job.Name, job.ID, job.Payload.Channel, job.Payload.ChatID), nil
}

func (t *CronTool) list() (string, error) {
	jobs := t.svc.ListJobs()
	if len(jobs) == 0 {
		return "No scheduled jobs.", nil
	}
	var sb strings.Builder
	sb.WriteString("Scheduled jobs:\n")
	for _, j := range jobs {
		var schedDesc string
		switch j.Schedule.Kind {
		case cron.ScheduleKindEvery:
			if j.Schedule.EveryMS != nil {
				d := time.Duration(*j.Schedule.EveryMS) * time.Millisecond
				schedDesc = "every " + d.String()
			}
		case cron.ScheduleKindCron:
			schedDesc = "cron(" + j.Schedule.Expr + ")"
		case cron.ScheduleKindAt:
			schedDesc = "one-shot"
		}
		next := "never"
		if j.State.NextRunAtMS != nil {
			next = time.UnixMilli(*j.State.NextRunAtMS).Format(time.RFC3339)
		}
		mode := j.Payload.Mode
		if mode == "" {
			mode = "default"
		}
		fmt.Fprintf(&sb, "- [%s] %s (%s) mode: %s next: %s conversation: %s:%s\n",
			j.ID, j.Name, schedDesc, mode, next, j.Payload.Channel, j.Payload.ChatID)
	}
	return sb.String(), nil
}

func (t *CronTool) remove(params map[string]any) (string, error) {
	jobID, _ := params["job_id"].(string)
	if jobID == "" {
		return "", fmt.Errorf("job_id is required for cron remove")
	}
	if t.svc.RemoveJob(jobID) {
		return fmt.Sprintf("Removed job %s.", jobID), nil
	}
	return fmt.Sprintf("No job found with id %s.", jobID), nil
}
