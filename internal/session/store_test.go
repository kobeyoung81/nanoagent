package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"gitee.com/kobeyoung81/nanosec/internal/provider"
)

func TestStore_SaveLoad(t *testing.T) {
	// Create temp dir
	tmpDir, err := os.MkdirTemp("", "nanosec-session-test")
	if err != nil {
		t.Fatalf("MkdirTemp failed: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	store, err := NewStore(tmpDir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}

	// Create and populate session
	sessionID := "test:123"
	sess := store.GetOrCreate(sessionID)
	sess.AddMessage(provider.Message{Role: "user", Content: "msg1"})
	sess.AddMessage(provider.Message{Role: "assistant", Content: "msg2"})

	// Save
	if err := store.Save(sess); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// Verify file exists
	expectedPath := filepath.Join(tmpDir, "test_123.jsonl")
	if _, err := os.Stat(expectedPath); os.IsNotExist(err) {
		t.Errorf("Expected file %s to exist", expectedPath)
	}

	// Create new store instance to verify loading
	store2, _ := NewStore(tmpDir)
	loadedSess := store2.GetOrCreate(sessionID)

	msgs := loadedSess.GetMessages()
	if len(msgs) != 2 {
		t.Fatalf("Expected 2 messages, got %d", len(msgs))
	}
	if msgs[0].Content != "msg1" {
		t.Errorf("Expected first msg 'msg1', got '%s'", msgs[0].Content)
	}
	if msgs[1].Content != "msg2" {
		t.Errorf("Expected second msg 'msg2', got '%s'", msgs[1].Content)
	}
}

func TestStore_GetOrCreate(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "nanosec-session-test")
	defer os.RemoveAll(tmpDir)

	store, _ := NewStore(tmpDir)

	s1 := store.GetOrCreate("id1")
	s2 := store.GetOrCreate("id1")

	if s1 != s2 {
		t.Error("GetOrCreate should return same instance for same ID")
	}

	s3 := store.GetOrCreate("id2")
	if s1 == s3 {
		t.Error("GetOrCreate should return different instances for different IDs")
	}
}

func TestStore_LoadMetadata(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "nanosec-session-test")
	defer os.RemoveAll(tmpDir)

	store, _ := NewStore(tmpDir)
	sess := store.GetOrCreate("meta-test")

	// Set specific times
	created := time.Date(2023, 1, 1, 10, 0, 0, 0, time.UTC)
	updated := time.Date(2023, 1, 2, 10, 0, 0, 0, time.UTC)
	sess.CreatedAt = created
	sess.UpdatedAt = updated

	if err := store.Save(sess); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// Reload
	store2, _ := NewStore(tmpDir)
	loaded := store2.GetOrCreate("meta-test")

	if !loaded.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt mismatch. Expected %v, got %v", created, loaded.CreatedAt)
	}
	// Note: UpdatedAt might change if AddMessage is called during load?
	// No, Store.load() uses direct struct assignment, not AddMessage.
	if !loaded.UpdatedAt.Equal(updated) {
		t.Errorf("UpdatedAt mismatch. Expected %v, got %v", updated, loaded.UpdatedAt)
	}
}

// recordingSyncer captures the snapshots it is asked to persist.
type recordingSyncer struct {
	mu    sync.Mutex
	calls []SessionSnapshot
}

func (r *recordingSyncer) SyncSession(snap SessionSnapshot) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, snap)
	// What a full snapshot would put on the wire, so the byte accounting has
	// something real to add up.
	payload, err := json.Marshal(snap.Messages)
	if err != nil {
		return 0, err
	}
	return len(payload), nil
}

func (r *recordingSyncer) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// failingSyncer always fails, standing in for an external store that stops
// accepting a session's snapshots.
type failingSyncer struct {
	mu    sync.Mutex
	calls int
}

func (f *failingSyncer) SyncSession(SessionSnapshot) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return 0, errors.New("external store rejected the snapshot")
}

