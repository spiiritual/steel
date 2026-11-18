// Package schedule runs things at set times: reminders sent as they are,
// and prompts the model works on. Schedules are saved, so they survive
// restarts; each run fires at most once, and runs missed while steel was
// down happen late if they're still useful.
//
// It knows nothing about channels or the agent: a Runner carries runs out
// in sessions, and Places, if the channel has them, give each recurring
// schedule a place of its own, like a Telegram topic.
package schedule

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/spiiritual/steel/internal/datafile"
)

// MaxSchedules caps how many schedules exist at once.
const MaxSchedules = 50

// ErrBusy is what Runner.Start returns while the session has work.
var ErrBusy = errors.New("session busy")

// Runner carries runs out in sessions, named by their IDs.
type Runner interface {
	// Post shows text to the user in session id's place and adds note to
	// what its model reads next, without starting a turn.
	Post(id, text, note string)
	// Prompt adds text to session id's conversation as a message to the
	// model and waits for the turn that reads it, returning its last reply.
	Prompt(ctx context.Context, id, text string) (string, error)
	// Start begins a new conversation in session id with notes and text,
	// and waits for its turn like Prompt. It returns ErrBusy, without
	// changing anything, if the session has work.
	Start(ctx context.Context, id, text string, notes []string) (string, error)
	// Quiet reports whether session id has no work and has had none for d.
	Quiet(id string, d time.Duration) bool
}

// Places give recurring schedules places of their own, e.g. Telegram
// topics, each with its own session.
type Places interface {
	// CreatePlace makes a place titled title beside session near's and
	// returns its session ID.
	CreatePlace(ctx context.Context, near, title string) (string, error)
	// EnsurePlace checks place id still exists, making a new one titled
	// title if it doesn't, and returns the session ID to use.
	EnsurePlace(ctx context.Context, id, title string) (string, error)
	// DeletePlace removes a place made by CreatePlace.
	DeletePlace(ctx context.Context, id string) error
}

// Schedule is one saved schedule.
type Schedule struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	When when   `json:"when"`
	// Exactly one of Message (sent as it is) and Prompt (for the model).
	Message string `json:"message,omitempty"`
	Prompt  string `json:"prompt,omitempty"`
	// From is the session that made the schedule; Place is where runs
	// happen: From, or a place of the schedule's own.
	From  string    `json:"from"`
	Place string    `json:"place"`
	Next  time.Time `json:"next"`
	// Running is the due time of a run that started and hasn't finished.
	// Found on startup, the run was cut short by a restart; at most once
	// means it isn't run again.
	Running time.Time `json:"running,omitzero"`
	// Skipped counts runs skipped since the last one, told to the next.
	Skipped int      `json:"skipped,omitempty"`
	Last    *outcome `json:"last,omitempty"`
}

type outcome struct {
	Due   time.Time `json:"due"`
	Reply string    `json:"reply,omitempty"`
	Err   string    `json:"error,omitempty"`
}

func (s *Schedule) ownPlace() bool { return s.Place != s.From }

// Options configure a Scheduler.
type Options struct {
	Path   string // the saved schedules; "" keeps them in memory
	Places Places // nil: every schedule runs where it was made
	Logf   func(format string, args ...any)
}

// Scheduler fires schedules.
type Scheduler struct {
	o      Options
	runner Runner // set by Start

	// Tunables; tests shorten them.
	now      func() time.Time
	check    time.Duration // longest sleep between checks of the wall clock
	quiet    time.Duration // how long a place must be quiet before a fresh run
	poll     time.Duration // how often a waiting run looks again
	retryMin time.Duration // first wait before retrying a failed run

	// firing ends at Stop: nothing new fires, and waiting runs give up.
	// ctx ends at Close: runs stop waiting for their turns.
	firing, ctx       context.Context
	stopFiring, close context.CancelFunc
	wake              chan struct{}
	wg                sync.WaitGroup

	mu        sync.Mutex
	list      []*Schedule
	saveMu    sync.Mutex
	newerFile bool // the file came from a newer steel: never overwrite it
}

