package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gitee.com/kobeyoung81/nanosec/internal/provider"
)

// SessionSnapshot is a point-in-time copy of a session suitable for async
// persistence to an external store without holding the session lock.
type SessionSnapshot struct {
	SessionID   string
	AgentID     string
	Messages    []provider.Message
	Summary     string
	Meta        map[string]string
	TurnRunning bool
}

// SessionSyncer persists session snapshots to an external backend (the bus, and
// through it the platform's store). It reports how many bytes it actually put on
// the wire, so the sync statistics measure what was sent rather than the size of
// the conversation — after the delta protocol those two are very different
// numbers, and the whole point of the change is that they are.
type SessionSyncer interface {
	SyncSession(snap SessionSnapshot) (int, error)
}

// SyncHealth is the external snapshot path's failure bookkeeping.
//
// The JSONL file stays authoritative for the agent's own context, so a failure
// here degrades the *portal's* view rather than the agent's memory — but it must
// never be invisible. A session whose snapshot stops landing serves stale history
// with no other symptom, which is exactly what happens when a whole-row upsert
// hits a payload ceiling: the write keeps failing and only a log line says so.
type SyncHealth struct {
	// Failures is the cumulative number of snapshots that did not reach the
	// external store since this Store was created. Cumulative on purpose: any
	// later successful caller can report it, so a failure is never missed just
	// because it happened on a path nobody looked at.
	Failures int64
	// LastError is the most recent failure; empty when there has been none.
	LastError string
	// LastErrorAt is when LastError was recorded; zero when there has been none.
	LastErrorAt time.Time
}

// defaultSyncFlushInterval bounds how long a coalesced DB snapshot can lag the
// JSONL write; turn-end flushes happen immediately regardless.
const defaultSyncFlushInterval = time.Second

// Store handles file-based session storage.
type Store struct {
	dir     string
	agentID string
	syncer  SessionSyncer
	cache   map[string]*Session
	mu      sync.RWMutex
	// saveMu serializes JSONL writes so the throttled mid-turn saver cannot
	// race the agent loop's save points on the same file.
	saveMu sync.Mutex

	// Async, coalescing sync-to-external-store machinery. The JSONL file write
	// stays synchronous (authoritative + prompt-cache safe); the DB snapshot is
	// flushed by a background worker so remote MySQL latency never blocks the
	// agent loop. Because each upsert replaces the whole row, per-session bursts
	// coalesce into a single write of the newest snapshot.
	syncEnabled  bool
	syncInterval time.Duration
	syncMu       sync.Mutex
	pending      map[string]SessionSnapshot
	syncStop     chan struct{}
	syncDone     chan struct{}
	syncWG       sync.WaitGroup

	// Profiling aggregates (best-effort, logged on Close).
	statsMu     sync.Mutex
	saveCount   int64
	syncCount   int64
	syncBytes   int64
	syncElapsed time.Duration
	// Failure bookkeeping for the external snapshot path (see SyncHealth).
	syncFailures    int64
	lastSyncError   string
	lastSyncErrorAt time.Time
}

// NewStore creates a new session store.
func NewStore(dir string) (*Store, error) {
	// Expand tilde if present
	if strings.HasPrefix(dir, "~/") {
		dirname, _ := os.UserHomeDir()
		dir = filepath.Join(dirname, dir[2:])
	}

	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create session dir: %w", err)
	}

	s := &Store{
		dir:          dir,
		cache:        make(map[string]*Session),
		syncEnabled:  true,
		syncInterval: defaultSyncFlushInterval,
		pending:      make(map[string]SessionSnapshot),
		syncStop:     make(chan struct{}),
		syncDone:     make(chan struct{}),
	}
	s.syncWG.Add(1)
	go s.syncLoop()
	return s, nil
}

// SetSyncer enables dual-write persistence of sessions to an external store.
func (s *Store) SetSyncer(agentID string, syncer SessionSyncer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.agentID = agentID
	s.syncer = syncer
}

// SetSyncEnabled toggles external-store syncing (default on).
func (s *Store) SetSyncEnabled(enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncEnabled = enabled
}

// SetSyncInterval sets the coalesced DB flush interval.
func (s *Store) SetSyncInterval(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d <= 0 {
		d = defaultSyncFlushInterval
	}
	s.syncInterval = d
}