func (f *failingSyncer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// TestStore_SyncFailureIsCounted pins the ledger that keeps a stalled snapshot
// path from being invisible. Before this existed, a sync that failed every
// second logged one WARN per second and looked like nothing at all: the session
// still worked (JSONL is authoritative) while the portal silently served stale
// history.
func TestStore_SyncFailureIsCounted(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	syncer := &failingSyncer{}
	s.SetSyncer("agent-1", syncer)
	s.SetSyncInterval(time.Hour) // drive flushes by hand
	defer s.Close()

	if h := s.SyncHealth(); h.Failures != 0 || h.LastError != "" {
		t.Fatalf("expected a clean ledger before any flush, got %+v", h)
	}

	sess := New("web:chat-fail")
	sess.AddMessage(provider.Message{Role: "user", Content: "hi"})
	if err := s.Save(sess); err != nil {
		t.Fatal(err)
	}
	s.SyncNow()

	if syncer.count() != 1 {
		t.Fatalf("expected the syncer to be called once, got %d", syncer.count())
	}
	h := s.SyncHealth()
	if h.Failures != 1 {
		t.Fatalf("expected 1 recorded failure, got %d", h.Failures)
	}
	if h.LastError == "" {
		t.Error("expected the last error to be recorded")
	}
	if h.LastErrorAt.IsZero() {
		t.Error("expected the last error timestamp to be set")
	}

	// The counter is cumulative: a second failure keeps climbing, so a caller
	// that only looks occasionally still sees the growth.
	sess.AddMessage(provider.Message{Role: "assistant", Content: "still failing"})
	if err := s.Save(sess); err != nil {
		t.Fatal(err)
	}
	s.SyncNow()
	if h := s.SyncHealth(); h.Failures != 2 {
		t.Fatalf("expected the failure count to accumulate to 2, got %d", h.Failures)
	}
}

// TestStore_SyncHealthCleanOnSuccess pins the other side: a healthy path must
// not accrue failures, otherwise the signal is worthless as an alert.
func TestStore_SyncHealthCleanOnSuccess(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	syncer := &recordingSyncer{}
	s.SetSyncer("agent-1", syncer)
	s.SetSyncInterval(time.Hour)
	defer s.Close()

	sess := New("web:chat-ok")
	sess.AddMessage(provider.Message{Role: "user", Content: "hi"})
	if err := s.Save(sess); err != nil {
		t.Fatal(err)
	}
	s.SyncNow()

	if syncer.count() != 1 {
		t.Fatalf("expected 1 sync, got %d", syncer.count())
	}
	if h := s.SyncHealth(); h.Failures != 0 {
		t.Fatalf("expected a clean ledger after a successful sync, got %+v", h)
	}
}

// TestStore_AsyncSyncCoalesces verifies that a burst of saves for one session
// collapses into a single external write of the newest snapshot.
func TestStore_AsyncSyncCoalesces(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	syncer := &recordingSyncer{}
	s.SetSyncer("agent-1", syncer)
	s.SetSyncInterval(20 * time.Millisecond)
	defer s.Close()

	sess := New("web:chat-1")
	for i := 0; i < 5; i++ {
		sess.AddMessage(provider.Message{Role: "user", Content: fmt.Sprintf("m%d", i)})
		if err := s.Save(sess); err != nil {
			t.Fatal(err)
		}
	}

	// Wait past the flush interval for the worker to drain.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && syncer.count() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	// Allow a possible second (extra) flush to settle.
	time.Sleep(50 * time.Millisecond)

	calls := syncer.count()
	if calls != 1 {
		t.Fatalf("expected exactly 1 coalesced sync, got %d", calls)
	}
	syncer.mu.Lock()
	last := syncer.calls[0]
	syncer.mu.Unlock()
	if len(last.Messages) != 5 {
		t.Fatalf("expected latest snapshot with 5 messages, got %d", len(last.Messages))
	}
	if last.Messages[4].Content != "m4" {
		t.Errorf("expected newest content, got %q", last.Messages[4].Content)
	}
}

// TestStore_SyncNowFlushesImmediately verifies turn-end flush writes pending
// snapshots without waiting for the ticker.
func TestStore_SyncNowFlushesImmediately(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	syncer := &recordingSyncer{}
	s.SetSyncer("agent-1", syncer)
	s.SetSyncInterval(time.Hour) // never tick
	defer s.Close()

	sess := New("web:chat-2")
	sess.AddMessage(provider.Message{Role: "user", Content: "hi"})
	if err := s.Save(sess); err != nil {
		t.Fatal(err)
	}
	if syncer.count() != 0 {
		t.Fatalf("expected no sync before flush, got %d", syncer.count())
	}
	s.SyncNow()
	if syncer.count() != 1 {
		t.Fatalf("expected 1 sync after SyncNow, got %d", syncer.count())
	}
}

// TestStore_SyncDisabledSkipsExternalWrites verifies JSONL still saves even when
// external syncing is disabled.
func TestStore_SyncDisabledSkipsExternalWrites(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	syncer := &recordingSyncer{}
	s.SetSyncer("agent-1", syncer)
	s.SetSyncEnabled(false)
	defer s.Close()

	sess := New("web:chat-3")
	sess.AddMessage(provider.Message{Role: "user", Content: "hi"})
	if err := s.Save(sess); err != nil {
		t.Fatal(err)
	}
	s.SyncNow()
	if syncer.count() != 0 {
		t.Fatalf("expected no sync when disabled, got %d", syncer.count())
	}
	if _, err := os.Stat(filepath.Join(dir, "web_chat-3.jsonl")); err != nil {
		t.Errorf("expected JSONL file written even with sync disabled: %v", err)
	}
}
