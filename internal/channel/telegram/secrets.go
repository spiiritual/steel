package telegram

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"

	"github.com/spiiritual/steel/internal/agent"
)

// Secrets saves the secrets users enter with /secret.
type Secrets interface {
	// CheckName reports why name can't be set, or nil if it can.
	CheckName(name string) error
	Set(name, value string) error
}

// SetSecrets makes /secret save into s. Call it before Run.
func (b *Bot) SetSecrets(s Secrets) { b.secrets = s }

// A secret's value is the next text message in the place asking for it,
// from any allowed user. It's deleted from the chat and goes to the
// session (a model's SecretRequest) or the store (/secret), never into a
// conversation. Commands still work meanwhile, so /stop can end the wait.

// secretMarker starts every secret prompt, so a reply to one is never
// taken for a message, even after the request is gone (e.g. a restart).
const secretMarker = "🔑"

// secretPrompt starts the prompt for secret name; a reply's prompt names
// the secret it was for.
func secretPrompt(name string) string { return secretMarker + " steel needs " + name }

// cancelSecretPrefix starts the callback data of a secret prompt's Cancel
// button.
const cancelSecretPrefix = "unsecret:"

// secretWait is a secret request waiting for its value in a place.
type secretWait struct {
	id   string
	name string
	// own: started by /secret, so the bot saves the value itself and ends
	// the request, after timer if nobody answers. Otherwise a session
	// waits for the value and reports the end with SecretDone.
	own   bool
	timer *time.Timer
}

// waitSecret makes w the request waiting in p, ending any user-started
// one already there; a session asks for one secret at a time.
func (b *Bot) waitSecret(p place, w *secretWait) {
	b.mu.Lock()
	old := b.waits[p]
	b.waits[p] = w
	b.mu.Unlock()
	if old != nil && old.own {
		old.timer.Stop()
		b.push(p, agent.SecretDone{ID: old.id, Outcome: agent.SecretCancelled})
	}
}

// takeSecret removes the request waiting in p, if its ID is id, and
// returns it.
func (b *Bot) takeSecret(p place, id string) *secretWait {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.takeSecretLocked(p, func(w *secretWait) bool { return w.id == id })
}

// takeSecretLocked removes and returns the request waiting in p if match
// accepts it. Requires b.mu.
func (b *Bot) takeSecretLocked(p place, match func(*secretWait) bool) *secretWait {
	w := b.waits[p]
	if w == nil || !match(w) {
		return nil
	}
	delete(b.waits, p)
	if w.timer != nil {
		w.timer.Stop()
	}
	return w
}

// askSecret handles /secret NAME.
func (b *Bot) askSecret(p place, name string) {
	if b.secrets == nil {
		b.push(p, agent.Notice{Text: "Secrets can't be saved from here."})
		return
	}
	if name == "" {
		b.push(p, agent.Notice{Text: "Send /secret NAME, e.g. /secret GITHUB_TOKEN, then the value as your next message. It's saved for tools to use and never shown to the model."})
		return
	}
	if err := b.secrets.CheckName(name); err != nil {
		b.push(p, agent.Notice{Text: err.Error()})
		return
	}
	var r [8]byte
	rand.Read(r[:])
	w := &secretWait{id: "u" + hex.EncodeToString(r[:]), name: name, own: true}
	// Checked and claimed at once, so a session's request can't slip in
	// between and be overwritten.
	b.mu.Lock()
	busy := b.waits[p] != nil
	if !busy {
		b.waits[p] = w
		w.timer = time.AfterFunc(b.secretTimeout, func() {
			if b.takeSecret(p, w.id) != nil {
				b.push(p, agent.SecretDone{ID: w.id, Outcome: agent.SecretExpired})
			}
		})
	}
	b.mu.Unlock()
	if busy {
		b.push(p, agent.Notice{Text: "A secret is already waiting to be entered here: send it, or press Cancel on its request first."})
		return
	}
	b.push(p, agent.SecretRequest{ID: w.id, Name: name})
}