// SyncNow flushes any pending snapshots immediately (used at turn end).
func (s *Store) SyncNow() {
	s.flushPending()
}

// Close stops the sync worker, flushes the last pending snapshots, and logs
// profiling aggregates.
func (s *Store) Close() {
	select {
	case <-s.syncStop:
		return // already closed
	default:
	}
	close(s.syncStop)
	s.syncWG.Wait()
	s.flushPending()

	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	if s.syncCount > 0 || s.syncFailures > 0 {
		slog.Info("session store sync stats",
			"saves", s.saveCount,
			"syncs", s.syncCount,
			"bytes", s.syncBytes,
			"elapsed_ms", s.syncElapsed.Milliseconds(),
			"failures", s.syncFailures,
			"last_error", s.lastSyncError,
		)
	}
}

// SyncHealth returns the external snapshot path's failure bookkeeping. Callers
// decide how loudly to complain; the Store only keeps the ledger.
func (s *Store) SyncHealth() SyncHealth {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	return SyncHealth{
		Failures:    s.syncFailures,
		LastError:   s.lastSyncError,
		LastErrorAt: s.lastSyncErrorAt,
	}
}

// recordSyncFailure counts a snapshot that did not reach the external store and
// logs it with the running total.
//
// A lone WARN is easy to lose in a stream (and was, in fact, lost: the session
// sync failing every second looked like nothing at all). The climbing counter is
// what an operator can alert on, and SyncHealth is what the agent surfaces.
func (s *Store) recordSyncFailure(sessionID string, err error) {
	s.statsMu.Lock()
	s.syncFailures++
	failures := s.syncFailures
	s.lastSyncError = err.Error()
	s.lastSyncErrorAt = time.Now()
	s.statsMu.Unlock()

	slog.Warn("session sync to external store failed",
		"session_id", sessionID,
		"error", err,
		"sync_failures_total", failures,
	)
}

// syncLoop drains pending snapshots on a fixed interval until Close.
func (s *Store) syncLoop() {
	defer s.syncWG.Done()
	defer close(s.syncDone)
	// Read the interval under the lock: SetSyncInterval may be called right
	// after NewStore (config wiring), and syncLoop reads it once to create its
	// ticker. Racing there was a real data race under -race.
	s.mu.RLock()
	interval := s.syncInterval
	s.mu.RUnlock()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.syncStop:
			return
		case <-ticker.C:
			s.flushPending()
		}
	}
}

// flushPending writes the latest snapshot for every dirty session. It never
// holds the session lock: snapshots were copied at enqueue time.
func (s *Store) flushPending() {
	s.mu.RLock()
	syncer := s.syncer
	enabled := s.syncEnabled
	s.mu.RUnlock()
	if syncer == nil || !enabled {
		return
	}

	s.syncMu.Lock()
	if len(s.pending) == 0 {
		s.syncMu.Unlock()
		return
	}
	batch := s.pending
	s.pending = make(map[string]SessionSnapshot)
	s.syncMu.Unlock()

	for _, snap := range batch {
		start := time.Now()
		// No marshal here any more: the syncer decides what to send (the whole
		// conversation, or only what is new) and reports the size. Serializing the
		// full array just to count its bytes was the last place the flush loop
		// touched every message.
		sent, err := syncer.SyncSession(snap)
		if err != nil {
			s.recordSyncFailure(snap.SessionID, err)
		}
		s.statsMu.Lock()
		s.syncCount++
		s.syncBytes += int64(sent)
		s.syncElapsed += time.Since(start)
		s.statsMu.Unlock()
	}
}

// enqueueSnapshot records the latest snapshot for a session; a later save for
// the same session replaces it (coalescing) so a burst of saves costs one write.
func (s *Store) enqueueSnapshot(snap SessionSnapshot) {
	s.mu.RLock()
	enabled := s.syncEnabled
	s.mu.RUnlock()
	if !enabled {
		return
	}
	s.syncMu.Lock()
	s.pending[snap.SessionID] = snap
	s.syncMu.Unlock()
}

