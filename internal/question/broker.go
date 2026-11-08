// Package question implements a process-wide broker that lets a tool block a
// turn while it waits for a user's answer, delivered asynchronously over the
// bus. Answers carry metadata.question_id so the poll loop can route them to
// the waiting turn instead of starting a new turn.
package question

import (
	"context"
	"fmt"
	"sync"
)

// Option is one selectable answer for a question.
type Option struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// Question is a single prompt presented to the user.
type Question struct {
	Question string   `json:"question"`
	Header   string   `json:"header,omitempty"`
	Options  []Option `json:"options,omitempty"`
	Multiple bool     `json:"multiple,omitempty"`
	Custom   bool     `json:"custom,omitempty"`
}

// Request is the payload of a question chunk and the pending-question metadata.
type Request struct {
	ID        string     `json:"id"`
	Questions []Question `json:"questions"`
}

type waiter struct {
	sessionID string
	ch        chan []string
}

// Broker tracks in-flight questions and resolves them when answers arrive.
type Broker struct {
	mu      sync.Mutex
	waiters map[string]*waiter
}

// NewBroker creates an empty Broker.
func NewBroker() *Broker {
	return &Broker{waiters: make(map[string]*waiter)}
}

// Wait registers a waiter for the given question id and blocks until an answer
// is delivered or ctx is cancelled. There is no timeout: the caller decides how
// long to wait.
func (b *Broker) Wait(ctx context.Context, id, sessionID string) ([]string, error) {
	if id == "" {
		return nil, fmt.Errorf("question id is required")
	}
	w := &waiter{sessionID: sessionID, ch: make(chan []string, 1)}

	b.mu.Lock()
	b.waiters[id] = w
	b.mu.Unlock()

	defer func() {
		b.mu.Lock()
		if b.waiters[id] == w {
			delete(b.waiters, id)
		}
		b.mu.Unlock()
	}()

	select {
	case answers := <-w.ch:
		return answers, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Deliver resolves the waiter for id, returning false if none is registered.
func (b *Broker) Deliver(id string, answers []string) bool {
	b.mu.Lock()
	w, ok := b.waiters[id]
	if ok {
		delete(b.waiters, id)
	}
	b.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case w.ch <- answers:
	default:
	}
	return true
}

// IsWaiting reports whether any question is pending for the session.
func (b *Broker) IsWaiting(sessionID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, w := range b.waiters {
		if w.sessionID == sessionID {
			return true
		}
	}
	return false
}

type questionIDKey struct{}

// WithQuestionID attaches the question id to a context so the question tool can
// wait on the same id the agent used when it emitted the chunk.
func WithQuestionID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, questionIDKey{}, id)
}

// QuestionID returns the question id stored on the context, if any.
func QuestionID(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(questionIDKey{}).(string)
	return id, ok && id != ""
}