const fileVersion = 1

type file struct {
	Schedules []*Schedule `json:"schedules"`
}

// Open loads the saved schedules. Call Start to begin firing them.
func Open(o Options) (*Scheduler, error) {
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	s := &Scheduler{
		o:        o,
		now:      time.Now,
		check:    30 * time.Second, // timers stop while a Mac sleeps; the wall clock doesn't
		quiet:    5 * time.Minute,
		poll:     5 * time.Second,
		retryMin: time.Minute,
		wake:     make(chan struct{}, 1),
	}
	s.ctx, s.close = context.WithCancel(context.Background())
	s.firing, s.stopFiring = context.WithCancel(s.ctx)
	if o.Path != "" {
		var f file
		_, err := datafile.Read(o.Path, fileVersion, &f)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case errors.Is(err, datafile.ErrNewer):
			// Run none rather than lose what the newer steel saved.
			s.newerFile = true
			o.Logf("not running schedules: %v", err)
		case err != nil:
			return nil, fmt.Errorf("schedules: %w", err)
		}
		s.list = f.Schedules
	}
	return s, nil
}

// Start reports runs a restart cut short, then fires schedules with r
// until Stop.
func (s *Scheduler) Start(r Runner) {
	s.runner = r
	s.mu.Lock()
	var interrupted []Schedule
	for _, sc := range s.list {
		if !sc.Running.IsZero() {
			sc.Last = &outcome{Due: sc.Running, Err: "cut short by a restart"}
			sc.Running = time.Time{}
			interrupted = append(interrupted, *sc)
		}
	}
	s.list = slices.DeleteFunc(s.list, func(sc *Schedule) bool { return sc.When.once() && sc.Next.IsZero() })
	s.mu.Unlock()
	for _, sc := range interrupted {
		s.runner.Post(sc.Place,
			fmt.Sprintf("⏰ %s: the run due %s was cut short by a restart; it isn't run again.", sc.Name, sc.When.format(sc.Last.Due)),
			fmt.Sprintf("The scheduled run of %q due %s was cut short by a restart and won't be retried.", sc.Name, sc.When.format(sc.Last.Due)))
	}
	if len(interrupted) > 0 {
		s.save()
	}
	s.wg.Add(1)
	go s.loop()
}

// Stop stops firing: nothing new starts and runs still waiting to start
// give up. Runs already started finish; call it before draining sessions.
func (s *Scheduler) Stop() { s.stopFiring() }

// Close stops firing and waits for the scheduler to finish. Runs still
// waiting for their turn are left marked running, so a restart reports
// them; call it after the sessions are closed.
func (s *Scheduler) Close() {
	s.close()
	s.wg.Wait()
}

func (s *Scheduler) loop() {
	defer s.wg.Done()
	for {
		now := s.now()
		s.fireDue(now)
		wait := s.check
		s.mu.Lock()
		for _, sc := range s.list {
			if !sc.Next.IsZero() && sc.Running.IsZero() {
				wait = min(wait, max(sc.Next.Sub(now), 0))
			}
		}
		s.mu.Unlock()
		t := time.NewTimer(wait)
		select {
		case <-s.firing.Done():
			t.Stop()
			return
		case <-s.wake:
		case <-t.C:
		}
		t.Stop()
	}
}

