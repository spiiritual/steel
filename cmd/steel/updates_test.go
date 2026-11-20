package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spiiritual/steel/internal/agent"
	"github.com/spiiritual/steel/internal/channel/telegram"
	"github.com/spiiritual/steel/internal/updater"
)

// fakeUpdater serves the updater's API from a status the test sets.
type fakeUpdater struct {
	mu     sync.Mutex
	status updater.Status
	starts int
	srv    *httptest.Server
}

func newFakeUpdater(t *testing.T, s updater.Status) *fakeUpdater {
	f := &fakeUpdater{status: s}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/status":
			json.NewEncoder(w).Encode(f.status)
		case "/update", "/rollback":
			if f.status.Busy {
				w.WriteHeader(http.StatusConflict)
				json.NewEncoder(w).Encode(map[string]string{"error": updater.ErrBusy.Error()})
				return
			}
			f.starts++
			res := updater.Result{Action: strings.TrimPrefix(r.URL.Path, "/"), From: f.status.Current, To: f.status.Latest, Started: time.Now()}
			f.status.Busy, f.status.Last = true, &res
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(res)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// finish ends the running action like the updater would.
func (f *fakeUpdater) finish(errText string, restored bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	last := *f.status.Last
	last.Error, last.Restored, last.Finished = errText, restored, time.Now()
	f.status.Busy, f.status.Last = false, &last
	if errText == "" {
		f.status.Previous, f.status.Current = f.status.Current, last.To
	}
}

// notices collects what's emitted to sessions and announced.
type notices struct {
	mu        sync.Mutex
	sent      []string // "session: text"
	announced []string
}

func (n *notices) emit(id string, e agent.Event) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sent = append(n.sent, id+": "+e.(agent.Notice).Text)
}

func (n *notices) announce(s telegram.UpdateStatus) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.announced = append(n.announced, s.Latest)
}

func (n *notices) got() ([]string, []string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.sent...), append([]string(nil), n.announced...)
}

// newTestUpdates is one steel's updates, saving state in dir.
func newTestUpdates(f *fakeUpdater, dir string, n *notices) *updates {
	return &updates{
		client: &updater.Client{URL: f.srv.URL}, path: filepath.Join(dir, "update.json"),
		announce: n.announce, emit: n.emit, logf: func(string, ...any) {},
		poll: time.Millisecond, giveUp: time.Minute, checkFirst: time.Millisecond, checkEvery: time.Hour,
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The steel that starts an update is stopped by it; the next steel, on the
// same volume, says how it went where it was asked.
func TestUpdateIsReportedByTheNextSteel(t *testing.T) {
	f := newFakeUpdater(t, updater.Status{Current: "v1.0.0", Latest: "v1.1.0"})
	dir := t.TempDir()
	first := newTestUpdates(f, dir, &notices{})
	first.reporting = true // its reporter dies with it, before seeing the end
	text, err := first.Update(context.Background(), "telegram:42")
	if err != nil || !strings.Contains(text, "from v1.0.0 to v1.1.0") {
		t.Fatalf("Update = %q, %v", text, err)
	}

	f.finish("", false)
	n := &notices{}
	second := newTestUpdates(f, dir, n)
	go second.run(t.Context())
	waitFor(t, "the report", func() bool {
		second.mu.Lock()
		defer second.mu.Unlock()
		s, _ := second.load()
		return s.Pending == nil
	})
	sent, _ := n.got()
	if len(sent) != 1 || !strings.HasPrefix(sent[0], "telegram:42: Updated to steel v1.1.0 (from v1.0.0)") {
		t.Errorf("sent %q", sent)
	}
	// Reported once: a third steel says nothing.
	n3 := &notices{}
	newTestUpdates(f, dir, n3).report(context.Background())
	if sent, _ := n3.got(); len(sent) != 0 {
		t.Errorf("reported again: %q", sent)
	}
}

func TestOutcomes(t *testing.T) {
	at := time.Now()
	p := pendingUpdate{Action: "update", From: "telegram:42", At: at}
	last := func(errText string, restored bool) *updater.Result {
		return &updater.Result{Action: "update", From: "v1.0.0", To: "v1.1.0", Started: at.Add(time.Second), Error: errText, Restored: restored}
	}
	for _, tc := range []struct {
		st   updater.Status
		want string
	}{
		{updater.Status{Current: "v1.0.0", Last: last("the new version didn't stay up", true)}, "steel v1.1.0 didn't work, so I'm back on v1.0.0. the new version didn't stay up"},
		{updater.Status{Current: "v1.0.0", Last: last("pulling: manifest unknown", false)}, "The update to v1.1.0 failed, so I'm still on v1.0.0: pulling: manifest unknown"},
		{updater.Status{Current: "v1.1.0"}, "the updater restarted meanwhile"},
	} {
		if got := outcome(p, tc.st); !strings.Contains(got, tc.want) {
			t.Errorf("outcome = %q, want %q", got, tc.want)
		}
	}
}

func TestStartWhileBusyKeepsWhatsPending(t *testing.T) {
	f := newFakeUpdater(t, updater.Status{Current: "v1.0.0", Latest: "v1.1.0"})
	dir := t.TempDir()
	n := &notices{}
	u := newTestUpdates(f, dir, n)
	u.reporting = true // the update never finishes here
	if _, err := u.Update(context.Background(), "telegram:42"); err != nil {
		t.Fatal(err)
	}
	text, err := u.Rollback(context.Background(), "telegram:7")
	if err != nil || !strings.Contains(text, "already under way") {
		t.Fatalf("Rollback while busy = %q, %v", text, err)
	}
	u.mu.Lock()
	s, _ := u.load()
	u.mu.Unlock()
	if s.Pending == nil || s.Pending.From != "telegram:42" || s.Pending.Action != "update" {
		t.Errorf("pending = %+v, want the first request's", s.Pending)
	}
}

func TestNewReleaseIsAnnouncedOnce(t *testing.T) {
	f := newFakeUpdater(t, updater.Status{Current: "v1.0.0", Latest: "v1.1.0"})
	dir := t.TempDir()
	n := &notices{}
	u := newTestUpdates(f, dir, n)
	for range 2 {
		if err := u.announceNew(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	// A restarted steel remembers it too.
	if err := newTestUpdates(f, dir, n).announceNew(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, a := n.got(); len(a) != 1 || a[0] != "v1.1.0" {
		t.Errorf("announced %q", a)
	}
	f.mu.Lock()
	f.status.Latest = "v1.2.0"
	f.mu.Unlock()
	u.announceNew(context.Background())
	if _, a := n.got(); len(a) != 2 || a[1] != "v1.2.0" {
		t.Errorf("announced %q", a)
	}
}
