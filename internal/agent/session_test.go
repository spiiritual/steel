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
	cfg.Model = "m"
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
	if h.box(0).resets != 1 {
		t.Errorf("toolbox Reset called %d times", h.box(0).resets)
	}
}

func TestIdleSessionLosesToolboxAndModelIsTold(t *testing.T) {
	h := newHarness(t, Config{IdleTimeout: 50 * time.Millisecond}, func(int, context.Context, llm.Request) (llm.Message, error) { return text("ok"), nil })
	h.ss.Send("c", "hi")
	h.waitTurn()
	waitFor(t, func() bool { b := h.box(0); b.mu.Lock(); defer b.mu.Unlock(); return b.closed })

	h.ss.Send("c", "still there?")
	h.waitTurn()
	msgs := h.llm.req(1).Messages
	if got := roles(msgs[len(msgs)-2:]); !strings.HasPrefix(got, "user:[steel] The scratchpad was closed") || !strings.HasSuffix(got, "user:still there?") {
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
	b, _ := os.ReadFile(filepath.Join(dir, "c.json"))
	if err := json.Unmarshal(b, &got); err != nil || len(got) != 51 {
		t.Errorf("final file has %d messages, err = %v", len(got), err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("leftover temp files: %d entries", len(entries))
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