func (s *Scheduler) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// fireDue starts every run due at now.
func (s *Scheduler) fireDue(now time.Time) {
	if s.firing.Err() != nil {
		return
	}
	type start struct {
		sc  Schedule
		due time.Time
	}
	type skip struct {
		sc    Schedule
		due   time.Time
		count int
	}
	var starts []start
	var skips []skip
	changed := false
	s.mu.Lock()
	for _, sc := range s.list {
		if sc.Next.IsZero() || sc.Next.After(now) {
			continue
		}
		changed = true
		// Only the latest run that's due can still happen: earlier ones
		// were missed while steel was down.
		first, due, missed := sc.Next, sc.Next, 0
		following, _ := sc.When.next(due)
		for !following.IsZero() && !following.After(now) {
			missed++
			due = following
			following, _ = sc.When.next(following)
		}
		sc.Next = following // zero for a one-off
		tooLate := now.Sub(due) > sc.When.grace(due)
		if tooLate {
			missed++
		}
		if missed > 0 {
			sc.Skipped += missed
			skips = append(skips, skip{*sc, first, missed})
		}
		if tooLate {
			continue
		}
		if !sc.Running.IsZero() { // the run before is still going
			sc.Skipped++
			continue
		}
		sc.Running = due
		starts = append(starts, start{*sc, due})
		sc.Skipped = 0 // this run reports them
	}
	s.mu.Unlock()
	if !changed {
		return
	}
	// Saved before anything runs: a crash now loses a run rather than
	// running it twice.
	s.save()
	for _, k := range skips {
		what := "the run due " + k.sc.When.format(k.due)
		if k.count > 1 {
			what = fmt.Sprintf("%d runs, the first due %s,", k.count, k.sc.When.format(k.due))
		}
		s.runner.Post(k.sc.Place, fmt.Sprintf("⏰ %s: skipped %s; steel wasn't running then.", k.sc.Name, what), "")
	}
	for _, st := range starts {
		s.wg.Go(func() { s.run(st.sc, st.due) })
	}
}

// run carries out one run of sc and records how it went.
func (s *Scheduler) run(sc Schedule, due time.Time) {
	late := ""
	if s.now().Sub(due) > 2*time.Minute {
		late = fmt.Sprintf(" (due %s; late, as steel wasn't running then)", sc.When.format(due))
	}
	if sc.ownPlace() {
		sc.Place = s.ensurePlace(sc)
	}
	if sc.Message != "" {
		s.runner.Post(sc.Place, "⏰ "+sc.Message+late,
			fmt.Sprintf("Scheduled message %q was sent to the user: %s", sc.Name, sc.Message))
		s.finish(sc.ID, due, outcome{Due: due})
		return
	}
	text := fmt.Sprintf("[scheduled: %q, due %s%s]\n%s", sc.Name, sc.When.format(due), late, sc.Prompt)
	var reply string
	var err error
	if sc.ownPlace() {
		reply, err = s.fresh(sc, due, text)
	} else {
		// In someone's conversation, the run joins it like any message.
		// It isn't retried: a failure shows there, and resending would
		// repeat the prompt.
		reply, err = s.runner.Prompt(s.ctx, sc.Place, text)
	}
	if err != nil && s.firing.Err() != nil {
		return // cut short by stopping: left marked running, so a restart reports it
	}
	s.finish(sc.ID, due, result(due, reply, err))
}

// maxReply caps the reply kept for the next run, which reads it.
const maxReply = 4000

func result(due time.Time, reply string, err error) outcome {
	if len(reply) > maxReply {
		reply = strings.ToValidUTF8(reply[:maxReply], "") + "…"
	}
	o := outcome{Due: due, Reply: reply}
	if err != nil {
		o.Err = err.Error()
	}
	return o
}

