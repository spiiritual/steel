package telegram

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Schedules lets the user list and cancel schedules with /schedules,
// without going through the model.
type Schedules interface {
	Overview() []ScheduleEntry
	// CancelSchedule deletes schedule id, returning its name.
	CancelSchedule(ctx context.Context, id string) (string, error)
}

// ScheduleEntry is one schedule in /schedules.
type ScheduleEntry struct {
	ID   string
	Name string
	Text string // one line: timing, next run, what it does
	From string // the session it was made in
}

// chatSchedules returns the schedules made in chat: /schedules in a group
// mustn't show or cancel what was scheduled in private.
func (b *Bot) chatSchedules(chat int64) []ScheduleEntry {
	var out []ScheduleEntry
	for _, e := range b.schedules.Overview() {
		if p, ok := parsePlace(e.From); ok && p.chat == chat {
			out = append(out, e)
		}
	}
	return out
}

// SetSchedules makes /schedules list and cancel s's schedules. Call it
// before Run.
func (b *Bot) SetSchedules(s Schedules) { b.schedules = s }

// cancelSchedulePrefix starts the callback data of /schedules' buttons.
const cancelSchedulePrefix = "unschedule:"

// buttons is a renderer input: a message with an inline keyboard.
type buttons struct {
	text     string
	keyboard *inlineKeyboard
}

// scheduleList is /schedules' message for chat.
func (b *Bot) scheduleList(chat int64) buttons {
	if b.schedules == nil {
		return buttons{text: "Schedules aren't available."}
	}
	entries := b.chatSchedules(chat)
	if len(entries) == 0 {
		return buttons{text: "No schedules. Ask me for one, e.g. \"remind me at 5pm to call mom\" or \"every weekday at 8, summarize my inbox\"."}
	}
	var text strings.Builder
	text.WriteString("Schedules:\n")
	kb := &inlineKeyboard{}
	for _, e := range entries {
		fmt.Fprintf(&text, "\n• %s", e.Text)
		kb.InlineKeyboard = append(kb.InlineKeyboard, []inlineButton{{Text: "Cancel " + e.Name, CallbackData: cancelSchedulePrefix + e.ID}})
	}
	return buttons{text: text.String(), keyboard: kb}
}

// cancelSchedule handles a press of a /schedules button: it cancels the
// schedule and updates the list.
func (b *Bot) cancelSchedule(q *callbackQuery, id string, reply func(string)) {
	if b.schedules == nil {
		reply("")
		return
	}
	ctx, cancel := context.WithTimeout(b.ctx, requestTimeout)
	defer cancel()
	chat := q.Message.Chat.ID
	name, err := "", errors.New("not in this chat")
	if slices.ContainsFunc(b.chatSchedules(chat), func(e ScheduleEntry) bool { return e.ID == id }) {
		name, err = b.schedules.CancelSchedule(ctx, id)
	}
	if err != nil {
		reply("That schedule is already gone.")
	} else {
		reply("Cancelled " + name + ".")
	}
	// The list may have been in the cancelled schedule's topic, now gone.
	l := b.scheduleList(chat)
	b.api.callRetry(ctx, "editMessageText", editMessageTextParams{ChatID: chat, MessageID: q.Message.MessageID, Text: l.text, ReplyMarkup: l.keyboard}, nil, requestTimeout, b.sendAttempts)
}

// CreatePlace makes a topic titled title in the chat of session near and
// returns the topic's session ID. It fails if the chat has no topics.
func (b *Bot) CreatePlace(ctx context.Context, near, title string) (string, error) {
	p, ok := parsePlace(near)
	if !ok {
		return "", fmt.Errorf("%q isn't a Telegram conversation", near)
	}
	var t forumTopic
	if err := b.api.call(ctx, "createForumTopic", createForumTopicParams{ChatID: p.chat, Name: title}, &t, requestTimeout); err != nil {
		return "", err
	}
	if t.MessageThreadID == 0 {
		return "", errors.New("createForumTopic: no topic in the response")
	}
	return place{chat: p.chat, topic: t.MessageThreadID}.sessionID(), nil
}

// EnsurePlace checks topic id still exists, and makes a new one titled
// title if the user deleted it. The check sets the topic's name to title:
// the only harmless call found that fails for a deleted topic (typing and
// empty edits succeed). Users can't rename a bot's topics, so it changes
// nothing.
func (b *Bot) EnsurePlace(ctx context.Context, id, title string) (string, error) {
	p, ok := parsePlace(id)
	if !ok || p.topic == 0 {
		return id, nil
	}
	err := b.api.callRetry(ctx, "editForumTopic", editForumTopicParams{ChatID: p.chat, MessageThreadID: p.topic, Name: title}, nil, requestTimeout, b.sendAttempts)
	if err == nil || !topicGone(err) {
		return id, err
	}
	b.logf("%s: topic gone; making a new one for %q", p, title)
	return b.CreatePlace(ctx, place{chat: p.chat}.sessionID(), title)
}

// DeletePlace deletes a topic made by CreatePlace.
func (b *Bot) DeletePlace(ctx context.Context, id string) error {
	p, ok := parsePlace(id)
	if !ok || p.topic == 0 {
		return fmt.Errorf("%q isn't a Telegram topic", id)
	}
	return b.api.callRetry(ctx, "deleteForumTopic", deleteForumTopicParams{ChatID: p.chat, MessageThreadID: p.topic}, nil, requestTimeout, b.sendAttempts)
}

// topicGone reports whether err says a topic no longer exists.
func topicGone(err error) bool {
	var ae *apiError
	if !errors.As(err, &ae) || ae.transient() {
		return false
	}
	d := strings.ToLower(ae.Description)
	// Seen live: TOPIC_ID_INVALID from editForumTopic, "message thread not
	// found" from sendMessage.
	return strings.Contains(d, "topic_id_invalid") || strings.Contains(d, "thread not found") || strings.Contains(d, "topic_deleted")
}
