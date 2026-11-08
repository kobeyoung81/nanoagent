package cron

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestService_AddAndListJobs(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "cron.json")
	svc := New(storePath, nil)

	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer svc.Stop()

	ms := int64(60_000)
	job, err := svc.AddJob("test task", Schedule{
		Kind:    ScheduleKindEvery,
		EveryMS: &ms,
	}, Payload{Message: "hello", Channel: "cli", ChatID: "1"}, false)

	if err != nil {
		t.Fatalf("AddJob failed: %v", err)
	}
	if job.ID == "" {
		t.Error("expected non-empty job ID")
	}
	if job.Name != "test task" {
		t.Errorf("unexpected name: %s", job.Name)
	}
	if job.State.NextRunAtMS == nil {
		t.Error("expected NextRunAtMS to be set")
	}

	jobs := svc.ListJobs()
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	if jobs[0].ID != job.ID {
		t.Errorf("unexpected job ID in list")
	}
}

func TestService_RemoveJob(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "cron.json")
	svc := New(storePath, nil)
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Stop()

	ms := int64(60_000)
	job, _ := svc.AddJob("to remove", Schedule{Kind: ScheduleKindEvery, EveryMS: &ms},
		Payload{Message: "bye"}, false)

	if !svc.RemoveJob(job.ID) {
		t.Error("expected RemoveJob to return true")
	}
	if svc.RemoveJob(job.ID) {
		t.Error("expected second RemoveJob to return false")
	}
	if len(svc.ListJobs()) != 0 {
		t.Error("expected empty job list after removal")
	}
}

func TestService_JobFires(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "cron.json")

	fired := make(chan string, 1)
	svc := New(storePath, func(_ context.Context, job *Job) {
		fired <- job.ID
	})

	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Stop()

	// Schedule to fire in 500ms
	ms := int64(500)
	job, err := svc.AddJob("fast job", Schedule{Kind: ScheduleKindEvery, EveryMS: &ms},
		Payload{Message: "tick"}, false)
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	select {
	case id := <-fired:
		if id != job.ID {
			t.Errorf("unexpected job ID fired: %s", id)
		}
	case <-time.After(3 * time.Second):
		t.Error("job did not fire within 3s")
	}
}

func TestService_Persistence(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "cron.json")

	// Add a job, stop, restart, check it persisted.
	svc := New(storePath, nil)
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	ms := int64(60_000)
	job, _ := svc.AddJob("persist me", Schedule{Kind: ScheduleKindEvery, EveryMS: &ms},
		Payload{Message: "saved"}, false)
	svc.Stop()

	// Verify file was written
	if _, err := os.Stat(storePath); err != nil {
		t.Fatalf("store file not found: %v", err)
	}

	// Restart and check the job is reloaded
	svc2 := New(storePath, nil)
	if err := svc2.Start(context.Background()); err != nil {
		t.Fatalf("Start2: %v", err)
	}
	defer svc2.Stop()

	jobs := svc2.ListJobs()
	if len(jobs) != 1 {
		t.Fatalf("expected 1 persisted job, got %d", len(jobs))
	}
	if jobs[0].ID != job.ID {
		t.Errorf("persisted job ID mismatch")
	}
}

func TestComputeNext_Every(t *testing.T) {
	svc := New("", nil)
	ms := int64(5_000)
	sched := Schedule{Kind: ScheduleKindEvery, EveryMS: &ms}
	nowMs := time.Now().UnixMilli()

	next := computeNext(svc.parser, sched, nowMs)
	if next == nil {
		t.Fatal("expected non-nil next time")
	}
	diff := *next - nowMs
	if diff < 4900 || diff > 5100 {
		t.Errorf("unexpected next interval: %dms", diff)
	}
}

func TestComputeNext_At_InPast(t *testing.T) {
	svc := New("", nil)
	past := time.Now().Add(-time.Minute).UnixMilli()
	sched := Schedule{Kind: ScheduleKindAt, AtMS: &past}
	next := computeNext(svc.parser, sched, time.Now().UnixMilli())
	if next != nil {
		t.Error("expected nil for past 'at' schedule")
	}
}

func TestComputeNext_CronExpr(t *testing.T) {
	svc := New("", nil)
	sched := Schedule{Kind: ScheduleKindCron, Expr: "0 9 * * *"} // 9am daily
	next := computeNext(svc.parser, sched, time.Now().UnixMilli())
	if next == nil {
		t.Fatal("expected non-nil next time for cron expression")
	}
	// Next 9am should be within the next 24 hours
	diff := time.Until(time.UnixMilli(*next))
	if diff <= 0 || diff > 25*time.Hour {
		t.Errorf("unexpected next cron time: %v from now", diff)
	}
}

func TestComputeNext_InvalidCronExpr(t *testing.T) {
	svc := New("", nil)
	sched := Schedule{Kind: ScheduleKindCron, Expr: "not a valid cron"}
	next := computeNext(svc.parser, sched, time.Now().UnixMilli())
	if next != nil {
		t.Error("expected nil for invalid cron expression")
	}
}