// fresh runs sc in a new conversation in its own place, once the place
// has been quiet for a while (or is merely idle, once the run would
// otherwise be too late), retrying failures while the run is still useful.
func (s *Scheduler) fresh(sc Schedule, due time.Time, text string) (string, error) {
	deadline := due.Add(sc.When.grace(due))
	place := sc.Place
	var notes []string
	if l := sc.Last; l != nil {
		switch {
		case l.Err != "":
			notes = append(notes, fmt.Sprintf("The previous run (due %s) failed: %s", sc.When.format(l.Due), l.Err))
		case l.Reply != "":
			notes = append(notes, fmt.Sprintf("The previous run (due %s) ended with this reply:\n%s", sc.When.format(l.Due), l.Reply))
		}
	}
	if sc.Skipped > 0 {
		notes = append(notes, fmt.Sprintf("%d earlier runs were skipped: steel wasn't running, or the run before was still going.", sc.Skipped))
	}
	backoff := s.retryMin
	for {
		if s.waitQuiet(place, deadline) != nil {
			return "", errors.New("steel stopped before the run could start")
		}
		if !s.exists(sc.ID) {
			return "", errors.New("cancelled before it started")
		}
		reply, err := s.runner.Start(s.ctx, place, text, notes)
		switch {
		case errors.Is(err, ErrBusy): // someone got there first
			// Wait before trying again rather than spin.
			if err := sleep(s.firing, s.poll); err != nil {
				return "", errors.New("steel stopped before the run could start")
			}
			continue
		case err == nil, s.ctx.Err() != nil:
			return reply, err
		case s.now().Add(backoff).After(deadline):
			return reply, err
		}
		s.o.Logf("schedule %q: run failed, retrying in %s: %v", sc.Name, backoff, err)
		if err := sleep(s.firing, backoff); err != nil {
			return "", fmt.Errorf("not retried, steel is stopping: %w", err)
		}
		backoff *= 2
	}
}

// ensurePlace returns sc's place, made again if the user deleted it.
func (s *Scheduler) ensurePlace(sc Schedule) string {
	if s.o.Places == nil {
		return sc.Place
	}
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()
	place, err := s.o.Places.EnsurePlace(ctx, sc.Place, sc.Name)
	if err != nil {
		s.o.Logf("schedule %q: checking its place: %v", sc.Name, err)
		return sc.Place
	}
	if place != sc.Place {
		s.mu.Lock()
		if cur := s.find(sc.ID); cur != nil {
			cur.Place = place
		}
		s.mu.Unlock()
		s.save()
	}
	return place
}

