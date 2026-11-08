// Package cron provides a lightweight persistent job scheduler.
package cron

import "time"

// ScheduleKind represents how a job is triggered.
type ScheduleKind string

const (
	// ScheduleKindAt fires once at a specific Unix-millisecond timestamp.
	ScheduleKindAt ScheduleKind = "at"
	// ScheduleKindEvery fires repeatedly on a fixed millisecond interval.
	ScheduleKindEvery ScheduleKind = "every"
	// ScheduleKindCron fires according to a standard 5-field cron expression.
	ScheduleKindCron ScheduleKind = "cron"
)

// Schedule defines when a job should run.
type Schedule struct {
	Kind    ScheduleKind `json:"kind"`
	AtMS    *int64       `json:"atMs,omitempty"`    // Unix ms for "at"
	EveryMS *int64       `json:"everyMs,omitempty"` // Interval ms for "every"
	Expr    string       `json:"expr,omitempty"`    // Cron expression for "cron"
}

// Payload defines what happens when a job fires. Firing injects the message as
// an inbound message for the owning agent, which starts a real turn in the
// task's own conversation (channel:chatID); there is no separate "deliver a
// notification" mode. A payload persisted by an older version carries a
// "deliver" field that is simply ignored on load.
type Payload struct {
	Message string `json:"message"`           // Instruction the agent executes
	Channel string `json:"channel,omitempty"` // Channel the task's conversation lives on
	ChatID  string `json:"chatId,omitempty"`  // Chat ID that names the task's conversation
	// Mode is the agent mode the task's turns run in ("build" | "plan"). Empty
	// means the agent's own setting, then the platform default, then build.
	Mode string `json:"mode,omitempty"`
}

// JobState holds mutable runtime state for a job.
type JobState struct {
	NextRunAtMS *int64 `json:"nextRunAtMs,omitempty"`
	LastRunAtMS *int64 `json:"lastRunAtMs,omitempty"`
	LastStatus  string `json:"lastStatus,omitempty"` // "ok" | "error"
	LastError   string `json:"lastError,omitempty"`
}

// Job represents a single scheduled task.
type Job struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Enabled        bool     `json:"enabled"`
	Schedule       Schedule `json:"schedule"`
	Payload        Payload  `json:"payload"`
	State          JobState `json:"state"`
	CreatedAtMS    int64    `json:"createdAtMs"`
	UpdatedAtMS    int64    `json:"updatedAtMs"`
	DeleteAfterRun bool     `json:"deleteAfterRun"`
}

// nowMS returns the current Unix time in milliseconds.
func nowMS() int64 {
	return time.Now().UnixMilli()
}