// captureSecret takes m as the value of the secret waiting in p, if any,
// reporting whether it did. A reply to a secret prompt is never a message
// either: it's the value only if the prompt is for the secret waiting now,
// and otherwise just deleted.
func (b *Bot) captureSecret(ss sender, p place, m *message) bool {
	r := m.ReplyToMessage
	toPrompt := r != nil && r.From != nil && r.From.IsBot && strings.HasPrefix(r.Text, secretMarker)
	if !toPrompt && strings.HasPrefix(m.Text, "/") {
		return false
	}
	b.mu.Lock()
	w := b.takeSecretLocked(p, func(w *secretWait) bool {
		prompt := secretPrompt(w.name)
		return !toPrompt || r.Text == prompt || strings.HasPrefix(r.Text, prompt+"\n")
	})
	b.mu.Unlock()
	if w == nil && !toPrompt {
		return false
	}
	b.deleteSecretMessage(p, m)
	value := strings.TrimSpace(m.Text)
	switch {
	case w != nil && w.own:
		done := agent.SecretDone{ID: w.id, Outcome: agent.SecretReceived}
		if err := b.secrets.Set(w.name, value); err != nil {
			done = agent.SecretDone{ID: w.id, Outcome: agent.SecretNotSaved, Problem: err.Error()}
		}
		b.push(p, done)
	case w == nil || !ss.ProvideSecret(p.sessionID(), w.id, value):
		b.push(p, agent.Notice{Text: "That request isn't waiting anymore, so nothing was saved."})
	}
	return true
}

// cancelSecret handles a press of a secret prompt's Cancel button.
func (b *Bot) cancelSecret(ss sender, p place, id string) bool {
	w := b.takeSecret(p, id)
	switch {
	case w == nil:
		return false
	case w.own:
		b.push(p, agent.SecretDone{ID: w.id, Outcome: agent.SecretDeclined})
		return true
	}
	return ss.CancelSecret(p.sessionID(), id)
}

// deleteSecretMessage deletes m, the user's message with a secret, in the
// background, so a slow Telegram doesn't hold up other updates, and tells
// the user if it couldn't.
func (b *Bot) deleteSecretMessage(p place, m *message) {
	b.mu.Lock()
	if b.closing {
		b.mu.Unlock()
		return
	}
	b.wg.Add(1)
	b.mu.Unlock()
	go func() {
		defer b.wg.Done()
		ctx, cancel := context.WithTimeout(b.ctx, requestTimeout)
		defer cancel()
		err := b.api.callRetry(ctx, "deleteMessage", deleteMessageParams{ChatID: m.Chat.ID, MessageID: m.MessageID}, nil, requestTimeout, b.sendAttempts)
		if err != nil && b.ctx.Err() == nil {
			b.logf("%s: deleting a message with a secret: %v", p, err)
			b.push(p, agent.Notice{Text: "I couldn't delete your message with the secret: delete it yourself."})
		}
	}()
}

// askSecret sends a secret prompt with a Cancel button.
func (c *chat) askSecret(e agent.SecretRequest) {
	text := secretPrompt(e.Name)
	if e.Purpose != "" {
		text += "\nFor: " + firstLine(e.Purpose, 300)
	}
	text += "\n\nSend it as your next message here. I'll delete it from the chat and save it for tools to use; the model never sees it."
	if !c.sendRequest(e.ID, text, []inlineButton{{Text: "Cancel", CallbackData: cancelSecretPrefix + e.ID}}) {
		// Unseen, it can't be answered: stop taking messages for it.
		// The session's request expires; the bot's own ends here.
		if w := c.b.takeSecret(c.place, e.ID); w != nil && w.own {
			c.b.push(c.place, agent.SecretDone{ID: w.id, Outcome: agent.SecretExpired})
		}
	}
}

// secretDone replaces a secret prompt's button with its outcome.
func (c *chat) secretDone(e agent.SecretDone) {
	outcome := map[agent.SecretOutcome]string{
		agent.SecretReceived:  "✅ Saved",
		agent.SecretDeclined:  "Cancelled",
		agent.SecretExpired:   "⌛ Nothing was sent in time; nothing was saved",
		agent.SecretCancelled: "Cancelled",
	}[e.Outcome]
	if e.Outcome == agent.SecretNotSaved {
		outcome = "❌ Not saved: " + e.Problem
	}
	c.settle(e.ID, outcome)
}
