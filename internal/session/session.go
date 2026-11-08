package session

import (
	"sync"
	"time"

	"gitee.com/kobeyoung81/nanosec/internal/provider"
)

// Session represents a chat session.
type Session struct {
	ID            string             `json:"id"`
	Summary       string             `json:"summary,omitempty"`
	Metadata      map[string]string  `json:"metadata,omitempty"`
	MemoryFlushed bool               `json:"memory_flushed,omitempty"`
	Messages      []provider.Message `json:"messages"`
	CreatedAt     time.Time          `json:"created_at"`
	UpdatedAt     time.Time          `json:"updated_at"`
	mu            sync.RWMutex
	// turnRunning reports whether a turn is currently being processed. It is
	// mirrored to the bus so the portal can reconcile history and show the
	// running indicator after a client reconnect.
	turnRunning bool
}

// SummaryCountMetaKey is the session metadata key that records the
// conversation count captured at the last successful summarization. It lets
// the summarizer re-run only after real growth instead of on every turn.
const SummaryCountMetaKey = "_summary_count"

// New creates a new session.
func New(id string) *Session {
	now := time.Now()
	return &Session{
		ID:        id,
		Metadata:  make(map[string]string),
		Messages:  make([]provider.Message, 0),
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// SetTurnRunning records whether a turn is in flight for this session.
func (s *Session) SetTurnRunning(running bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.turnRunning = running
	if running {
		s.UpdatedAt = time.Now()
	}
}

// TurnRunning reports whether a turn is currently in flight.
func (s *Session) TurnRunning() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.turnRunning
}

// AddMessage adds a message to the session.
func (s *Session) AddMessage(msg provider.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if msg.Timestamp.IsZero() {
		msg.Timestamp = time.Now()
	}
	s.Messages = append(s.Messages, msg)
	s.UpdatedAt = time.Now()
}

// GetMessages returns a copy of the messages in the session.
func (s *Session) GetMessages() []provider.Message {
	s.mu.RLock()
	defer s.mu.RUnlock()
	msgs := make([]provider.Message, len(s.Messages))
	copy(msgs, s.Messages)
	return msgs
}

// ConversationMessages returns a copy of the messages excluding ephemeral
// "thought", "error" and "compaction" rows — i.e. exactly the sequence that was
// (or will be) sent to the LLM. Those rows are persisted for the web but must
// never reach the provider.
func (s *Session) ConversationMessages() []provider.Message {
	s.mu.RLock()
	defer s.mu.RUnlock()
	msgs := make([]provider.Message, 0, len(s.Messages))
	for _, m := range s.Messages {
		if isEphemeralRole(m.Role) {
			continue
		}
		msgs = append(msgs, m)
	}
	return msgs
}

// MessageCount returns the number of messages in the session.
func (s *Session) MessageCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.Messages)
}

// ConversationCount returns the number of LLM-facing messages (excluding
// ephemeral thought/error/compaction rows), used for summarization thresholds.
func (s *Session) ConversationCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, m := range s.Messages {
		if !isEphemeralRole(m.Role) {
			n++
		}
	}
	return n
}

// isEphemeralRole reports whether a message role is persisted for display only
// and must be excluded from the LLM conversation.
func isEphemeralRole(role string) bool {
	switch role {
	case "thought", "error", "compaction":
		return true
	}
	return false
}

// SetSummary sets the conversation summary.
func (s *Session) SetSummary(summary string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Summary = summary
	s.UpdatedAt = time.Now()
}

// GetSummary returns the conversation summary.
func (s *Session) GetSummary() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Summary
}

// SetMeta sets a metadata key-value pair.
func (s *Session) SetMeta(key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Metadata == nil {
		s.Metadata = make(map[string]string)
	}
	s.Metadata[key] = value
	s.UpdatedAt = time.Now()
}

// GetMeta returns the value for a metadata key.
func (s *Session) GetMeta(key string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Metadata[key]
}

// DeleteMeta removes a metadata key. Removing beats writing an empty value: the
// metadata blob is persisted and synced verbatim, so a cleared key should not
// look like a set-but-empty one.
func (s *Session) DeleteMeta(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.Metadata[key]; !ok {
		return
	}
	delete(s.Metadata, key)
	s.UpdatedAt = time.Now()
}

// GetAllMeta returns a copy of all metadata.
func (s *Session) GetAllMeta() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m := make(map[string]string, len(s.Metadata))
	for k, v := range s.Metadata {
		m[k] = v
	}
	return m
}

// TruncateMessages keeps only the last n messages, discarding older ones.
func (s *Session) TruncateMessages(keepLast int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if keepLast >= len(s.Messages) {
		return
	}
	if keepLast < 0 {
		keepLast = 0
	}
	s.Messages = append([]provider.Message(nil), s.Messages[len(s.Messages)-keepLast:]...)
	s.UpdatedAt = time.Now()
}

// Clear clears all messages from the session.
func (s *Session) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Messages = make([]provider.Message, 0)
	s.Summary = ""
	s.MemoryFlushed = false
	s.turnRunning = false
	// Reset the summarization watermark so a fresh conversation is not
	// suppressed by a stale count from the previous one.
	if s.Metadata != nil {
		delete(s.Metadata, SummaryCountMetaKey)
	}
	s.UpdatedAt = time.Now()
}