// waitQuiet waits until place has been quiet for s.quiet, or, past
// deadline, until it's idle at all.
func (s *Scheduler) waitQuiet(place string, deadline time.Time) error {
	for {
		if s.runner.Quiet(place, s.quiet) || (!s.now().Before(deadline) && s.runner.Quiet(place, 0)) {
			return nil
		}
		if err := sleep(s.firing, s.poll); err != nil {
			return err
		}
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// finish records a run's outcome; a one-off is then done.
func (s *Scheduler) finish(id string, due time.Time, o outcome) {
	s.mu.Lock()
	if sc := s.find(id); sc != nil && sc.Running.Equal(due) {
		sc.Running, sc.Last = time.Time{}, &o
		if sc.When.once() {
			s.list = slices.DeleteFunc(s.list, func(x *Schedule) bool { return x.ID == id })
		}
	}
	s.mu.Unlock()
	s.save()
	s.poke() // the next run may be waiting on this one
}

// exists reports whether schedule id hasn't been cancelled.
func (s *Scheduler) exists(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.find(id) != nil
}

// find returns schedule id, or nil. Callers hold s.mu.
func (s *Scheduler) find(id string) *Schedule {
	i := slices.IndexFunc(s.list, func(sc *Schedule) bool { return sc.ID == id })
	if i < 0 {
		return nil
	}
	return s.list[i]
}

// save writes the schedules. A failure is logged and doesn't stop runs:
// a reminder that never fires is worse than one that might fire twice.
func (s *Scheduler) save() {
	if s.o.Path == "" || s.newerFile {
		return
	}
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	s.mu.Lock()
	f := file{Schedules: make([]*Schedule, len(s.list))}
	for i, sc := range s.list {
		c := *sc
		f.Schedules[i] = &c
	}
	s.mu.Unlock()
	if err := datafile.Write(s.o.Path, fileVersion, f); err != nil {
		s.o.Logf("saving schedules: %v", err)
	}
}

// AddArgs describe a new schedule. The fields match schedule.add's
// arguments, which convert to it.
type AddArgs struct {
	Name                      string
	At, In, Every, Cron, Zone string
	Message, Prompt           string
}

// Add makes a schedule for session from. A recurring one gets a place of
// its own if Places can make one.
func (s *Scheduler) Add(ctx context.Context, from string, a AddArgs) (Schedule, error) {
	name := strings.Join(strings.Fields(a.Name), " ")
	switch {
	case name == "":
		return Schedule{}, errors.New("give the schedule a short name")
	case len(name) > 64:
		return Schedule{}, errors.New("the name must be at most 64 bytes")
	case (strings.TrimSpace(a.Message) == "") == (strings.TrimSpace(a.Prompt) == ""):
		return Schedule{}, errors.New(`give exactly one of "message" (sent as it is) or "prompt" (for you to work on)`)
	}
	w, err := timing{At: a.At, In: a.In, Every: a.Every, Cron: a.Cron, Zone: a.Zone}.parse(s.now())
	if err != nil {
		return Schedule{}, err
	}
	next, _ := w.next(s.now())
	sc := Schedule{ID: newID(), Name: name, When: w, Message: strings.TrimSpace(a.Message), Prompt: strings.TrimSpace(a.Prompt),
		From: from, Place: from, Next: next}
	if s.newerFile {
		return Schedule{}, errors.New("schedules are off: they were saved by a newer steel, so new ones couldn't be saved")
	}
	s.mu.Lock()
	full := len(s.list) >= MaxSchedules
	s.mu.Unlock()
	if full {
		return Schedule{}, fmt.Errorf("there are already %d schedules, the most allowed; cancel one first", MaxSchedules)
	}
	if !w.once() && s.o.Places != nil {
		place, err := s.o.Places.CreatePlace(ctx, from, name)
		if err != nil {
			s.o.Logf("schedule %q: no place of its own, so it runs where it was made: %v", name, err)
		} else {
			sc.Place = place
		}
	}
	s.mu.Lock()
	s.list = append(s.list, &sc)
	s.mu.Unlock()
	s.save()
	s.poke()
	return sc, nil
}

// Cancel deletes schedule id, and its place if it has one.
func (s *Scheduler) Cancel(ctx context.Context, id string) (Schedule, error) {
	s.mu.Lock()
	sc := s.find(id)
	if sc == nil {
		s.mu.Unlock()
		return Schedule{}, fmt.Errorf("no schedule %q", id)
	}
	gone := *sc
	s.list = slices.DeleteFunc(s.list, func(x *Schedule) bool { return x.ID == id })
	s.mu.Unlock()
	s.save()
	if gone.ownPlace() && s.o.Places != nil {
		if err := s.o.Places.DeletePlace(ctx, gone.Place); err != nil {
			s.o.Logf("schedule %q: deleting its place: %v", gone.Name, err)
		}
	}
	return gone, nil
}

// List returns the schedules, soonest first.
func (s *Scheduler) List() []Schedule {
	s.mu.Lock()
	out := make([]Schedule, len(s.list))
	for i, sc := range s.list {
		out[i] = *sc
	}
	s.mu.Unlock()
	slices.SortStableFunc(out, func(a, b Schedule) int { return a.Next.Compare(b.Next) })
	return out
}

// Upcoming returns sc's next n run times.
func (sc Schedule) Upcoming(n int) []time.Time {
	var out []time.Time
	t := sc.Next
	for !t.IsZero() && len(out) < n {
		out = append(out, t)
		t, _ = sc.When.next(t)
	}
	return out
}

// Describe is a one-line summary for people.
func (sc Schedule) Describe() string {
	what := sc.Prompt
	if sc.Message != "" {
		what = "message: " + sc.Message
	}
	if r := []rune(what); len(r) > 80 {
		what = string(r[:77]) + "…"
	}
	when := sc.When.String()
	switch {
	case sc.When.once(): // the time is the next run
	case sc.Next.IsZero():
		when += ", not due again"
	default:
		when += ", next " + sc.When.format(sc.Next)
	}
	return fmt.Sprintf("%s — %s — %s", sc.Name, when, what)
}

func newID() string {
	var b [3]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
