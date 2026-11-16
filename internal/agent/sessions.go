package agent

import (
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

// Send routes text from chat id to its session. After Close it does nothing.
func (ss *Sessions) Send(id, text string) {
	if s := ss.get(id); s != nil {
		s.Send(text)
	}
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
