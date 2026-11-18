package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spiiritual/steel/internal/llm"
)

func TestPromptWaitsForTheTurnAndReturnsItsReply(t *testing.T) {
	h := newHarness(t, Config{}, func(i int, _ context.Context, _ llm.Request) (llm.Message, error) {
		return []llm.Message{calls("echo"), text("all done")}[i], nil
	})
	reply, err := h.ss.Prompt(context.Background(), "c", "[scheduled] check the package")
	if err != nil || reply != "all done" {
		t.Fatalf("Prompt = %q, %v", reply, err)
	}
}

func TestPromptReportsAFailedTurn(t *testing.T) {
	h := newHarness(t, Config{}, func(int, context.Context, llm.Request) (llm.Message, error) {
		return llm.Message{}, errors.New("API down")
	})
	if _, err := h.ss.Prompt(context.Background(), "c", "hi"); err == nil || !strings.Contains(err.Error(), "API down") {
		t.Fatalf("err = %v", err)
	}
}

func TestStartBeginsAFreshConversationWithNotes(t *testing.T) {
	h := newHarness(t, Config{System: "sys"}, func(int, context.Context, llm.Request) (llm.Message, error) {
		return text("ok"), nil
	})
	h.ss.Send("c", "earlier chat")
	h.waitTurn()
	if _, err := h.ss.Start(context.Background(), "c", "run", []string{"last time: fine"}); err != nil {
		t.Fatal(err)
	}
	if got, want := roles(h.llm.req(1).Messages), "system:sys user:[steel] last time: fine user:run"; got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	// The user's reply continues the run.
	h.ss.Send("c", "thanks")
	h.waitTurn()
	h.waitTurn()
	if got, want := roles(h.llm.req(2).Messages), "system:sys user:[steel] last time: fine user:run assistant:ok user:thanks"; got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestStartWontInterruptWork(t *testing.T) {
	h := newHarness(t, Config{}, func(i int, _ context.Context, _ llm.Request) (llm.Message, error) {
		return []llm.Message{calls("block"), text("ok")}[i], nil
	})
	h.ss.Send("c", "long job")
	waitFor(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return len(h.boxes) == 1 })
	if h.ss.Quiet("c", 0) {
		t.Error("busy session is quiet")
	}
	if _, err := h.ss.Start(context.Background(), "c", "run", nil); !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v; want ErrBusy", err)
	}
	close(h.box(0).release)
	h.waitTurn()
	if !h.ss.Quiet("c", 0) || h.ss.Quiet("c", time.Hour) {
		t.Error("Quiet doesn't measure the time since the last turn")
	}
	if got := roles(h.llm.req(1).Messages); strings.Contains(got, "run") {
		t.Errorf("busy Start changed the conversation: %s", got)
	}
}

func TestStopEndsAWaitingPrompt(t *testing.T) {
	h := newHarness(t, Config{}, func(i int, ctx context.Context, _ llm.Request) (llm.Message, error) {
		return []llm.Message{calls("block"), text("ok"), text("ok")}[i], nil
	})
	h.ss.Send("c", "long job")
	waitFor(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return len(h.boxes) == 1 })
	got := make(chan error, 1)
	go func() {
		_, err := h.ss.Prompt(context.Background(), "c", "[scheduled] later")
		got <- err
	}()
	waitFor(t, func() bool { return !h.ss.Quiet("c", 0) && h.inboxLen("c") == 1 })
	h.ss.Send("c", "/stop")
	select {
	case err := <-got:
		if err == nil {
			t.Error("dropped prompt reported success")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Prompt still waiting after /stop")
	}
}

func TestPostShowsTextAndTellsTheModel(t *testing.T) {
	h := newHarness(t, Config{}, func(int, context.Context, llm.Request) (llm.Message, error) {
		return text("ok"), nil
	})
	h.ss.Send("c", "hi")
	h.waitTurn()
	h.ss.Post("c", "⏰ Call mom", "Reminder sent: Call mom")
	h.mu.Lock()
	last := h.events[len(h.events)-1]
	h.mu.Unlock()
	if n, ok := last.(Notice); !ok || n.Text != "⏰ Call mom" {
		t.Errorf("last event = %#v", last)
	}
	h.ss.Send("c", "done it")
	h.waitTurn()
	if got, want := roles(h.llm.req(1).Messages), "user:hi assistant:ok user:[steel] Reminder sent: Call mom user:done it"; got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func (h *harness) inboxLen(id string) int {
	h.ss.mu.Lock()
	s := h.ss.m[id]
	h.ss.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.inbox)
}

func TestStoppedPromptIsAFailure(t *testing.T) {
	h := newHarness(t, Config{}, func(i int, ctx context.Context, _ llm.Request) (llm.Message, error) {
		return []llm.Message{calls("block"), text("ok")}[i], nil
	})
	got := make(chan error, 1)
	go func() {
		_, err := h.ss.Prompt(context.Background(), "c", "[scheduled] long job")
		got <- err
	}()
	waitFor(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return len(h.boxes) == 1 })
	h.ss.Send("c", "/stop")
	select {
	case err := <-got:
		if err == nil {
			t.Error("stopped prompt reported success")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Prompt still waiting after /stop")
	}
}

func TestPromptThatFailedToWakeNeverReachesTheModel(t *testing.T) {
	mem := &fakeMemory{err: errors.New("disk gone")}
	h := newHarness(t, Config{Memory: mem}, func(int, context.Context, llm.Request) (llm.Message, error) { return text("ok"), nil })
	if _, err := h.ss.Prompt(context.Background(), "c", "[scheduled] check"); err == nil {
		t.Fatal("Prompt succeeded without the memory")
	}
	mem.setErr(nil)
	h.ss.Send("c", "hi")
	h.waitTurn()
	h.waitTurn()
	if got, want := roles(h.llm.req(0).Messages), "user:mem1 user:hi"; got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}
