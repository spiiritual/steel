package telegram

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spiiritual/steel/internal/agent"
)

// Updates updates steel, for /update and /rollback. Both ask before doing
// anything, with a button: an update restarts steel.
type Updates interface {
	Status(ctx context.Context) (UpdateStatus, error)
	// Update starts updating steel to the newest release, and Rollback
	// starts going back to the previous version. Each returns what to tell
	// the user now; how it went is reported later to session from.
	Update(ctx context.Context, from string) (string, error)
	Rollback(ctx context.Context, from string) (string, error)
}

// UpdateStatus describes steel's versions.
type UpdateStatus struct {
	Current string
	// Latest is the newest release, if known; Available means it's newer
	// than Current. LatestError says why Latest is unknown.
	Latest      string
	LatestError string
	Available   bool
	Previous    string // what a rollback returns to; empty if nothing
	Busy        bool   // an update or rollback is under way
}

// SetUpdates enables /update and /rollback. Call it before Run.
func (b *Bot) SetUpdates(u Updates) { b.updates = u }

// releaseURL is where a version's release notes are.
const releaseURL = "https://github.com/spiiritual/steel/releases/tag/"

// Callback data of the confirm buttons. An update's names the version
// offered, though the updater always installs the newest.
const (
	updatePrefix   = "update:"
	rollbackPrefix = "rollback:"
)

// noUpdates is the reply to /update and /rollback without an updater.
const noUpdates = "steel can't update itself here. In Docker, add the updater service from steel's compose.yaml; elsewhere, run `steel update` where steel runs."

// updateOffer is what /update shows, and what's announced when a release
// comes out.
func updateOffer(s UpdateStatus) buttons {
	switch {
	case s.Busy:
		return buttons{text: "An update is under way; I'll say here how it went."}
	case s.Available:
		return buttons{
			text: fmt.Sprintf("steel %s is out; this is %s. Updating restarts me once I've finished what I'm doing.", s.Latest, s.Current),
			keyboard: &inlineKeyboard{InlineKeyboard: [][]inlineButton{{
				{Text: "Update to " + s.Latest, CallbackData: updatePrefix + s.Latest},
				{Text: "What's new", URL: releaseURL + s.Latest},
			}}},
		}
	case s.LatestError != "":
		return buttons{text: fmt.Sprintf("This is steel %s. I couldn't check for a newer release: %s", s.Current, s.LatestError)}
	}
	text := fmt.Sprintf("steel %s is up to date.", s.Current)
	if s.Previous != "" {
		text += fmt.Sprintf(" /rollback goes back to %s.", s.Previous)
	}
	return buttons{text: text}
}

// rollbackOffer is what /rollback shows.
func rollbackOffer(s UpdateStatus) buttons {
	switch {
	case s.Busy:
		return buttons{text: "An update is under way; I'll say here how it went."}
	case s.Previous == "":
		return buttons{text: fmt.Sprintf("This is steel %s, and there's no previous version to roll back to.", s.Current)}
	}
	return buttons{
		text: fmt.Sprintf("This is steel %s. Rolling back to %s restarts me once I've finished what I'm doing.", s.Current, s.Previous),
		keyboard: &inlineKeyboard{InlineKeyboard: [][]inlineButton{{
			{Text: "Roll back to " + s.Previous, CallbackData: rollbackPrefix + s.Previous},
		}}},
	}
}

// offerUpdate answers /update or /rollback with the status and a button.
func (b *Bot) offerUpdate(p place, cmd string) {
	if b.updates == nil {
		b.push(p, agent.Notice{Text: noUpdates})
		return
	}
	b.async(func(ctx context.Context) {
		s, err := b.updates.Status(ctx)
		if err != nil {
			b.push(p, agent.Notice{Text: "Couldn't ask the updater: " + err.Error()})
			return
		}
		if cmd == "/rollback" {
			b.push(p, rollbackOffer(s))
		} else {
			b.push(p, updateOffer(s))
		}
	})
}

// AnnounceUpdate tells each allowed user, in their private chat, that a
// release is out, with the button to install it.
func (b *Bot) AnnounceUpdate(s UpdateStatus) {
	for id := range b.allowed {
		b.push(place{chat: id}, updateOffer(s))
	}
}

// startUpdate handles a press of an update or rollback button: it starts
// the work and puts what's happening in place of the offer.
func (b *Bot) startUpdate(q *callbackQuery, action string, reply func(string)) {
	reply("")
	if b.updates == nil {
		return
	}
	p := placeOf(q.Message)
	b.async(func(ctx context.Context) {
		start := b.updates.Update
		if action == "rollback" {
			start = b.updates.Rollback
		}
		text, err := start(ctx, p.sessionID())
		if err != nil {
			text = "Couldn't start the " + action + ": " + err.Error()
		}
		b.api.callRetry(ctx, "editMessageText", editMessageTextParams{ChatID: q.Message.Chat.ID, MessageID: q.Message.MessageID, Text: text}, nil, requestTimeout, b.sendAttempts)
	})
}

// async runs f off the update loop, which mustn't wait on the network.
// Close waits for it.
func (b *Bot) async(f func(ctx context.Context)) {
	b.mu.Lock()
	if b.closing {
		b.mu.Unlock()
		return
	}
	b.wg.Add(1)
	b.mu.Unlock()
	go func() {
		defer b.wg.Done()
		ctx, cancel := context.WithTimeout(b.ctx, time.Minute)
		defer cancel()
		f(ctx)
	}()
}

// updateAction returns the action a button's callback data starts.
func updateAction(data string) (string, bool) {
	switch {
	case strings.HasPrefix(data, updatePrefix):
		return "update", true
	case strings.HasPrefix(data, rollbackPrefix):
		return "rollback", true
	}
	return "", false
}