// GetOrCreate retrieves a session or creates a new one if it doesn't exist.
func (s *Store) GetOrCreate(sessionID string) *Session {
	s.mu.RLock()
	sess, ok := s.cache[sessionID]
	s.mu.RUnlock()
	if ok {
		return sess
	}

	// Try to load from file
	sess = s.load(sessionID)
	if sess == nil {
		sess = New(sessionID)
	}

	s.mu.Lock()
	s.cache[sessionID] = sess
	s.mu.Unlock()

	return sess
}

// ActiveVersion returns the newest live session for a base key, starting a new
// version when every existing one is archived.
//
// Base key format: "channel:chatID". Versioned: "channel:chatID:v2", etc.
// Archived sessions are never returned: a compaction archives the segment it
// replaced, and a client still holding that id must continue in the live one
// instead of appending to a transcript the portal has already collapsed into
// the previous version.
func (s *Store) ActiveVersion(baseKey string) *Session {
	if id := s.activeVersionID(baseKey); id != "" {
		return s.GetOrCreate(id)
	}
	return s.NewSession(baseKey)
}

// activeVersionID returns the id of the newest non-archived version of a base
// key ("" when every existing version is archived or none exists). It must not
// be called while holding s.mu: it resolves cached versions itself and loads
// on-disk ones to read their archived marker.
func (s *Store) activeVersionID(baseKey string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	maxVersion := -1
	activeID := ""
	consider := func(id string, sess *Session) {
		base, ver := ParseSessionKey(id)
		if base != baseKey || ver < maxVersion {
			return
		}
		if sess != nil && sess.GetMeta("_archived") == "true" {
			return
		}
		maxVersion, activeID = ver, id
	}

	for id, sess := range s.cache {
		consider(id, sess)
	}

	// On-disk versions not in the cache (archived segments are evicted from it).
	// The glob is only a prefilter: the base key is verified per candidate, so a
	// sibling chat whose name is a prefix of this one is never matched.
	pattern := strings.ReplaceAll(baseKey, ":", "_") + "*.jsonl"
	matches, _ := filepath.Glob(filepath.Join(s.dir, pattern))
	for _, m := range matches {
		name := strings.TrimSuffix(filepath.Base(m), ".jsonl")
		id := strings.ReplaceAll(name, "_", ":")
		if _, ver := ParseSessionKey(id); ver < maxVersion {
			continue
		}
		consider(id, s.load(id))
	}
	return activeID
}

// NewSession creates a new versioned session for a base key, archiving the current one.
func (s *Store) NewSession(baseKey string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Find current max version
	maxVersion := 0
	for id := range s.cache {
		base, ver := ParseSessionKey(id)
		if base == baseKey && ver > maxVersion {
			maxVersion = ver
		}
	}

	// Also check disk
	pattern := strings.ReplaceAll(baseKey, ":", "_") + "*.jsonl"
	matches, _ := filepath.Glob(filepath.Join(s.dir, pattern))
	for _, m := range matches {
		name := strings.TrimSuffix(filepath.Base(m), ".jsonl")
		id := strings.ReplaceAll(name, "_", ":")
		_, ver := ParseSessionKey(id)
		if ver > maxVersion {
			maxVersion = ver
		}
	}

	newID := fmt.Sprintf("%s:v%d", baseKey, maxVersion+1)
	sess := New(newID)
	s.cache[newID] = sess
	return sess
}

// ArchiveSession marks the current session as archived (sets metadata) and removes from cache.
func (s *Store) ArchiveSession(sessionID string) error {
	s.mu.Lock()
	sess, ok := s.cache[sessionID]
	if ok {
		sess.SetMeta("_archived", "true")
		delete(s.cache, sessionID)
	}
	s.mu.Unlock()

	if ok {
		return s.Save(sess)
	}
	return nil
}

// ParseSessionKey splits "channel:chatID:vN" into base key and version.
func ParseSessionKey(id string) (baseKey string, version int) {
	// Look for :vN suffix
	idx := strings.LastIndex(id, ":v")
	if idx < 0 {
		return id, 0
	}
	suffix := id[idx+2:]
	ver := 0
	for _, c := range suffix {
		if c >= '0' && c <= '9' {
			ver = ver*10 + int(c-'0')
		} else {
			return id, 0 // not a version suffix
		}
	}
	return id[:idx], ver
}

