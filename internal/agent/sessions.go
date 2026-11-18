package agent

import (
	"context"
	"maps"
	"os"
	"slices"
	"sync"
	"time"
)

// Sessions maps chat IDs to sessions, creating them on first use and
// closing idle sessions' toolboxes.
type Sessions struct {
	cfg  Config
	emit func(id string, e Event)

	// inputMu orders user input against Drain: Send holds it shared from
	// checking draining until the session has the input, so once Drain
	// holds it exclusively and sets draining, no input slips past.
	inputMu  sync.RWMutex
	draining bool // Drain started: user input is ignored

	mu        sync.Mutex
	m         map[string]*Session
	closed    bool
	done      chan struct{}
	closeOnce sync.Once
}

// NewSessions returns a session manager. emit receives every session's
// events tagged with its ID and must be safe for concurrent use.
func NewSessions(cfg Config, emit func(id string, e Event)) (*Sessions, error) {
	if cfg.Dir != "" {
		if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
			return nil, err
		}
	}
	ss := &Sessions{cfg: cfg, emit: emit, m: map[string]*Session{}, done: make(chan struct{})}
	if cfg.IdleTimeout > 0 {
		go ss.janitor()
	}
	return ss, nil
}

// Send routes text from chat id to its session. After Drain or Close it
// does nothing.
func (ss *Sessions) Send(id, text string) {
	ss.inputMu.RLock()
	defer ss.inputMu.RUnlock()
	if ss.draining {
		return
	}
	if s := ss.get(id); s != nil {
		s.Send(text)
	}
}

// Answer passes the user's answer to an ApprovalRequest from session id,
// reporting whether the request was still waiting for one.
func (ss *Sessions) Answer(id, approvalID string, allow bool) bool {
	ss.mu.Lock()
	s, ok := ss.m[id]
	ss.mu.Unlock()
	return ok && s.answer(approvalID, allow)
}

// get returns id's session, creating it if needed; nil after Close.
func (ss *Sessions) get(id string) *Session {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.closed {
		return nil
	}
	s, ok := ss.m[id]
	if !ok {
		s = newSession(id, &ss.cfg, func(e Event) { ss.emit(id, e) })
		ss.m[id] = s
	}
	return s
}

// Idle reports whether session id has no running turn, no queued input, and
// no pending background work. Unknown sessions are idle.
func (ss *Sessions) Idle(id string) bool {
	ss.mu.Lock()
	s, ok := ss.m[id]
	ss.mu.Unlock()
	if !ok {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.running && len(s.inbox) == 0 && (s.box == nil || !s.box.Busy())
}

func (ss *Sessions) janitor() {
	t := time.NewTicker(max(ss.cfg.IdleTimeout/4, 10*time.Millisecond))
	defer t.Stop()
	for {
		select {
		case <-ss.done:
			return
		case <-t.C:
			ss.mu.Lock()
			all := make([]*Session, 0, len(ss.m))
			for _, s := range ss.m {
				all = append(all, s)
			}
			ss.mu.Unlock()
			for _, s := range all {
				s.evictIfIdle(ss.cfg.IdleTimeout)
			}
		}
	}
}

// Drain prepares for shutdown: it stops taking user input and waits until
// every session is idle, with no running turn, queued input, or background
// work. Background results still reach the model meanwhile. If ctx ends
// first it returns ctx's error; Close then cancels whatever is left.
func (ss *Sessions) Drain(ctx context.Context) error {
	ss.inputMu.Lock()
	ss.draining = true
	ss.inputMu.Unlock()
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		ss.mu.Lock()
		ids := slices.Collect(maps.Keys(ss.m))
		ss.mu.Unlock()
		if !slices.ContainsFunc(ids, func(id string) bool { return !ss.Idle(id) }) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// Close stops all sessions, waits for their running turns to finish and
// save, and closes their toolboxes. Later input is ignored. It is safe to
// call twice.
func (ss *Sessions) Close() {
	ss.closeOnce.Do(func() {
		close(ss.done)
		ss.mu.Lock()
		ss.closed = true
		all := slices.Collect(maps.Values(ss.m))
		ss.mu.Unlock()
		for _, s := range all {
			s.close()
		}
	})
}
