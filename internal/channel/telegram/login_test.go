package telegram

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeLogin struct{ flow *fakeFlow }

func (l fakeLogin) Begin() (LoginFlow, error) { return l.flow, nil }
func (l fakeLogin) Logout() (string, error)   { return "Logged out", nil }

type fakeFlow struct {
	mu   sync.Mutex
	code string
	err  error
}

func (f *fakeFlow) URL() string { return "https://claude.ai/oauth/authorize?state=test" }

func (f *fakeFlow) Finish(ctx context.Context, code string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.code = code
	return "Logged in to Claude", f.err
}

func (f *fakeFlow) got() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.code
}

func TestLoginSendsURLAndTakesNextMessage(t *testing.T) {
	f := newFakeAPI(t)
	b, _, _ := testBot(t, f, 42)
	flow := &fakeFlow{}
	b.SetLogin(fakeLogin{flow: flow})
	ss := &fakeSessions{}
	start(t, b, ss)
	eventually(t, "polling", b.polling.Load)

	f.add(textUpdate(1, 42, 42, "/login"))
	eventually(t, "url", func() bool { return len(f.callsOf("sendMessage")) == 1 })
	text := f.callsOf("sendMessage")[0].Params["text"].(string)
	if !strings.Contains(text, "https://claude.ai/oauth/authorize?state=test") {
		t.Errorf("message = %q", text)
	}

	f.add(textUpdate(2, 42, 42, "abc#state"))
	eventually(t, "code", func() bool { return flow.got() == "abc#state" })
	eventually(t, "deleted", func() bool { return len(f.callsOf("deleteMessage")) == 1 })
	eventually(t, "notice", func() bool { return len(f.callsOf("sendMessage")) == 2 })
	if text := f.callsOf("sendMessage")[1].Params["text"].(string); !strings.Contains(text, "Logged in to Claude") {
		t.Errorf("notice = %q", text)
	}
	if got := ss.got(); len(got) != 0 {
		t.Errorf("the code reached the session: %q", got)
	}
}

func TestLoginWithoutOAuthSaysSo(t *testing.T) {
	f := newFakeAPI(t)
	b, _, _ := testBot(t, f, 42)
	ss := &fakeSessions{}
	start(t, b, ss)
	eventually(t, "polling", b.polling.Load)

	f.add(textUpdate(1, 42, 42, "/login"))
	eventually(t, "notice", func() bool { return len(f.callsOf("sendMessage")) == 1 })
	if text := f.callsOf("sendMessage")[0].Params["text"].(string); !strings.Contains(text, "isn't available") {
		t.Errorf("message = %q", text)
	}
}

func TestLoginTimesOut(t *testing.T) {
	f := newFakeAPI(t)
	b, _, _ := testBot(t, f, 42)
	b.secretTimeout = 20 * time.Millisecond
	b.SetLogin(fakeLogin{flow: &fakeFlow{}})
	ss := &fakeSessions{}
	start(t, b, ss)
	eventually(t, "polling", b.polling.Load)

	f.add(textUpdate(1, 42, 42, "/login"))
	eventually(t, "expired", func() bool {
		sent := f.callsOf("sendMessage")
		return len(sent) == 2 && strings.Contains(sent[1].Params["text"].(string), "timed out")
	})
	// A later message is delivered again, not taken for a code.
	f.add(textUpdate(2, 42, 42, "hello"))
	eventually(t, "message", func() bool { return len(ss.got()) == 1 })
}

// In a group, only the user who sent /login has their next message taken.
func TestLoginTakesOnlyItsUsersCode(t *testing.T) {
	f := newFakeAPI(t)
	b, _, _ := testBot(t, f, 42, 43)
	flow := &fakeFlow{}
	b.SetLogin(fakeLogin{flow: flow})
	ss := &fakeSessions{}
	start(t, b, ss)
	eventually(t, "polling", b.polling.Load)

	f.add(textUpdate(1, 42, -100, "/login"))
	eventually(t, "url", func() bool { return len(f.callsOf("sendMessage")) == 1 })
	f.add(textUpdate(2, 43, -100, "what's the weather?"))
	eventually(t, "other user's message", func() bool { return len(ss.got()) == 1 })
	f.add(textUpdate(3, 42, -100, "abc#state"))
	eventually(t, "code", func() bool { return flow.got() == "abc#state" })
	if n := len(f.callsOf("deleteMessage")); n > 1 {
		t.Errorf("%d messages deleted", n)
	}
}

type fakeModels struct {
	mu  sync.Mutex
	set [][2]string
}

func (m *fakeModels) Describe() string { return "model: m" }

func (m *fakeModels) SetModel(name string) (string, error)        { return m.record("", name) }
func (m *fakeModels) SetCompactModel(name string) (string, error) { return m.record("compact", name) }
func (m *fakeModels) SetEffort(level string) (string, error)      { return m.record("effort", level) }

func (m *fakeModels) record(what, value string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.set = append(m.set, [2]string{what, value})
	return "set " + what + " " + value, nil
}

func (m *fakeModels) got() [][2]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([][2]string(nil), m.set...)
}

func TestModelCommand(t *testing.T) {
	f := newFakeAPI(t)
	b, _, _ := testBot(t, f, 42)
	models := &fakeModels{}
	b.SetModels(models)
	ss := &fakeSessions{}
	start(t, b, ss)
	eventually(t, "polling", b.polling.Load)

	f.add(textUpdate(1, 42, 42, "/model"))
	f.add(textUpdate(2, 42, 42, "/model claude-opus-5-5"))
	f.add(textUpdate(3, 42, 42, "/model compact claude-haiku-5-5"))
	f.add(textUpdate(4, 42, 42, "/model a b c"))
	f.add(textUpdate(5, 42, 42, "/model effort high"))
	f.add(textUpdate(6, 42, 42, "/model effort")) // asks, doesn't set a model named "effort"
	eventually(t, "replies", func() bool { return len(f.callsOf("sendMessage")) == 6 })
	want := [][2]string{{"", "claude-opus-5-5"}, {"compact", "claude-haiku-5-5"}, {"effort", "high"}}
	if got := models.got(); !slices.Equal(got, want) {
		t.Errorf("set = %v, want %v", got, want)
	}
	sent := f.callsOf("sendMessage")
	if text := sent[0].Params["text"].(string); text != "model: m" {
		t.Errorf("describe = %q", text)
	}
	if text := sent[3].Params["text"].(string); !strings.Contains(text, "Usage") {
		t.Errorf("bad args = %q", text)
	}
	if text := sent[5].Params["text"].(string); text != "model: m" {
		t.Errorf("/model effort = %q", text)
	}
}
