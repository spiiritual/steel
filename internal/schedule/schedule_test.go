package schedule

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type post struct{ id, text, note string }

type start struct {
	id, text string
	notes    []string
}

// fakeRunner records what the scheduler asks of sessions.
type fakeRunner struct {
	mu      sync.Mutex
	posts   []post
	prompts []start
	starts  []start
	fails   int             // Start fails this many times first
	busy    map[string]bool // sessions that aren't quiet
	block   chan struct{}   // if set, Start waits for it
	cutErr  error           // Start's error once block closes, as when steel stops a turn
	reply   string
}

func (r *fakeRunner) Post(id, text, note string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.posts = append(r.posts, post{id, text, note})
}

func (r *fakeRunner) Prompt(_ context.Context, id, text string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prompts = append(r.prompts, start{id: id, text: text})
	return r.reply, nil
}

func (r *fakeRunner) Start(ctx context.Context, id, text string, notes []string) (string, error) {
	r.mu.Lock()
	r.starts = append(r.starts, start{id, text, notes})
	block, cutErr := r.block, r.cutErr
	fail := r.fails > 0
	if fail {
		r.fails--
	}
	r.mu.Unlock()
	if block != nil {
		select {
		case <-block:
			if cutErr != nil {
				return "partial", cutErr
			}
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if fail {
		return "", errors.New("API down")
	}
	return r.reply, nil
}

func (r *fakeRunner) Quiet(id string, _ time.Duration) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.busy[id]
}

func (r *fakeRunner) setBusy(id string, busy bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.busy[id] = busy
}

func (r *fakeRunner) snapshot() (posts []post, prompts, starts []start) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]post(nil), r.posts...), append([]start(nil), r.prompts...), append([]start(nil), r.starts...)
}

// fakePlaces makes topics "<near>/<n>".
type fakePlaces struct {
	mu      sync.Mutex
	n       int
	gone    map[string]bool
	deleted []string
	off     bool // the chat has no topics
}

func (p *fakePlaces) CreatePlace(_ context.Context, near, _ string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.off {
		return "", errors.New("topics are off")
	}
	p.n++
	return near + "/" + string(rune('0'+p.n)), nil
}

func (p *fakePlaces) EnsurePlace(ctx context.Context, id, title string) (string, error) {
	p.mu.Lock()
	gone := p.gone[id]
	p.mu.Unlock()
	if gone {
		near, _, _ := strings.Cut(id, "/")
		return p.CreatePlace(ctx, near, title)
	}
	return id, nil
}

func (p *fakePlaces) DeletePlace(_ context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deleted = append(p.deleted, id)
	return nil
}

// clock is a settable time.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

var t0 = time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)

type harness struct {
	s      *Scheduler
	r      *fakeRunner
	places *fakePlaces
	clock  *clock
	path   string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{r: &fakeRunner{busy: map[string]bool{}, reply: "done"}, places: &fakePlaces{gone: map[string]bool{}},
		clock: &clock{t: t0}, path: filepath.Join(t.TempDir(), "schedules.json")}
	h.s = h.open(t)
	return h
}

