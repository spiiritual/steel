package telegram

import (
	"context"
	"strings"
	"time"

	"github.com/spiiritual/steel/internal/agent"
)

// SetLogin makes /login start a Claude OAuth login, and /logout end it.
// Call it before Run.
func (b *Bot) SetLogin(l Login) { b.login = l }

// SetModels enables /model. Call it before Run.
func (b *Bot) SetModels(m Models) { b.models = m }

// loginWait is a /login waiting for the code Anthropic shows from the user
// who sent it; in a group, other people's messages pass through.
type loginWait struct {
	flow  LoginFlow
	user  int64
	timer *time.Timer
}

// beginLogin sends the authorization URL and waits for the next message.
func (b *Bot) beginLogin(p place, user int64) {
	if b.login == nil {
		b.push(p, agent.Notice{Text: "Claude login isn't available here."})
		return
	}
	flow, err := b.login.Begin()
	if err != nil {
		b.push(p, agent.Notice{Text: "Couldn't start the Claude login: " + err.Error()})
		return
	}
	w := &loginWait{flow: flow, user: user}
	w.timer = time.AfterFunc(b.secretTimeout, func() {
		b.mu.Lock()
		timedOut := b.takeLoginLocked(p, func(l *loginWait) bool { return l == w }) != nil
		b.mu.Unlock()
		if timedOut {
			b.push(p, agent.Notice{Text: "The Claude login timed out; send /login to start again."})
		}
	})
	b.mu.Lock()
	old := b.logins[p]
	b.logins[p] = w
	b.mu.Unlock()
	if old != nil {
		old.timer.Stop()
	}
	b.push(p, agent.Notice{Text: "Open this URL in a browser, sign in, and send the code Anthropic shows as your next message here. I'll delete it from the chat.\n\n" + flow.URL()})
}

// takeLoginLocked removes and returns the login waiting in p if match
// accepts it. Requires b.mu.
func (b *Bot) takeLoginLocked(p place, match func(*loginWait) bool) *loginWait {
	w := b.logins[p]
	if w == nil || !match(w) {
		return nil
	}
	delete(b.logins, p)
	w.timer.Stop()
	return w
}

// captureLogin takes m as the code for a waiting /login, reporting whether
// it did. The message is deleted, like a secret's.
func (b *Bot) captureLogin(p place, m *message) bool {
	if strings.HasPrefix(m.Text, "/") {
		return false
	}
	b.mu.Lock()
	w := b.takeLoginLocked(p, func(w *loginWait) bool { return w.user == m.From.ID })
	if w == nil {
		b.mu.Unlock()
		return false
	}
	if b.closing {
		b.mu.Unlock()
		// Too late to delete it: Close no longer waits for new work.
		b.push(p, agent.Notice{Text: "steel is shutting down, so I couldn't use or delete your code: delete it yourself, and send /login again once steel is back."})
		return true
	}
	// Tracked so Close waits for the exchange: the code is single-use, and
	// cancelling midway would spend it without saving tokens.
	b.wg.Add(1)
	b.mu.Unlock()

	b.deletePrivateMessage(p, m, "a login code")
	go func() {
		defer b.wg.Done()
		ctx, cancel := context.WithTimeout(b.ctx, time.Minute)
		defer cancel()
		text, err := w.flow.Finish(ctx, strings.TrimSpace(m.Text))
		if err != nil {
			b.push(p, agent.Notice{Text: "Claude login failed: " + err.Error()})
			return
		}
		b.push(p, agent.Notice{Text: text})
	}()
	return true
}

func (b *Bot) logout(p place) {
	if b.login == nil {
		b.push(p, agent.Notice{Text: "Claude login isn't available here."})
		return
	}
	text, err := b.login.Logout()
	if err != nil {
		text = "Couldn't log out of Claude: " + err.Error()
	}
	b.push(p, agent.Notice{Text: text})
}

// model handles /model, /model NAME, /model effort LEVEL and /model
// compact NAME. Without a value, it describes what's in use.
func (b *Bot) model(p place, args string) {
	if b.models == nil {
		b.push(p, agent.Notice{Text: "The model can't be changed here."})
		return
	}
	words := strings.Fields(args)
	var text string
	var err error
	switch {
	case len(words) == 0, len(words) == 1 && (words[0] == "compact" || words[0] == "effort"):
		text = b.models.Describe()
	case len(words) == 1:
		text, err = b.models.SetModel(words[0])
	case len(words) == 2 && words[0] == "compact":
		text, err = b.models.SetCompactModel(words[1])
	case len(words) == 2 && words[0] == "effort":
		text, err = b.models.SetEffort(words[1])
	default:
		text = "Usage: /model NAME, /model effort LEVEL, or /model compact NAME for memory's summaries."
	}
	if err != nil {
		text = "Couldn't change the model: " + err.Error()
	}
	b.push(p, agent.Notice{Text: text})
}
