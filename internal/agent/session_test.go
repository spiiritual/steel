package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spiiritual/steel/internal/datafile"
	"github.com/spiiritual/steel/internal/llm"
)

// fakeLLM answers each request with respond(i, ctx, req) and records requests.
type fakeLLM struct {
	mu      sync.Mutex
	reqs    []llm.Request
	respond func(i int, ctx context.Context, req llm.Request) (llm.Message, error)
}

func (f *fakeLLM) Stream(ctx context.Context, req llm.Request, _ func(string) error) (llm.Message, error) {
	f.mu.Lock()
	i := len(f.reqs)
	f.reqs = append(f.reqs, llm.Request{Model: req.Model, Messages: append([]llm.Message(nil), req.Messages...), Tools: req.Tools})
	f.mu.Unlock()
	return f.respond(i, ctx, req)
}

func (f *fakeLLM) req(i int) llm.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reqs[i]
}

func text(s string) llm.Message { return llm.Message{Role: llm.RoleAssistant, Content: s} }

func calls(names ...string) llm.Message {
	m := llm.Message{Role: llm.RoleAssistant}
	for i, n := range names {
		m.ToolCalls = append(m.ToolCalls, llm.ToolCall{ID: fmt.Sprintf("c%d", i), Name: n, Arguments: `{}`})
	}
	return m
}

// fakeBox has tools "echo" and "block" (waits for ctx or release).
type fakeBox struct {
	hooks   Hooks
	release chan struct{}
	mu      sync.Mutex
	stops   int
	resets  int
	closed  bool
}

func (b *fakeBox) Defs() []llm.ToolDef { return []llm.ToolDef{{Name: "echo"}, {Name: "block"}} }