// open loads the schedules as a fresh steel would, without starting the
// firing loop: tests fire with fire.
func (h *harness) open(t *testing.T) *Scheduler {
	t.Helper()
	s, err := Open(Options{Path: h.path, Places: h.places, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	s.now = h.clock.now
	s.poll, s.retryMin = time.Millisecond, time.Millisecond
	s.runner = h.r
	t.Cleanup(s.Close)
	return s
}

// fire runs whatever is due at t and waits for the runs to finish.
func (h *harness) fire(t time.Time) {
	h.clock.set(t)
	h.s.fireDue(t)
	h.s.wg.Wait()
}

func (h *harness) add(t *testing.T, a AddArgs) Schedule {
	t.Helper()
	sc, err := h.s.Add(context.Background(), "chat", a)
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

func TestOneOffMessageIsSentOnceThenGone(t *testing.T) {
	h := newHarness(t)
	sc := h.add(t, AddArgs{Name: "Call mom", In: "1h", Message: "Call mom"})
	if sc.ownPlace() {
		t.Errorf("one-off got a place of its own: %s", sc.Place)
	}
	h.fire(t0.Add(59 * time.Minute))
	if posts, _, _ := h.r.snapshot(); len(posts) != 0 {
		t.Fatalf("sent early: %v", posts)
	}
	h.fire(t0.Add(time.Hour))
	h.fire(t0.Add(2 * time.Hour))
	posts, _, _ := h.r.snapshot()
	if len(posts) != 1 || posts[0].id != "chat" || posts[0].text != "⏰ Call mom" {
		t.Fatalf("posts = %v", posts)
	}
	if got := h.s.List(); len(got) != 0 {
		t.Errorf("still scheduled: %v", got)
	}
}

func TestLateOneOffSaysSo(t *testing.T) {
	h := newHarness(t)
	h.add(t, AddArgs{Name: "Call mom", In: "1h", Message: "Call mom"})
	h.fire(t0.Add(30 * time.Hour)) // steel was down
	posts, _, _ := h.r.snapshot()
	if len(posts) != 1 || !strings.Contains(posts[0].text, "late") {
		t.Fatalf("posts = %v", posts)
	}
}

func TestOneOffPromptJoinsTheConversation(t *testing.T) {
	h := newHarness(t)
	h.add(t, AddArgs{Name: "Package", In: "1h", Prompt: "Check if the package shipped"})
	h.fire(t0.Add(time.Hour))
	_, prompts, starts := h.r.snapshot()
	if len(starts) != 0 || len(prompts) != 1 || prompts[0].id != "chat" || !strings.Contains(prompts[0].text, "Check if the package shipped") {
		t.Fatalf("prompts = %v, starts = %v", prompts, starts)
	}
}

func TestRecurringRunsFreshInItsTopicWithTheLastReply(t *testing.T) {
	h := newHarness(t)
	sc := h.add(t, AddArgs{Name: "Digest", Every: "1h", Prompt: "Summarize my inbox"})
	if sc.Place != "chat/1" {
		t.Fatalf("place = %q", sc.Place)
	}
	h.fire(t0.Add(time.Hour))
	h.r.reply = "3 new emails"
	h.fire(t0.Add(2 * time.Hour))
	_, _, starts := h.r.snapshot()
	if len(starts) != 2 || starts[0].id != "chat/1" || starts[1].id != "chat/1" {
		t.Fatalf("starts = %v", starts)
	}
	if len(starts[0].notes) != 0 {
		t.Errorf("first run's notes = %v", starts[0].notes)
	}
	if n := starts[1].notes; len(n) != 1 || !strings.Contains(n[0], "done") {
		t.Errorf("second run's notes = %v; want the first run's reply", n)
	}
}

func TestRecurringWithoutTopicsRunsWhereItWasMade(t *testing.T) {
	h := newHarness(t)
	h.places.off = true
	sc := h.add(t, AddArgs{Name: "Digest", Every: "1h", Prompt: "Summarize my inbox"})
	if sc.Place != "chat" {
		t.Fatalf("place = %q", sc.Place)
	}
	h.fire(t0.Add(time.Hour))
	if _, prompts, starts := h.r.snapshot(); len(prompts) != 1 || len(starts) != 0 {
		t.Fatalf("prompts = %v, starts = %v; want it added to the conversation", prompts, starts)
	}
}

func TestMissedRunCatchesUpOnlyWhileUseful(t *testing.T) {
	h := newHarness(t)
	sc := h.add(t, AddArgs{Name: "Digest", Cron: "0 8 * * *", Zone: "UTC", Prompt: "Summarize my inbox"})
	due := time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC)
	if !sc.Next.Equal(due) {
		t.Fatalf("next = %v", sc.Next)
	}
	// Three hours late: still worth doing.
	h.fire(due.Add(3 * time.Hour))
	if _, _, starts := h.r.snapshot(); len(starts) != 1 || !strings.Contains(starts[0].text, "late") {
		t.Fatalf("starts = %v", starts)
	}
	// The next day's, 13 hours late (past half a day), and the one after
	// are skipped, with a notice.
	h.fire(due.Add(2*24*time.Hour + 13*time.Hour))
	posts, _, starts := h.r.snapshot()
	if len(starts) != 1 {
		t.Fatalf("starts = %v", starts)
	}
	if len(posts) != 1 || !strings.Contains(posts[0].text, "skipped 2 runs") {
		t.Fatalf("posts = %v", posts)
	}
	if next := h.s.List()[0].Next; !next.Equal(due.Add(3 * 24 * time.Hour)) {
		t.Errorf("next = %v", next)
	}
	// The next run hears about the skipped ones.
	h.fire(due.Add(3 * 24 * time.Hour))
	_, _, starts = h.r.snapshot()
	if n := starts[1].notes; len(n) != 2 || !strings.Contains(n[1], "2 earlier runs were skipped") {
		t.Errorf("notes = %v", n)
	}
}

func TestRunWaitsForItsTopicToBeQuiet(t *testing.T) {
	h := newHarness(t)
	h.add(t, AddArgs{Name: "Digest", Every: "1h", Prompt: "Summarize my inbox"})
	h.r.setBusy("chat/1", true)
	h.clock.set(t0.Add(time.Hour))
	h.s.fireDue(t0.Add(time.Hour))
	time.Sleep(20 * time.Millisecond)
	if _, _, starts := h.r.snapshot(); len(starts) != 0 {
		t.Fatalf("started in a busy topic: %v", starts)
	}
	h.r.setBusy("chat/1", false)
	h.s.wg.Wait()
	if _, _, starts := h.r.snapshot(); len(starts) != 1 {
		t.Fatalf("starts = %v", starts)
	}
}

func TestFailedRunIsRetriedWhileUseful(t *testing.T) {
	h := newHarness(t)
	h.add(t, AddArgs{Name: "Digest", Every: "1h", Prompt: "Summarize my inbox"})
	h.r.fails = 2
	h.fire(t0.Add(time.Hour))
	_, _, starts := h.r.snapshot()
	if len(starts) != 3 {
		t.Fatalf("%d attempts; want 3", len(starts))
	}
	if l := h.s.List()[0].Last; l == nil || l.Err != "" {
		t.Errorf("last = %+v", l)
	}
}

func TestFailedRunGivesUpPastItsGrace(t *testing.T) {
	h := newHarness(t)
	h.add(t, AddArgs{Name: "Digest", Every: "1h", Prompt: "Summarize my inbox"})
	h.s.retryMin = time.Hour // any retry would be too late
	h.r.fails = 1
	h.fire(t0.Add(time.Hour))
	if _, _, starts := h.r.snapshot(); len(starts) != 1 {
		t.Fatalf("%d attempts; want 1", len(starts))
	}
	if l := h.s.List()[0].Last; l == nil || l.Err != "API down" {
		t.Errorf("last = %+v", l)
	}
}

func TestOverlappingRunIsSkipped(t *testing.T) {
	h := newHarness(t)
	h.add(t, AddArgs{Name: "Digest", Every: "1h", Prompt: "Summarize my inbox"})
	h.r.block = make(chan struct{})
	h.clock.set(t0.Add(time.Hour))
	h.s.fireDue(t0.Add(time.Hour))
	h.clock.set(t0.Add(2 * time.Hour))
	h.s.fireDue(t0.Add(2 * time.Hour)) // the first is still going
	close(h.r.block)
	h.s.wg.Wait()
	_, _, starts := h.r.snapshot()
	if len(starts) != 1 {
		t.Fatalf("starts = %v", starts)
	}
	h.fire(t0.Add(3 * time.Hour))
	_, _, starts = h.r.snapshot()
	if n := starts[1].notes; len(n) != 2 || !strings.Contains(n[1], "1 earlier runs were skipped") {
		t.Errorf("notes = %v", n)
	}
}

func TestRunCutShortByRestartIsReportedNotRepeated(t *testing.T) {
	h := newHarness(t)
	h.add(t, AddArgs{Name: "Digest", Every: "1h", Prompt: "Summarize my inbox"})
	h.r.block = make(chan struct{})
	h.clock.set(t0.Add(time.Hour))
	h.s.fireDue(t0.Add(time.Hour))
	h.s.Close() // steel quits mid-run

	h.r.block = nil
	h.s = h.open(t)
	h.s.Start(h.r)
	h.s.Close()
	posts, _, starts := h.r.snapshot()
	if len(starts) != 1 {
		t.Errorf("starts = %v; want the cut-short run not repeated", starts)
	}
	if len(posts) != 1 || posts[0].id != "chat/1" || !strings.Contains(posts[0].text, "cut short by a restart") {
		t.Errorf("posts = %v", posts)
	}
	if l := h.s.List()[0]; !l.Running.IsZero() || l.Last == nil {
		t.Errorf("after restart: %+v", l)
	}
}

func TestSchedulesSurviveRestart(t *testing.T) {
	h := newHarness(t)
	sc := h.add(t, AddArgs{Name: "Digest", Cron: "0 8 * * 1-5", Zone: "Europe/London", Prompt: "Summarize my inbox"})
	h.s = h.open(t)
	got := h.s.List()
	if len(got) != 1 || got[0].ID != sc.ID || !got[0].Next.Equal(sc.Next) || got[0].Place != sc.Place {
		t.Fatalf("after restart: %+v; want %+v", got, sc)
	}
	if d := got[0].Describe(); !strings.Contains(d, "GMT") {
		t.Errorf("describe = %q; want times in the schedule's zone", d)
	}
}

func TestDeletedTopicIsMadeAgain(t *testing.T) {
	h := newHarness(t)
	h.add(t, AddArgs{Name: "Water", Every: "1h", Message: "Drink water"})
	h.places.gone["chat/1"] = true
	h.fire(t0.Add(time.Hour))
	posts, _, _ := h.r.snapshot()
	if len(posts) != 1 || posts[0].id != "chat/2" {
		t.Fatalf("posts = %v", posts)
	}
	if p := h.s.List()[0].Place; p != "chat/2" {
		t.Errorf("place = %q", p)
	}
}

func TestCancelDeletesTheTopic(t *testing.T) {
	h := newHarness(t)
	sc := h.add(t, AddArgs{Name: "Digest", Every: "1h", Prompt: "Summarize my inbox"})
	if _, err := h.s.Cancel(context.Background(), sc.ID); err != nil {
		t.Fatal(err)
	}
	if len(h.s.List()) != 0 || len(h.places.deleted) != 1 || h.places.deleted[0] != "chat/1" {
		t.Errorf("list = %v, deleted = %v", h.s.List(), h.places.deleted)
	}
	if _, err := h.s.Cancel(context.Background(), sc.ID); err == nil {
		t.Error("cancelled twice")
	}
	h.fire(t0.Add(time.Hour))
	if _, _, starts := h.r.snapshot(); len(starts) != 0 {
		t.Errorf("a cancelled schedule ran: %v", starts)
	}
}

func TestAddChecksItsArguments(t *testing.T) {
	h := newHarness(t)
	for _, a := range []AddArgs{
		{In: "1h", Message: "x"},                         // no name
		{Name: "x", In: "1h"},                            // neither message nor prompt
		{Name: "x", In: "1h", Message: "x", Prompt: "y"}, // both
		{Name: "x", Every: "1m", Message: "x"},           // too often
	} {
		if _, err := h.s.Add(context.Background(), "chat", a); err == nil {
			t.Errorf("%+v: no error", a)
		}
	}
	for range MaxSchedules {
		h.add(t, AddArgs{Name: "x", In: "1h", Message: "x"})
	}
	if _, err := h.s.Add(context.Background(), "chat", AddArgs{Name: "x", In: "1h", Message: "x"}); err == nil {
		t.Errorf("added more than %d", MaxSchedules)
	}
}

func TestOnlyTheLatestMissedRunCatchesUp(t *testing.T) {
	h := newHarness(t)
	h.add(t, AddArgs{Name: "Hourly", Every: "1h", Prompt: "Check the build"})
	// Down from just before the first run until 10 minutes after the
	// third: the first two are skipped, the third still runs.
	h.fire(t0.Add(3*time.Hour + 10*time.Minute))
	posts, _, starts := h.r.snapshot()
	if len(starts) != 1 || !strings.Contains(starts[0].text, "due Tue 17 Nov") {
		t.Fatalf("starts = %v", starts)
	}
	if n := starts[0].notes; len(n) != 1 || !strings.Contains(n[0], "2 earlier runs were skipped") {
		t.Errorf("notes = %v", n)
	}
	if len(posts) != 1 || !strings.Contains(posts[0].text, "skipped 2 runs") {
		t.Errorf("posts = %v", posts)
	}
	if next := h.s.List()[0].Next; !next.Equal(t0.Add(4 * time.Hour)) {
		t.Errorf("next = %v", next)
	}
}

func TestRunStoppedByShutdownIsReportedAtRestart(t *testing.T) {
	h := newHarness(t)
	h.add(t, AddArgs{Name: "Digest", Every: "1h", Prompt: "Summarize my inbox"})
	h.r.block, h.r.cutErr = make(chan struct{}), errors.New("stopped")
	h.clock.set(t0.Add(time.Hour))
	h.s.fireDue(t0.Add(time.Hour))
	h.s.Stop()
	close(h.r.block) // draining sessions stops the turn
	h.s.wg.Wait()
	if l := h.s.List()[0]; l.Running.IsZero() {
		t.Errorf("run stopped by shutdown recorded as finished: %+v", l.Last)
	}
}

func TestCancelStopsARunStillWaiting(t *testing.T) {
	h := newHarness(t)
	sc := h.add(t, AddArgs{Name: "Digest", Every: "1h", Prompt: "Summarize my inbox"})
	h.r.setBusy("chat/1", true)
	h.clock.set(t0.Add(time.Hour))
	h.s.fireDue(t0.Add(time.Hour))
	if _, err := h.s.Cancel(context.Background(), sc.ID); err != nil {
		t.Fatal(err)
	}
	h.r.setBusy("chat/1", false)
	h.s.wg.Wait()
	if _, _, starts := h.r.snapshot(); len(starts) != 0 {
		t.Errorf("cancelled run started: %v", starts)
	}
}

func TestNewerFileRefusesNewSchedules(t *testing.T) {
	h := newHarness(t)
	if err := os.WriteFile(h.path, []byte(`{"version":99,"data":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	h.s = h.open(t)
	if _, err := h.s.Add(context.Background(), "chat", AddArgs{Name: "Digest", Every: "1h", Prompt: "x"}); err == nil || !strings.Contains(err.Error(), "newer steel") {
		t.Errorf("err = %v", err)
	}
}
