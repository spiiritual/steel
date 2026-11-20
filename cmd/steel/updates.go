package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sync"
	"time"

	"github.com/spiiritual/steel/internal/agent"
	"github.com/spiiritual/steel/internal/channel/telegram"
	"github.com/spiiritual/steel/internal/datafile"
	"github.com/spiiritual/steel/internal/update"
	"github.com/spiiritual/steel/internal/updater"
)

// updates connects Telegram's /update and /rollback to the updater
// sidecar, and tells the user how each went. The updater replaces steel's
// container, so the steel that started an update usually isn't the one
// that learns how it went: what's pending is saved, and whichever steel
// sees the updater finish reports it.
type updates struct {
	client   *updater.Client
	path     string // update.json
	announce func(telegram.UpdateStatus)
	emit     func(id string, e agent.Event)
	logf     func(format string, args ...any)

	// Tunables; tests shorten them.
	poll       time.Duration // how often to ask whether a pending action finished
	giveUp     time.Duration // how long to wait for it
	checkFirst time.Duration // first release check after start
	checkEvery time.Duration // release checks after that

	mu        sync.Mutex
	reporting bool
}

// updateState is update.json.
type updateState struct {
	// Announced is the newest release the user has been told about.
	Announced string         `json:"announced,omitempty"`
	Pending   *pendingUpdate `json:"pending,omitempty"`
}

// pendingUpdate is an update or rollback started and not yet reported.
type pendingUpdate struct {
	Action string    `json:"action"`
	From   string    `json:"from"` // the session to report to
	At     time.Time `json:"at"`
}

const updateStateVersion = 1

func newUpdates(url, path string, bot *telegram.Bot, logf func(string, ...any)) *updates {
	return &updates{
		client:     &updater.Client{URL: url},
		path:       path,
		announce:   bot.AnnounceUpdate,
		emit:       bot.Emit,
		logf:       logf,
		poll:       5 * time.Second,
		giveUp:     30 * time.Minute,
		checkFirst: time.Minute,
		checkEvery: 12 * time.Hour,
	}
}

// load reads update.json; a missing file is an empty state. Callers hold u.mu.
func (u *updates) load() (updateState, error) {
	var s updateState
	_, err := datafile.Read(u.path, updateStateVersion, &s)
	if errors.Is(err, fs.ErrNotExist) {
		err = nil
	}
	return s, err
}

// change applies f to the saved state. Callers hold u.mu.
func (u *updates) change(f func(*updateState)) error {
	s, err := u.load()
	if err != nil {
		return err
	}
	f(&s)
	return datafile.Write(u.path, updateStateVersion, s)
}

func (u *updates) Status(ctx context.Context) (telegram.UpdateStatus, error) {
	s, err := u.client.Status(ctx)
	if err != nil {
		return telegram.UpdateStatus{}, err
	}
	return telegram.UpdateStatus{
		Current:     s.Current,
		Latest:      s.Latest,
		LatestError: s.LatestError,
		Available:   s.Latest != "" && update.Newer(s.Current, s.Latest),
		Previous:    s.Previous,
		Busy:        s.Busy,
	}, nil
}

func (u *updates) Update(ctx context.Context, from string) (string, error) {
	return u.start(ctx, "update", from)
}

func (u *updates) Rollback(ctx context.Context, from string) (string, error) {
	return u.start(ctx, "rollback", from)
}

