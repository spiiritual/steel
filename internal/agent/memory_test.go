package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/spiiritual/steel/internal/llm"
)

// fakeMemory wakes each conversation with a message numbering the wake.
type fakeMemory struct {
	mu    sync.Mutex
	err   error
	wakes []string // the chat of each wake
}

func (m *fakeMemory) Wake(_ context.Context, chat string) ([]llm.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}
	m.wakes = append(m.wakes, chat)
	return []llm.Message{{Role: llm.RoleUser, Content: fmt.Sprintf("mem%d", len(m.wakes))}}, nil
}

func (m *fakeMemory) setErr(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = err
}

func TestMemoryWakesEachConversationOnceAfterTheSystemPrompt(t *testing.T) {
	mem := &fakeMemory{}
	dir := t.TempDir()
	h := newHarness(t, Config{System: "sys", Memory: mem, Dir: dir}, func(int, context.Context, llm.Request) (llm.Message, error) { return text("ok"), nil })
	h.ss.Send("c", "first")
	h.waitTurn()
	h.ss.Send("c", "second")
	h.waitTurn()
	if got, want := roles(h.llm.req(1).Messages), "system:sys user:mem1 user:first assistant:ok user:second"; got != want {
		t.Errorf("second turn:\n got %s\nwant %s", got, want)
	}

	// The wake is saved with the history: a restart doesn't wake again.
	h2 := newHarness(t, Config{System: "sys", Memory: mem, Dir: dir}, func(int, context.Context, llm.Request) (llm.Message, error) { return text("ok"), nil })
	h2.ss.Send("c", "third")
	h2.waitTurn()
	if got := roles(h2.llm.req(0).Messages); !strings.HasPrefix(got, "system:sys user:mem1 user:first") {
		t.Errorf("after restart: %s", got)
	}

	// /new is a new conversation, so it wakes again.
	h2.ss.Send("c", "/new")
	h2.ss.Send("c", "fourth")
	h2.waitTurn()
	if got, want := roles(h2.llm.req(1).Messages), "system:sys user:mem2 user:fourth"; got != want {
		t.Errorf("after /new:\n got %s\nwant %s", got, want)
	}
}

func TestFailedWakeFailsTheTurnAndIsRetried(t *testing.T) {
	mem := &fakeMemory{err: errors.New("disk gone")}
	h := newHarness(t, Config{Memory: mem}, func(int, context.Context, llm.Request) (llm.Message, error) { return text("ok"), nil })
	h.ss.Send("c", "hi")
	if d := h.waitTurn(); d.Err == nil || !strings.Contains(d.Err.Error(), "disk gone") {
		t.Errorf("turn error = %v", d.Err)
	}
	if n := h.llm.calls(); n != 0 {
		t.Errorf("%d model calls without the memory", n)
	}

	mem.setErr(nil)
	h.ss.Send("c", "again")
	h.waitTurn()
	if got, want := roles(h.llm.req(0).Messages), "user:mem1 user:hi user:again"; got != want {
		t.Errorf("retry:\n got %s\nwant %s", got, want)
	}
}

func (f *fakeLLM) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}
