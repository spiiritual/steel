package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/spiiritual/steel/internal/llm"
)

const reflectPrompt = "Look back: anything worth a skill?"

func (h *harness) requests() int {
	h.llm.mu.Lock()
	defer h.llm.mu.Unlock()
	return len(h.llm.reqs)
}

// lastReq is the newest request's messages, rendered.
func (h *harness) lastReq() string {
	h.llm.mu.Lock()
	defer h.llm.mu.Unlock()
	return roles(h.llm.reqs[len(h.llm.reqs)-1].Messages)
}

func (h *harness) eventsSince(n int) []Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Event(nil), h.events[n:]...)
}

func (h *harness) eventCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.events)
}

// replies answers a tool call to the user's "work" and text to anything
// else, so each user message is one stretch of work.
func replies(_ int, _ context.Context, req llm.Request) (llm.Message, error) {
	last := req.Messages[len(req.Messages)-1]
	if last.Role == llm.RoleUser && strings.HasPrefix(last.Content, "work") {
		return calls("echo"), nil
	}
	if last.Role == llm.RoleUser && strings.Contains(last.Content, reflectPrompt) {
		return calls("echo"), nil // e.g. writing a skill
	}
	return text("done"), nil
}

// After work with tools goes quiet, the model reflects once, unseen by the
// user, and isn't asked again until it works again.
func TestIdleWorkIsReflectedOnOnce(t *testing.T) {
	h := newHarness(t, Config{IdleTimeout: 20 * time.Millisecond, Reflect: reflectPrompt}, replies)
	h.ss.Send("c", "work please")
	h.waitTurn()
	seen := h.eventCount()
	waitFor(t, func() bool { return h.requests() == 4 }) // work: 2 calls; reflection: 2
	if got := h.lastReq(); !strings.Contains(got, "user:[steel] "+reflectPrompt+" assistant:call(echo) tool(c0):echoed") {
		t.Errorf("reflection request: %s", got)
	}
	time.Sleep(100 * time.Millisecond) // several idle periods
	if n := h.requests(); n != 4 {
		t.Errorf("reflected again without new work: %d requests", n)
	}
	for _, e := range h.eventsSince(seen) {
		t.Errorf("the user saw %T %+v", e, e)
	}

	h.ss.Send("c", "work again")
	h.waitTurn()
	waitFor(t, func() bool { return h.requests() == 8 })
}

// Talk without tools has nothing to learn from.
func TestChatIsNotReflectedOn(t *testing.T) {
	h := newHarness(t, Config{IdleTimeout: 20 * time.Millisecond, Reflect: reflectPrompt}, replies)
	h.ss.Send("c", "hello")
	h.waitTurn()
	time.Sleep(100 * time.Millisecond)
	if n := h.requests(); n != 1 {
		t.Errorf("%d requests, want 1", n)
	}
}

// A message from the user during the reflection is answered in the open.
func TestUserInterruptsReflection(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, Config{IdleTimeout: 20 * time.Millisecond, Reflect: reflectPrompt}, func(i int, ctx context.Context, req llm.Request) (llm.Message, error) {
		last := req.Messages[len(req.Messages)-1]
		if last.Role == llm.RoleUser && strings.Contains(last.Content, reflectPrompt) {
			<-release // the user writes while the model reflects
			return calls("echo"), nil
		}
		return replies(i, ctx, req)
	})
	h.ss.Send("c", "work please")
	h.waitTurn()
	waitFor(t, func() bool { return h.requests() == 3 })
	h.ss.Send("c", "are you there?")
	close(release)
	h.waitTurn()
	var got []string
	for _, e := range h.eventsSince(0) {
		if r, ok := e.(Reply); ok {
			got = append(got, r.Text)
		}
	}
	if len(got) != 2 { // the work's answer, and the one to "are you there?"
		t.Errorf("replies shown: %q", got)
	}
}