// start asks the updater for action, saving it as pending first: the
// updater may stop this steel before the reply arrives.
func (u *updates) start(ctx context.Context, action, from string) (string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	s, err := u.load()
	if err != nil {
		return "", err
	}
	before := s.Pending
	if err := u.change(func(s *updateState) { s.Pending = &pendingUpdate{Action: action, From: from, At: time.Now()} }); err != nil {
		return "", err
	}
	r, err := u.client.Start(ctx, action)
	if err != nil {
		// Whatever was pending before still is: a busy updater is working
		// on it.
		if serr := u.change(func(s *updateState) { s.Pending = before }); serr != nil {
			u.logf("updates: %v", serr)
		}
	}
	switch {
	case errors.Is(err, updater.ErrUpToDate):
		return fmt.Sprintf("steel %s is already the newest release.", r.From), nil
	case errors.Is(err, updater.ErrBusy):
		return "An update is already under way; I'll say how it went where it was started.", nil
	case err != nil:
		return "", err
	}
	go u.report(context.Background())
	verb := "Updating"
	if action == "rollback" {
		verb = "Rolling back"
	}
	return fmt.Sprintf("%s from %s to %s. I'll finish what I'm doing, restart, and tell you here how it went.", verb, r.From, r.To), nil
}

// run reports a pending action left by an earlier steel, then tells the
// user about new releases, until ctx ends.
func (u *updates) run(ctx context.Context) {
	go u.report(ctx)
	wait := u.checkFirst
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = u.checkEvery
		if err := u.announceNew(ctx); err != nil && ctx.Err() == nil {
			u.logf("updates: checking for a new release: %v", err)
		}
	}
}

// announceNew tells the user about a release once, unless an update is
// already pending.
func (u *updates) announceNew(ctx context.Context) error {
	st, err := u.Status(ctx)
	if err != nil || !st.Available || st.Busy {
		return err
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	s, err := u.load()
	if err != nil || s.Pending != nil || s.Announced == st.Latest {
		return err
	}
	if err := u.change(func(s *updateState) { s.Announced = st.Latest }); err != nil {
		return err
	}
	u.announce(st)
	return nil
}

// report waits for the pending action, if any, to finish and tells the
// session that started it how it went. One runs at a time.
func (u *updates) report(ctx context.Context) {
	u.mu.Lock()
	if u.reporting {
		u.mu.Unlock()
		return
	}
	u.reporting = true
	u.mu.Unlock()
	defer func() {
		u.mu.Lock()
		u.reporting = false
		u.mu.Unlock()
	}()

	deadline := time.Now().Add(u.giveUp)
	for {
		u.mu.Lock()
		s, err := u.load()
		u.mu.Unlock()
		if err != nil {
			u.logf("updates: %v", err)
			return
		}
		p := s.Pending
		if p == nil {
			return
		}
		st, err := u.client.Status(ctx)
		text := ""
		switch {
		case err == nil && !st.Busy:
			text = outcome(*p, st)
		case time.Now().After(deadline):
			text = fmt.Sprintf("I couldn't find out how the %s went. /update shows which version is running.", p.Action)
		}
		if text != "" {
			u.emit(p.From, agent.Notice{Text: text})
			u.mu.Lock()
			err := u.change(func(s *updateState) { s.Pending = nil })
			u.mu.Unlock()
			if err != nil {
				u.logf("updates: %v", err)
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(u.poll):
		}
	}
}

// outcome says how pending p went, given the updater's idle status.
func outcome(p pendingUpdate, st updater.Status) string {
	r := st.Last
	// Started by this request; a minute's slack for clocks in different
	// containers.
	if r == nil || r.Action != p.Action || r.Started.Before(p.At.Add(-time.Minute)) {
		return fmt.Sprintf("I'm back, on steel %s; the updater restarted meanwhile, so I can't say how the %s went.", st.Current, p.Action)
	}
	switch {
	case r.Error == "" && r.Action == "rollback":
		return fmt.Sprintf("Rolled back to steel %s (from %s). /update offers the newest again.", r.To, r.From)
	case r.Error == "":
		return fmt.Sprintf("Updated to steel %s (from %s). /rollback goes back if something's off.", r.To, r.From)
	case r.Restored:
		return fmt.Sprintf("steel %s didn't work, so I'm back on %s. %s", r.To, r.From, r.Error)
	}
	return fmt.Sprintf("The %s to %s failed, so I'm still on %s: %s", r.Action, r.To, st.Current, r.Error)
}