// filePath returns the file path for a session ID.
func (s *Store) filePath(sessionID string) string {
	// sanitize: replace : with _ to be safe on fs
	safe := strings.ReplaceAll(sessionID, ":", "_")
	return filepath.Join(s.dir, safe+".jsonl")
}

// load loads a session from disk.
func (s *Store) load(sessionID string) *Session {
	path := s.filePath(sessionID)
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	sess := New(sessionID)
	// Reset messages as New() creates empty slice
	sess.Messages = make([]provider.Message, 0)

	scanner := bufio.NewScanner(f)
	// Default bufio.Scanner cap is 64 KB. Large tool results (e.g. bash output)
	// can exceed that and cause Scan() to silently stop, losing session history.
	// Allow up to 16 MB per JSONL line.
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var data map[string]any
		if err := json.Unmarshal([]byte(line), &data); err != nil {
			continue
		}

		if data["_type"] == "metadata" {
			if t, ok := data["created_at"].(string); ok {
				sess.CreatedAt, _ = time.Parse(time.RFC3339, t)
			}
			if t, ok := data["updated_at"].(string); ok {
				sess.UpdatedAt, _ = time.Parse(time.RFC3339, t)
			}
			if v, ok := data["summary"].(string); ok {
				sess.Summary = v
			}
			if v, ok := data["memory_flushed"].(bool); ok {
				sess.MemoryFlushed = v
			}
			if md, ok := data["metadata"].(map[string]any); ok {
				for k, v := range md {
					if sv, ok := v.(string); ok {
						sess.Metadata[k] = sv
					}
				}
			}
		} else {
			var msg provider.Message
			if err := json.Unmarshal([]byte(line), &msg); err == nil {
				sess.Messages = append(sess.Messages, msg)
			}
		}
	}

	return sess
}

// Save persists the session to disk (synchronous, authoritative) and enqueues
// a snapshot for async, coalesced sync to the external store. File write
// failures are returned; external sync failures are logged but do not fail the
// save. The JSONL write is profiled so DB-vs-disk costs are observable.
func (s *Store) Save(sess *Session) error {
	// Serialize writers: os.Create truncates, so interleaved saves would corrupt
	// the file. Locking here (not the per-session lock) keeps it simple.
	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	s.mu.RLock()
	agentID := s.agentID
	s.mu.RUnlock()

	sess.mu.RLock()
	defer sess.mu.RUnlock()

	start := time.Now()
	path := s.filePath(sess.ID)
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	defer f.Close()

	encoder := json.NewEncoder(f)

	// Write metadata
	metadata := map[string]any{
		"_type":          "metadata",
		"id":             sess.ID,
		"created_at":     sess.CreatedAt.Format(time.RFC3339),
		"updated_at":     sess.UpdatedAt.Format(time.RFC3339),
		"summary":        sess.Summary,
		"memory_flushed": sess.MemoryFlushed,
	}
	if len(sess.Metadata) > 0 {
		metadata["metadata"] = sess.Metadata
	}
	if err := encoder.Encode(metadata); err != nil {
		return fmt.Errorf("encode metadata: %w", err)
	}

	// Write messages
	for _, msg := range sess.Messages {
		if err := encoder.Encode(msg); err != nil {
			return fmt.Errorf("encode message: %w", err)
		}
	}

	writeElapsed := time.Since(start)
	s.statsMu.Lock()
	s.saveCount++
	s.statsMu.Unlock()
	slog.Debug("session saved",
		"session_id", sess.ID,
		"messages", len(sess.Messages),
		"jsonl_ms", writeElapsed.Milliseconds(),
	)

	// Async, coalesced snapshot for the external store (display/portal). The
	// agent's LLM context is served by the JSONL file above, never this sync.
	snap := SessionSnapshot{
		SessionID:   sess.ID,
		AgentID:     agentID,
		Messages:    append([]provider.Message(nil), sess.Messages...),
		Summary:     sess.Summary,
		Meta:        copyMeta(sess.Metadata),
		TurnRunning: sess.turnRunning,
	}
	s.enqueueSnapshot(snap)

	return nil
}

func copyMeta(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]string, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}
