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

	// inputMu orders user input against Drain and retiring: Send holds it
	// shared from checking draining until the session has the input, so
	// once Drain holds it exclusively and sets draining, no input slips
	// past, and the janitor holding it can retire a session that no Send
	// is about to use.
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

// Post shows text in session id's place and, if note isn't empty, adds
// it to what the model reads next, without starting a turn.
func (ss *Sessions) Post(id, text, note string) {
	ss.inputMu.RLock()
	defer ss.inputMu.RUnlock()
	if s := ss.get(id); s != nil {
		s.emit(Notice{Text: text})
		if note != "" {
			s.note(note)
		}
	}
}

// Prompt gives text to session id's model like a user message, and waits
// until the turn that reads it ends or ctx does, returning its last reply.
func (ss *Sessions) Prompt(ctx context.Context, id, text string) (string, error) {
	return ss.await(ctx, id, func(s *Session) (<-chan TurnResult, error) { return s.prompt(text) })
}

// Start begins a new conversation in session id with notes and text, and
// waits like Prompt. If the session has work it changes nothing and
// returns ErrBusy.
func (ss *Sessions) Start(ctx context.Context, id, text string, notes []string) (string, error) {
	return ss.await(ctx, id, func(s *Session) (<-chan TurnResult, error) { return s.start(text, notes) })
}

// await queues input with queue and waits for the turn that reads it.
// Like user input, it's refused once Drain starts.
func (ss *Sessions) await(ctx context.Context, id string, queue func(*Session) (<-chan TurnResult, error)) (string, error) {
	ss.inputMu.RLock()
	var ch <-chan TurnResult
	var err error
	if s := ss.get(id); s == nil || ss.draining {
		err = errDropped
	} else {
		ch, err = queue(s)
	}
	ss.inputMu.RUnlock()
	if err != nil {
		return "", err
	}
	select {
	case r := <-ch:
		return r.Reply, r.Err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// Quiet reports whether session id is idle and has been for d.
func (ss *Sessions) Quiet(id string, d time.Duration) bool {
	ss.mu.Lock()
	s, ok := ss.m[id]
	ss.mu.Unlock()
	if !ok {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.lastUsed) >= d && !s.busyLocked()
}

// Answer passes the user's answer to an ApprovalRequest from session id,
// reporting whether the request was still waiting for one.
func (ss *Sessions) Answer(id, approvalID string, allow bool) bool {
	ss.mu.Lock()
	s, ok := ss.m[id]
	ss.mu.Unlock()
	return ok && s.answer(approvalID, allow)
}

// ProvideSecret passes the value the user entered for a SecretRequest from
// session id, reporting whether the request was still waiting for it.
func (ss *Sessions) ProvideSecret(id, requestID, value string) bool {
	return ss.answerSecret(id, requestID, secretAnswer{value: value, ok: true})
}

// CancelSecret declines a SecretRequest from session id, reporting whether
// it was still waiting.
func (ss *Sessions) CancelSecret(id, requestID string) bool {
	return ss.answerSecret(id, requestID, secretAnswer{})
}

func (ss *Sessions) answerSecret(id, requestID string, a secretAnswer) bool {
	ss.mu.Lock()
	s, ok := ss.m[id]
	ss.mu.Unlock()
	return ok && s.answerSecret(requestID, a)
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

// Idle reports whether session id has no running turn, no queued input, no
// question for the user and no pending background work. Unknown sessions
// are idle.
func (ss *Sessions) Idle(id string) bool {
	ss.mu.Lock()
	s, ok := ss.m[id]
	ss.mu.Unlock()
	if !ok {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.busyLocked()
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
			ss.reflect(all)
			for _, s := range all {
				s.evictIfIdle(ss.cfg.IdleTimeout)
			}
			ss.retireSettled()
		}
	}
}

// reflect starts the reflection of every session due one. Before
// evicting, so the model still has its scratchpad; never while draining,
// which waits for work to end rather than start.
func (ss *Sessions) reflect(all []*Session) {
	ss.inputMu.RLock()
	defer ss.inputMu.RUnlock()
	if ss.draining {
		return
	}
	for _, s := range all {
		s.reflectIfDue(ss.cfg.IdleTimeout)
	}
}

// retireSettled drops settled sessions from the map, freeing their history;
// each is loaded from its file again on its next message. Without this,
// every chat and topic ever used would stay in memory until steel exits.
func (ss *Sessions) retireSettled() {
	// Skip this tick rather than wait behind a Send or Drain.
	if !ss.inputMu.TryLock() {
		return
	}
	defer ss.inputMu.Unlock()
	ss.mu.Lock()
	settled := map[string]*Session{}
	for id, s := range ss.m {
		if s.settled(ss.cfg.IdleTimeout) {
			settled[id] = s
		}
	}
	ss.mu.Unlock()
	// With inputMu held, nothing adds sessions (only Send does) and no
	// input reaches a settled one, which has no scratchpad or approvals to
	// hear from either. So each stays settled while it is saved, outside
	// ss.mu. Usually its file is already current and nothing is written.
	for id, s := range settled {
		if s.retire() {
			ss.mu.Lock()
			delete(ss.m, id)
			ss.mu.Unlock()
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