func (b *fakeBox) Call(ctx context.Context, name string, _ json.RawMessage) (string, error) {
	switch name {
	case "echo":
		return "echoed", nil
	case "block":
		select {
		case <-b.release:
			return "released", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return "", fmt.Errorf("unknown tool %q", name)
}

func (b *fakeBox) Stop() bool { b.mu.Lock(); b.stops++; b.mu.Unlock(); return false }
func (b *fakeBox) Reset()     { b.mu.Lock(); b.resets++; b.mu.Unlock() }
func (b *fakeBox) Busy() bool { return false }
func (b *fakeBox) Close()     { b.mu.Lock(); b.closed = true; b.mu.Unlock() }

// harness wires Sessions to fakes and records events.
type harness struct {
	t      *testing.T
	llm    *fakeLLM
	ss     *Sessions
	mu     sync.Mutex
	boxes  []*fakeBox
	events []Event
	done   chan TurnDone
}

func newHarness(t *testing.T, cfg Config, respond func(int, context.Context, llm.Request) (llm.Message, error)) *harness {
	h := &harness{t: t, llm: &fakeLLM{respond: respond}, done: make(chan TurnDone, 16)}
	cfg.Provider = h.llm
	cfg.Model = func() llm.Request { return llm.Request{Model: "m"} }
	cfg.NewToolbox = func(hk Hooks) Toolbox {
		b := &fakeBox{hooks: hk, release: make(chan struct{})}
		h.mu.Lock()
		h.boxes = append(h.boxes, b)
		h.mu.Unlock()
		return b
	}
	ss, err := NewSessions(cfg, func(_ string, e Event) {
		h.mu.Lock()
		h.events = append(h.events, e)
		h.mu.Unlock()
		if d, ok := e.(TurnDone); ok {
			h.done <- d
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	h.ss = ss
	t.Cleanup(ss.Close)
	return h
}

func (h *harness) box(i int) *fakeBox {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.boxes[i]
}

func (h *harness) waitTurn() TurnDone {
	h.t.Helper()
	select {
	case d := <-h.done:
		return d
	case <-time.After(2 * time.Second):
		h.t.Fatal("turn did not finish")
		return TurnDone{}
	}
}

// roles renders messages compactly, e.g. "user:hi tool(c0):echoed".
func roles(ms []llm.Message) string {
	var out []string
	for _, m := range ms {
		switch {
		case m.Role == llm.RoleTool:
			out = append(out, fmt.Sprintf("tool(%s):%s", m.ToolCallID, m.Content))
		case len(m.ToolCalls) > 0:
			var ns []string
			for _, c := range m.ToolCalls {
				ns = append(ns, c.Name)
			}
			out = append(out, "assistant:call("+strings.Join(ns, ",")+")")
		default:
			out = append(out, string(m.Role)+":"+m.Content)
		}
	}
	return strings.Join(out, " ")
}

func TestTurnRunsToolsUntilFinalAnswer(t *testing.T) {
	h := newHarness(t, Config{}, func(i int, _ context.Context, _ llm.Request) (llm.Message, error) {
		return []llm.Message{calls("echo", "missing"), text("done")}[i], nil
	})
	h.ss.Send("c", "go")
	if d := h.waitTurn(); d.Err != nil {
		t.Fatal(d.Err)
	}
	if got, want := roles(h.llm.req(1).Messages), `user:go assistant:call(echo,missing) tool(c0):echoed tool(c1):error: unknown tool "missing"`; got != want {
		t.Errorf("second request:\n got %s\nwant %s", got, want)
	}

	var seq []string
	for _, e := range h.events {
		switch e := e.(type) {
		case ToolStart:
			seq = append(seq, "start "+e.Name)
		case ToolEnd:
			seq = append(seq, "end "+e.Name)
		case Reply:
			seq = append(seq, "reply "+e.Text)
		case TurnDone:
			seq = append(seq, "done")
		}
	}
	if got, want := strings.Join(seq, ", "), "start echo, end echo, start missing, end missing, reply done, done"; got != want {
		t.Errorf("events = %s, want %s", got, want)
	}
}

func TestMessageSentMidTurnReachesNextModelCall(t *testing.T) {
	h := newHarness(t, Config{}, func(i int, _ context.Context, _ llm.Request) (llm.Message, error) {
		return []llm.Message{calls("block"), text("ok")}[i], nil
	})
	h.ss.Send("c", "first")
	waitFor(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return len(h.boxes) == 1 })
	h.ss.Send("c", "also this")
	time.Sleep(20 * time.Millisecond) // let it land in the inbox while the tool blocks
	close(h.box(0).release)
	h.waitTurn()

	if got, want := roles(h.llm.req(1).Messages), "user:first assistant:call(block) tool(c0):released user:also this"; got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestDeliveredResultStartsTurnWhenIdle(t *testing.T) {
	h := newHarness(t, Config{}, func(int, context.Context, llm.Request) (llm.Message, error) { return text("ok"), nil })
	h.ss.Send("c", "hi")
	h.waitTurn()

	h.box(0).hooks.Deliver("[job j1 finished]\n42")
	h.waitTurn()
	if got, want := roles(h.llm.req(1).Messages), "user:hi assistant:ok user:[job j1 finished]\n42"; got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestStopKeepsHistoryValid(t *testing.T) {
	h := newHarness(t, Config{}, func(i int, _ context.Context, _ llm.Request) (llm.Message, error) {
		return []llm.Message{calls("block", "echo"), text("after")}[i], nil
	})
	h.ss.Send("c", "go")
	waitFor(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return len(h.boxes) == 1 })
	time.Sleep(20 * time.Millisecond) // block is now running
	h.ss.Send("c", "/stop")
	if d := h.waitTurn(); d.Err != nil {
		t.Fatalf("stopped turn reported error: %v", d.Err)
	}
	if h.box(0).stops != 1 {
		t.Errorf("toolbox Stop called %d times", h.box(0).stops)
	}

	// Every tool call got a result, so the next request is valid.
	h.ss.Send("c", "again")
	h.waitTurn()
	if got, want := roles(h.llm.req(1).Messages), "user:go assistant:call(block,echo) tool(c0):error: context canceled tool(c1):error: cancelled user:[steel] The user stopped your previous turn. user:again"; got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestModelErrorKeepsUserMessage(t *testing.T) {
	h := newHarness(t, Config{}, func(i int, _ context.Context, _ llm.Request) (llm.Message, error) {
		if i == 0 {
			return llm.Message{}, errors.New("boom")
		}
		return text("ok"), nil
	})
	h.ss.Send("c", "one")
	if d := h.waitTurn(); d.Err == nil || d.Err.Error() != "boom" {
		t.Fatalf("TurnDone = %+v", d)
	}
	h.ss.Send("c", "two")
	h.waitTurn()
	if got, want := roles(h.llm.req(1).Messages), "user:one user:two"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestHistoryPersistsAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	respond := func(int, context.Context, llm.Request) (llm.Message, error) { return text("ok"), nil }

	h1 := newHarness(t, Config{Dir: dir, System: "old"}, respond)
	h1.ss.Send("chat/1", "remember me")
	h1.waitTurn()
	h1.ss.Close()

	h2 := newHarness(t, Config{Dir: dir, System: "new"}, respond)
	h2.ss.Send("chat/1", "back")
	h2.waitTurn()
	if got, want := roles(h2.llm.req(0).Messages), "system:new user:remember me assistant:ok user:back"; got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// After a restart the model hears that its scratchpad is gone, once, and
// only if it had used it.
func TestRestartTellsModelOnlyIfItUsedTheScratchpad(t *testing.T) {
	dir := t.TempDir()
	h1 := newHarness(t, Config{Dir: dir}, func(i int, _ context.Context, _ llm.Request) (llm.Message, error) {
		if i == 0 {
			return calls("echo"), nil
		}
		return text("ok"), nil
	})
	h1.ss.Send("c", "compute")
	h1.waitTurn()
	h1.ss.Close()

	respond := func(int, context.Context, llm.Request) (llm.Message, error) { return text("ok"), nil }
	h2 := newHarness(t, Config{Dir: dir}, respond)
	h2.ss.Send("c", "back")
	h2.waitTurn()
	if got := roles(h2.llm.req(0).Messages); !strings.HasSuffix(got, "user:"+scratchpadClosed+" user:back") {
		t.Errorf("not told: %s", got)
	}
	h2.ss.Close()

	h3 := newHarness(t, Config{Dir: dir}, respond)
	h3.ss.Send("c", "again")
	h3.waitTurn()
	if got := roles(h3.llm.req(0).Messages); strings.Count(got, scratchpadClosed) != 1 {
		t.Errorf("told again: %s", got)
	}
}

// boxClosed reports whether toolbox i was closed.
func (h *harness) boxClosed(i int) bool {
	b := h.box(i)
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed
}

// live reports whether session id is in memory.
func (h *harness) live(id string) bool {
	h.ss.mu.Lock()
	defer h.ss.mu.Unlock()
	_, ok := h.ss.m[id]
	return ok
}

// An idle session leaves memory and comes back from its file, its
// conversation intact, on its next message.
func TestIdleSessionIsRetiredAndReloaded(t *testing.T) {
	h := newHarness(t, Config{Dir: t.TempDir(), System: "sys", IdleTimeout: 20 * time.Millisecond},
		func(int, context.Context, llm.Request) (llm.Message, error) { return text("ok"), nil })
	h.ss.Send("c", "hi")
	h.waitTurn()
	waitFor(t, func() bool { return !h.live("c") })

	h.ss.Send("c", "back")
	h.waitTurn()
	if got, want := roles(h.llm.req(1).Messages), "system:sys user:hi assistant:ok user:back"; got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// A note pending when a session retires is saved with it and still
// reaches the model.
func TestRetiredSessionKeepsPendingNote(t *testing.T) {
	h := newHarness(t, Config{Dir: t.TempDir(), IdleTimeout: 20 * time.Millisecond},
		func(int, context.Context, llm.Request) (llm.Message, error) { return text("ok"), nil })
	h.ss.Send("c", "hi")
	h.waitTurn()
	h.ss.Send("c", "/reset")
	waitFor(t, func() bool { return !h.live("c") })
	h.ss.Send("c", "next")
	h.waitTurn()
	if got := roles(h.llm.req(1).Messages); !strings.Contains(got, "The user cleared the scratchpad") {
		t.Errorf("note lost: %s", got)
	}
}

// Without a history directory, nothing could bring a session back.
func TestUnsavedSessionIsNeverRetired(t *testing.T) {
	h := newHarness(t, Config{IdleTimeout: 20 * time.Millisecond},
		func(int, context.Context, llm.Request) (llm.Message, error) { return text("ok"), nil })
	h.ss.Send("c", "hi")
	h.waitTurn()
	waitFor(t, func() bool { return h.boxClosed(0) })
	h.ss.retireSettled() // idle long enough: the box closing proves it
	if !h.live("c") {
		t.Error("session without a history directory was retired")
	}
}

func TestNewClearsConversation(t *testing.T) {
	h := newHarness(t, Config{System: "sys"}, func(int, context.Context, llm.Request) (llm.Message, error) { return text("ok"), nil })
	h.ss.Send("c", "old")
	h.waitTurn()
	h.ss.Send("c", "/new")
	h.ss.Send("c", "fresh")
	h.waitTurn()
	if got, want := roles(h.llm.req(1).Messages), "system:sys user:fresh"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
	// The old toolbox is closed and the new conversation gets its own.
	b := h.box(0)
	b.mu.Lock()
	closed := b.closed
	b.mu.Unlock()
	h.mu.Lock()
	n := len(h.boxes)
	h.mu.Unlock()
	if !closed || n != 2 {
		t.Errorf("old toolbox closed = %v, toolboxes = %d", closed, n)
	}
}

func TestIdleSessionLosesToolboxAndModelIsTold(t *testing.T) {
	h := newHarness(t, Config{IdleTimeout: 50 * time.Millisecond}, func(i int, _ context.Context, _ llm.Request) (llm.Message, error) {
		if i == 0 {
			return calls("echo"), nil // the model uses its scratchpad
		}
		return text("ok"), nil
	})
	h.ss.Send("c", "hi")
	h.waitTurn()
	waitFor(t, func() bool { return h.boxClosed(0) })

	h.ss.Send("c", "still there?")
	h.waitTurn()
	msgs := h.llm.req(2).Messages
	if got, want := roles(msgs[len(msgs)-2:]), "user:"+scratchpadClosed+" user:still there?"; got != want {
		t.Errorf("got %s", got)
	}
	h.mu.Lock()
	n := len(h.boxes)
	h.mu.Unlock()
	if n != 2 {
		t.Errorf("toolboxes created = %d, want 2", n)
	}
}

func TestResetTellsModelStateIsGone(t *testing.T) {
	h := newHarness(t, Config{}, func(int, context.Context, llm.Request) (llm.Message, error) { return text("ok"), nil })
	h.ss.Send("c", "hi")
	h.waitTurn()
	h.ss.Send("c", "/reset")
	h.ss.Send("c", "next")
	h.waitTurn()
	msgs := h.llm.req(1).Messages
	if got := roles(msgs[2:]); !strings.HasPrefix(got, "user:[steel] The user cleared the scratchpad") || !strings.HasSuffix(got, "user:next") {
		t.Errorf("got %s", got)
	}
	if h.box(0).resets != 1 {
		t.Errorf("Reset called %d times", h.box(0).resets)
	}
}

func TestCloseWaitsForTurnAndSaves(t *testing.T) {
	dir := t.TempDir()
	started := make(chan struct{})
	h := newHarness(t, Config{Dir: dir}, func(_ int, ctx context.Context, _ llm.Request) (llm.Message, error) {
		close(started)
		<-ctx.Done()
		return llm.Message{}, ctx.Err()
	})
	h.ss.Send("c", "in flight")
	<-started
	// Already saved while the turn runs, in case shutdown can't wait for it.
	if b, err := os.ReadFile(filepath.Join(dir, "c.json")); err != nil || !strings.Contains(string(b), "in flight") {
		t.Errorf("mid-turn save = %s, err = %v", b, err)
	}
	h.ss.Close()

	// The cancelled turn's save completed before Close returned.
	b, err := os.ReadFile(filepath.Join(dir, "c.json"))
	if err != nil || !strings.Contains(string(b), "in flight") {
		t.Errorf("saved = %s, err = %v", b, err)
	}
	h.ss.Send("c", "after close") // ignored, must not panic or start a turn
}

func TestConcurrentSavesKeepFileValid(t *testing.T) {
	dir := t.TempDir()
	s := newSession("c", &Config{Dir: dir, System: "sys"}, func(Event) {})
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.mu.Lock()
			s.history = append(s.history, llm.Message{Role: llm.RoleUser, Content: fmt.Sprint(i)})
			s.mu.Unlock()
			s.save()
		}()
	}
	wg.Wait()
	var got []llm.Message
	if _, err := datafile.Read(filepath.Join(dir, "c.json"), historyVersion, &got); err != nil || len(got) != 51 {
		t.Errorf("final file has %d messages, err = %v", len(got), err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("leftover temp files: %d entries", len(entries))
	}
}

func TestDrainLetsRunningTurnFinishAndIgnoresNewInput(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, Config{}, func(i int, ctx context.Context, _ llm.Request) (llm.Message, error) {
		if i == 0 {
			select {
			case <-release:
			case <-ctx.Done():
				return llm.Message{}, ctx.Err()
			}
		}
		return text("done"), nil
	})
	h.ss.Send("c", "work")
	waitFor(t, func() bool { return !h.ss.Idle("c") })

	drained := make(chan error, 1)
	go func() { drained <- h.ss.Drain(context.Background()) }()
	waitFor(t, func() bool { h.ss.inputMu.RLock(); defer h.ss.inputMu.RUnlock(); return h.ss.draining })
	h.ss.Send("c", "ignored")
	select {
	case err := <-drained:
		t.Fatalf("Drain returned with a turn running: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-drained; err != nil {
		t.Fatal(err)
	}
	// The turn finished normally rather than being cancelled, and input
	// sent while draining started nothing.
	if d := h.waitTurn(); d.Err != nil {
		t.Errorf("turn err = %v", d.Err)
	}
	h.llm.mu.Lock()
	n := len(h.llm.reqs)
	h.llm.mu.Unlock()
	if n != 1 {
		t.Errorf("model requests = %d, want 1", n)
	}
}

func TestDrainGivesUpAtDeadline(t *testing.T) {
	h := newHarness(t, Config{}, func(_ int, ctx context.Context, _ llm.Request) (llm.Message, error) {
		<-ctx.Done()
		return llm.Message{}, ctx.Err()
	})
	h.ss.Send("c", "stuck")
	waitFor(t, func() bool { return !h.ss.Idle("c") })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := h.ss.Drain(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Drain = %v, want deadline exceeded", err)
	}
}

func TestHistoryFromNewerSteelIsLeftAlone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.json")
	newer := `{"version":99,"data":{"something":"new"}}`
	os.WriteFile(path, []byte(newer), 0o600)
	var notices []string
	s := newSession("c", &Config{Dir: dir}, func(e Event) {
		if n, ok := e.(Notice); ok {
			notices = append(notices, n.Text)
		}
	})
	s.history = append(s.history, llm.Message{Role: llm.RoleUser, Content: "hi"})
	s.save()
	if b, _ := os.ReadFile(path); string(b) != newer {
		t.Errorf("newer file overwritten: %s", b)
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "newer steel") {
		t.Errorf("notices = %q", notices)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A tool's approval request reaches the channel as an event; the channel's
// answer reaches the tool once, and the outcome is announced.
func TestApprovalRoundTrip(t *testing.T) {
	h := newHarness(t, Config{}, func(int, context.Context, llm.Request) (llm.Message, error) { return text("ok"), nil })
	h.ss.Send("c", "hi")
	h.waitTurn()
	if h.ss.Answer("other", "x", true) {
		t.Error("an unknown session accepted an answer")
	}

	request := func() ApprovalRequest {
		var r ApprovalRequest
		waitFor(t, func() bool {
			h.mu.Lock()
			defer h.mu.Unlock()
			for _, e := range h.events {
				if e, ok := e.(ApprovalRequest); ok && e.ID != "" {
					r = e
				}
			}
			return r.ID != ""
		})
		h.mu.Lock()
		h.events = nil
		h.mu.Unlock()
		return r
	}
	done := func(id string) ApprovalOutcome {
		var o ApprovalOutcome
		waitFor(t, func() bool {
			h.mu.Lock()
			defer h.mu.Unlock()
			for _, e := range h.events {
				if e, ok := e.(ApprovalDone); ok && e.ID == id {
					o = e.Outcome
				}
			}
			return o != ""
		})
		return o
	}
	type result struct {
		ok  bool
		err error
	}
	ask := func(ctx context.Context) chan result {
		c := make(chan result, 1)
		go func() {
			ok, err := h.box(0).hooks.Approve(ctx, "shell.bash", "rm -rf x", []string{"deletes recursively"})
			c <- result{ok, err}
		}()
		return c
	}

	for _, allow := range []bool{true, false} {
		res := ask(context.Background())
		r := request()
		if r.Action != "rm -rf x" || r.Tool != "shell.bash" || len(r.Reasons) != 1 {
			t.Errorf("request %+v", r)
		}
		if !h.ss.Answer("c", r.ID, allow) {
			t.Fatal("answer not accepted")
		}
		if h.ss.Answer("c", r.ID, !allow) {
			t.Error("a second answer was accepted")
		}
		if got := <-res; got.ok != allow || got.err != nil {
			t.Errorf("allow=%v: tool got %+v", allow, got)
		}
		want := map[bool]ApprovalOutcome{true: Allowed, false: Refused}[allow]
		if o := done(r.ID); o != want {
			t.Errorf("outcome %s, want %s", o, want)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	res := ask(ctx)
	r := request()
	cancel() // e.g. /stop
	if got := <-res; got.ok || got.err == nil {
		t.Errorf("cancelled: tool got %+v", got)
	}
	if o := done(r.ID); o != Cancelled {
		t.Errorf("outcome %s, want cancelled", o)
	}
	if h.ss.Answer("c", r.ID, true) {
		t.Error("a cancelled request accepted an answer")
	}

	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	res = ask(ctx)
	r = request()
	<-res
	if o := done(r.ID); o != Expired {
		t.Errorf("outcome %s, want expired", o)
	}
}

// A tool's secret request reaches the channel as an event; the value the
// channel provides reaches the tool, and only one request waits at a time.
func TestSecretRoundTrip(t *testing.T) {
	h := newHarness(t, Config{}, func(int, context.Context, llm.Request) (llm.Message, error) { return text("ok"), nil })
	h.ss.Send("c", "hi")
	h.waitTurn()

	events := func() []Event {
		h.mu.Lock()
		defer h.mu.Unlock()
		return append([]Event(nil), h.events...)
	}
	request := func() SecretRequest {
		var r SecretRequest
		waitFor(t, func() bool {
			for _, e := range events() {
				if e, ok := e.(SecretRequest); ok {
					r = e
				}
			}
			return r.ID != ""
		})
		h.mu.Lock()
		h.events = nil
		h.mu.Unlock()
		return r
	}
	done := func(id string) SecretDone {
		var d SecretDone
		waitFor(t, func() bool {
			for _, e := range events() {
				if e, ok := e.(SecretDone); ok && e.ID == id {
					d = e
					return true
				}
			}
			return false
		})
		return d
	}
	type result struct {
		saved string
		err   error
	}
	// ask asks for GH_TOKEN, saving the value unless saveErr is set.
	ask := func(saveErr error) chan result {
		c := make(chan result, 1)
		go func() {
			var saved string
			err := h.box(0).hooks.AskSecret(context.Background(), "GH_TOKEN", "GitHub", func(v string) error {
				if saveErr != nil {
					return saveErr
				}
				saved = v
				return nil
			})
			c <- result{saved, err}
		}()
		return c
	}

	res := ask(nil)
	r := request()
	if r.Name != "GH_TOKEN" || r.Purpose != "GitHub" {
		t.Errorf("request %+v", r)
	}
	if err := h.box(0).hooks.AskSecret(context.Background(), "OTHER", "x", func(string) error { return nil }); err == nil {
		t.Error("a second request was made while one was waiting")
	}
	if !h.ss.ProvideSecret("c", r.ID, "ghp_value") {
		t.Fatal("value not accepted")
	}
	if got := <-res; got.saved != "ghp_value" || got.err != nil {
		t.Errorf("tool got %+v", got)
	}
	if d := done(r.ID); d.Outcome != SecretReceived {
		t.Errorf("done %+v", d)
	}

	// The user is told "saved" only once it is.
	res = ask(errors.New("disk full"))
	r = request()
	h.ss.ProvideSecret("c", r.ID, "ghp_value")
	if got := <-res; got.err == nil || !strings.Contains(got.err.Error(), "disk full") {
		t.Errorf("failed save: tool got %+v", got)
	}
	if d := done(r.ID); d.Outcome != SecretNotSaved || d.Problem != "disk full" {
		t.Errorf("failed save: done %+v", d)
	}

	res = ask(nil)
	r = request()
	if !h.ss.CancelSecret("c", r.ID) {
		t.Fatal("cancel not accepted")
	}
	if got := <-res; got.err == nil || got.saved != "" {
		t.Errorf("declined: tool got %+v", got)
	}
}
