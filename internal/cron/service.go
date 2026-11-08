package cron

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	robcron "github.com/robfig/cron/v3"
)

// JobCallback is invoked whenever a scheduled job fires.
type JobCallback func(ctx context.Context, job *Job)

// persistStore is the on-disk JSON format.
type persistStore struct {
	Version int    `json:"version"`
	Jobs    []*Job `json:"jobs"`
}

// Service manages scheduled jobs with file-backed persistence.
type Service struct {
	storePath string
	onJob     JobCallback
	mu        sync.Mutex
	jobs      []*Job
	cancel    context.CancelFunc
	parser    robcron.Parser
}

// New creates a Service. onJob is called on each job fire; it may be nil.
func New(storePath string, onJob JobCallback) *Service {
	return &Service{
		storePath: storePath,
		onJob:     onJob,
		parser: robcron.NewParser(
			robcron.Minute | robcron.Hour | robcron.Dom | robcron.Month | robcron.Dow,
		),
	}
}

// Start loads persisted jobs and begins the scheduling loop.
func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	if err := s.load(); err != nil {
		slog.Warn("cron: failed to load store", "error", err)
	}
	s.recomputeAll()
	_ = s.save()
	count := len(s.jobs)
	s.mu.Unlock()

	slog.Info("cron: service started", "jobs", count)

	loopCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	go s.loop(loopCtx)
	return nil
}

// Stop halts the scheduling loop.
func (s *Service) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
}

// AddJob creates and persists a new job. Returns the created Job.
func (s *Service) AddJob(name string, sched Schedule, payload Payload, deleteAfterRun bool) (*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := nowMS()
	next := computeNext(s.parser, sched, now)
	job := &Job{
		ID:             uuid.New().String()[:8],
		Name:           name,
		Enabled:        true,
		Schedule:       sched,
		Payload:        payload,
		State:          JobState{NextRunAtMS: next},
		CreatedAtMS:    now,
		UpdatedAtMS:    now,
		DeleteAfterRun: deleteAfterRun,
	}

	s.jobs = append(s.jobs, job)
	if err := s.save(); err != nil {
		return nil, fmt.Errorf("cron save: %w", err)
	}
	slog.Info("cron: job added", "name", name, "id", job.ID)
	return job, nil
}

// RemoveJob deletes a job by ID. Returns true if the job was found.
func (s *Service) RemoveJob(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := s.removeLocked(id)
	if removed {
		_ = s.save()
		slog.Info("cron: job removed", "id", id)
	}
	return removed
}

// ListJobs returns all currently enabled jobs.
func (s *Service) ListJobs() []*Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		if j.Enabled {
			out = append(out, j)
		}
	}
	return out
}

// loop checks for due jobs every second.
func (s *Service) loop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

func (s *Service) tick(ctx context.Context) {
	now := nowMS()
	s.mu.Lock()
	var due []*Job
	for _, j := range s.jobs {
		if j.Enabled && j.State.NextRunAtMS != nil && now >= *j.State.NextRunAtMS {
			due = append(due, j)
		}
	}
	s.mu.Unlock()

	for _, j := range due {
		s.fire(ctx, j)
	}

	if len(due) > 0 {
		s.mu.Lock()
		_ = s.save()
		s.mu.Unlock()
	}
}

func (s *Service) fire(ctx context.Context, job *Job) {
	slog.Info("cron: firing job", "name", job.Name, "id", job.ID)
	if s.onJob != nil {
		s.onJob(ctx, job)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := nowMS()
	job.State.LastRunAtMS = &now
	job.State.LastStatus = "ok"
	job.UpdatedAtMS = nowMS()

	if job.Schedule.Kind == ScheduleKindAt {
		if job.DeleteAfterRun {
			s.removeLocked(job.ID)
		} else {
			job.Enabled = false
			job.State.NextRunAtMS = nil
		}
	} else {
		job.State.NextRunAtMS = computeNext(s.parser, job.Schedule, nowMS())
	}
}

func (s *Service) recomputeAll() {
	now := nowMS()
	for _, j := range s.jobs {
		if j.Enabled {
			j.State.NextRunAtMS = computeNext(s.parser, j.Schedule, now)
		}
	}
}

func (s *Service) removeLocked(id string) bool {
	n := len(s.jobs)
	kept := s.jobs[:0]
	for _, j := range s.jobs {
		if j.ID != id {
			kept = append(kept, j)
		}
	}
	s.jobs = kept
	return len(s.jobs) < n
}

func (s *Service) load() error {
	data, err := os.ReadFile(s.storePath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	var ps persistStore
	if err := json.Unmarshal(data, &ps); err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	s.jobs = ps.Jobs
	return nil
}

func (s *Service) save() error {
	if err := os.MkdirAll(filepath.Dir(s.storePath), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	ps := persistStore{Version: 1, Jobs: s.jobs}
	data, err := json.MarshalIndent(ps, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	return os.WriteFile(s.storePath, data, 0o644)
}

// computeNext returns the next scheduled run time in Unix-milliseconds, or nil.
func computeNext(parser robcron.Parser, sched Schedule, nowMs int64) *int64 {
	switch sched.Kind {
	case ScheduleKindAt:
		if sched.AtMS != nil && *sched.AtMS > nowMs {
			return sched.AtMS
		}
	case ScheduleKindEvery:
		if sched.EveryMS != nil && *sched.EveryMS > 0 {
			next := nowMs + *sched.EveryMS
			return &next
		}
	case ScheduleKindCron:
		if sched.Expr != "" {
			parsed, err := parser.Parse(sched.Expr)
			if err != nil {
				slog.Warn("cron: invalid expression", "expr", sched.Expr, "error", err)
				return nil
			}
			t := parsed.Next(time.UnixMilli(nowMs)).UnixMilli()
			return &t
		}
	}
	return nil
}
