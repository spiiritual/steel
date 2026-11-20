package telegram

import (
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/spiiritual/steel/internal/agent"
)

// maxText is Telegram's limit for message and draft text, in UTF-16 code
// units (how the Bot API counts characters).
const maxText = 4096

// requestTimeout bounds every request except getUpdates.
const requestTimeout = 30 * time.Second

// Renderer inputs besides agent events.
type (
	// userSent: a user message was handed to the session; a turn is coming.
	userSent struct{}
	// stopping: /stop or /new is about to reach the session.
	stopping struct{}
)

// chat renders the events of one place: a chat, or a topic in it. Events
// queue without blocking and are applied in order by its own goroutine,
// which does all the
// network I/O: replies and notices become messages; streamed text and tool
// activity show in a draft (private chats) or as a typing indicator
// (groups, where drafts aren't available).
type chat struct {
	b     *Bot
	place place

	mu    sync.Mutex
	queue []any
	wake  chan struct{}

	// Owned by run.
	drafts                           bool   // the chat can show drafts
	active                           bool   // a turn is running
	stopped                          bool   // the user stopped the turn; ignore its stragglers until TurnDone
	text                             string // reply text streamed since the last sent message
	status                           string // latest tool activity
	draftID                          int64
	draftSeq                         uint32 // draftID's counter part
	dirty                            bool   // the draft is out of date
	showing                          bool   // a draft is on screen
	lastDraft, nextDraft, nextTyping time.Time
	requests                         map[string]requestMessage // approval and secret requests by ID, while waiting
}

// maxApprovalAction is how much of a command an approval request shows,
// in UTF-16 code units, leaving room for its reasons.
const maxApprovalAction = 3000

// requestMessage is a sent ApprovalRequest or SecretRequest.
type requestMessage struct {
	id   int64 // message ID
	text string
}

func newChat(b *Bot, p place) *chat {
	// Private chats, the only ones with drafts, share their user's
	// positive ID; group IDs are negative.
	c := &chat{b: b, place: p, wake: make(chan struct{}, 1), drafts: p.chat > 0}
	c.newDraft()
	return c
}

// newDraft moves on to a new draft: a sent message replaced the old one.
func (c *chat) newDraft() {
	c.draftSeq++
	if c.draftSeq == 0 { // wrapped; draft IDs must be non-zero
		c.draftSeq = 1
	}
	c.draftID = draftID(c.place.topic, c.draftSeq)
}

// idle reports whether the renderer holds nothing worth keeping: no turn,
// draft on screen, or request waiting for its outcome.
func (c *chat) idle() bool {
	return !c.active && !c.showing && len(c.requests) == 0
}

// remove takes the renderer out of the bot's map unless an event arrived;
// the next event starts a new one.
func (c *chat) remove() bool {
	c.b.mu.Lock()
	defer c.b.mu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.queue) > 0 || c.b.chats[c.place] != c {
		return false
	}
	delete(c.b.chats, c.place)
	return true
}

func (c *chat) push(e any) {
	c.mu.Lock()
	c.queue = append(c.queue, e)
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *chat) run() {
	defer c.b.wg.Done()
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	closing := false
	for {
		c.mu.Lock()
		q := c.queue
		c.queue = nil
		c.mu.Unlock()
		for _, e := range q {
			c.apply(e)
		}
		if c.b.ctx.Err() != nil {
			return
		}
		if closing {
			if len(q) == 0 {
				return
			}
			continue // deliver what's left, without drafts
		}
		var tick <-chan time.Time
		wait, ok := c.tick()
		idle := !ok && c.idle()
		if idle {
			wait, ok = c.b.chatIdle, true
		}
		if ok {
			timer.Reset(wait)
			tick = timer.C
		}
		select {
		case <-c.wake:
		case <-tick:
			if idle && c.remove() {
				return
			}
		case <-c.b.closingCh:
			closing = true
		case <-c.b.ctx.Done():
			return
		}
	}
}

func (c *chat) apply(e any) {
	switch e := e.(type) {
	case userSent:
		c.stopped = false
		c.activate()
	case stopping:
		// Keep what the user saw of the reply, ahead of "Stopped.".
		c.flush()
		c.active, c.stopped, c.dirty, c.status = false, true, false, ""
	case agent.TextDelta:
		if c.stopped {
			return // the rest of a reply the user stopped
		}
		c.text += e.Text
		c.status = ""
		c.activate()
		c.dirty = true
	case agent.Reply:
		c.text, c.status = "", ""
		c.sendFormatted(markdown(e.Text), nil)
		// The message replaced the draft. Don't send an empty one (a
		// "Thinking…" placeholder) or a typing indicator before TurnDone;
		// a following tool call or text brings the draft back.
		c.dirty = false
		c.nextTyping = time.Now().Add(c.b.typingInterval)
	case agent.ToolStart:
		c.status = "⚙ " + e.Name
		// Nested calls can come from a background job after its turn
		// ended; they update the status but don't make an idle chat look
		// busy, since a dropped job (/reset) never ends with a TurnDone.
		if !e.Nested {
			c.activate()
		}
		c.dirty = c.active
	case agent.ToolEnd:
		if e.Err != nil {
			c.status = "⚠ " + e.Name + " failed"
			c.dirty = c.active
		}
	case agent.Notice:
		c.send(e.Text)
		c.dirty = c.active // the message replaced the draft; bring it back
	case buttons:
		c.sendButtons(e)
		c.dirty = c.active
	case agent.ApprovalRequest:
		c.flush() // what the model said so far explains the request
		c.askApproval(e)
		c.dirty = c.active
	case agent.ApprovalDone:
		c.approvalDone(e)
	case agent.SecretRequest:
		c.flush() // what the model said so far explains the request
		c.askSecret(e)
		c.dirty = c.active
	case agent.SecretDone:
		c.secretDone(e)
	case agent.TurnDone:
		c.flush() // e.g. a reply cut off by an error
		if e.Err != nil {
			c.send("Error: " + firstLine(e.Err.Error(), 300))
		}
		c.active, c.stopped, c.dirty, c.status = false, false, false, ""
	}
}

func (c *chat) activate() {
	if c.stopped || c.active {
		return
	}
	c.active, c.dirty = true, true
	c.nextTyping = time.Time{}
}

// flush sends the streamed reply text not yet sent as a message.
func (c *chat) flush() {
	t := c.text
	c.text = ""
	c.sendFormatted(markdown(t), nil)
}

// send delivers steel's own text, unformatted, as one or more messages.
func (c *chat) send(text string) { c.sendFormatted(plain(text), nil) }

// sendButtons sends a message with an inline keyboard.
func (c *chat) sendButtons(m buttons) { c.sendFormatted(plain(m.text), m.keyboard) }

// sendFormatted sends f, split as needed, with kb under its last part. A
// sent message replaces the draft, so the next draft gets a new ID.
func (c *chat) sendFormatted(f formatted, kb *inlineKeyboard) {
	parts := splitRanges(f.text, maxText)
	sent := 0
	for i, r := range parts {
		params := sendMessageParams{ChatID: c.place.chat, MessageThreadID: c.place.topic, Text: f.text[r[0]:r[1]], Entities: f.entities(r[0], r[1]), LinkPreview: noPreview}
		if i == len(parts)-1 {
			params.ReplyMarkup = kb
		}
		err := c.b.api.callRetry(c.b.ctx, "sendMessage", params, nil, requestTimeout, c.b.sendAttempts)
		if badEntities(err) && params.Entities != nil {
			// The text matters more than its looks.
			c.b.logf("%s: formatting refused (%v); sending it plain", c.place, err)
			params.Entities = nil
			err = c.b.api.callRetry(c.b.ctx, "sendMessage", params, nil, requestTimeout, c.b.sendAttempts)
		}
		if err != nil {
			if c.b.ctx.Err() == nil {
				c.b.logf("%s: message not delivered: %v", c.place, err)
			}
			break
		}
		sent++
	}
	if sent > 0 {
		c.showing = false
		c.newDraft()
	}
}

// badEntities reports whether Telegram refused a message's entities, as
// opposed to the message.
func badEntities(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.Code == http.StatusBadRequest && strings.Contains(strings.ToLower(ae.Description), "entit")
}

// askApproval sends an approval request with Allow and Refuse buttons.
func (c *chat) askApproval(e agent.ApprovalRequest) {
	action := e.Action
	if utf16Len(action) > maxApprovalAction {
		action = action[:prefix(action, maxApprovalAction)] + "\n… (cut; the rest runs too)"
	}
	text := "Allow this?\n\n" + action
	if len(e.Reasons) > 0 {
		text += "\n\nIt " + strings.Join(e.Reasons, "; ") + "."
	}
	// Nobody can answer an undelivered request; it expires.
	c.sendRequest(e.ID, text, []inlineButton{
		{Text: "Allow", CallbackData: allowPrefix + e.ID},
		{Text: "Refuse", CallbackData: refusePrefix + e.ID},
	})
}

// approvalDone replaces a request's buttons with its outcome.
func (c *chat) approvalDone(e agent.ApprovalDone) {
	c.settle(e.ID, map[agent.ApprovalOutcome]string{
		agent.Allowed:   "✅ Allowed",
		agent.Refused:   "❌ Refused",
		agent.Expired:   "⌛ No answer in time, so it didn't run",
		agent.Cancelled: "Cancelled",
	}[e.Outcome])
}

// sendRequest sends request id's message with buttons, keeping it until
// settle, and reports whether it was delivered.
func (c *chat) sendRequest(id, text string, buttons []inlineButton) bool {
	text = tail(text, maxText-100) // room for the outcome line
	var sent message
	err := c.b.api.callRetry(c.b.ctx, "sendMessage", sendMessageParams{
		ChatID:          c.place.chat,
		MessageThreadID: c.place.topic,
		Text:            text,
		LinkPreview:     noPreview,
		ReplyMarkup:     &inlineKeyboard{InlineKeyboard: [][]inlineButton{buttons}},
	}, &sent, requestTimeout, c.b.sendAttempts)
	if err != nil {
		if c.b.ctx.Err() == nil {
			c.b.logf("%s: request not delivered: %v", c.place, err)
		}
		return false
	}
	c.showing = false
	c.newDraft()
	if c.requests == nil {
		c.requests = map[string]requestMessage{}
	}
	c.requests[id] = requestMessage{id: sent.MessageID, text: text}
	return true
}

// settle replaces request id's buttons with its outcome. The text is kept
// in front, so a secret prompt still starts with its marker.
func (c *chat) settle(id, outcome string) {
	m, ok := c.requests[id]
	if !ok {
		return
	}
	delete(c.requests, id)
	err := c.b.api.callRetry(c.b.ctx, "editMessageText", editMessageTextParams{ChatID: c.place.chat, MessageID: m.id, Text: m.text + "\n\n" + outcome}, nil, requestTimeout, c.b.sendAttempts)
	if err != nil && c.b.ctx.Err() == nil {
		c.b.logf("%s: request not updated: %v", c.place, err)
	}
}

// tick sends a draft or typing indicator if one is due, and returns how
// long until the next is; ok is false if none is pending.
func (c *chat) tick() (wait time.Duration, ok bool) {
	if !c.active {
		return 0, false
	}
	now := time.Now()
	if c.drafts {
		due, pending := c.draftDue()
		if pending && !now.Before(due) {
			c.sendDraft(now)
		}
	}
	if !c.drafts { // possibly just turned off by a failed draft
		if !now.Before(c.nextTyping) {
			c.typing(now)
		}
		return c.nextTyping.Sub(now), true
	}
	due, pending := c.draftDue()
	return max(due.Sub(now), 0), pending
}

// draftDue returns when the draft should next be sent, if at all: when it
// changes (throttled), and periodically while shown so it doesn't expire.
func (c *chat) draftDue() (time.Time, bool) {
	switch {
	case c.dirty:
		return c.nextDraft, true
	case c.showing:
		return later(c.nextDraft, c.lastDraft.Add(c.b.draftRefresh)), true
	}
	return time.Time{}, false
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func (c *chat) sendDraft(now time.Time) {
	// Formatted as the reply will be. Half-written Markdown (an unclosed
	// "**") shows as typed until it closes.
	f := markdown(c.text)
	if c.status != "" {
		if f.text != "" {
			f.text += "\n\n"
		}
		f.text += c.status + "…"
	}
	// The end of a long draft, behind "…".
	text, entities := f.text, f.entities(0, len(f.text))
	if start := tailStart(f.text, maxText); start > 0 {
		text, entities = "…"+f.text[start:], f.entities(start, len(f.text))
		for i := range entities {
			entities[i].Offset++ // "…" is one UTF-16 unit
		}
	}
	stop := c.b.polling.Load() // the stop button works only while polling
	params := sendMessageDraftParams{
		ChatID:          c.place.chat,
		MessageThreadID: c.place.topic,
		DraftID:         c.draftID,
		Text:            text,
		Entities:        entities,
		CanStop:         stop,
		KeepOnStop:      stop,
	}
	err := c.b.api.call(c.b.ctx, "sendMessageDraft", params, nil, requestTimeout)
	if badEntities(err) && params.Entities != nil {
		params.Entities = nil
		err = c.b.api.call(c.b.ctx, "sendMessageDraft", params, nil, requestTimeout)
	}
	switch {
	case err == nil:
		c.showing, c.dirty = true, false
		c.lastDraft, c.nextDraft = now, now.Add(c.b.draftInterval)
	case retryAfter(err) > 0:
		c.nextDraft = now.Add(retryAfter(err))
	case permanent(err):
		c.drafts = false
		c.b.logf("%s: drafts unavailable (%v); showing a typing indicator instead", c.place, err)
	default: // transient; try again later
		c.nextDraft = now.Add(max(c.b.draftInterval, c.b.api.backoffMin))
	}
}

func (c *chat) typing(now time.Time) {
	c.nextTyping = now.Add(c.b.typingInterval)
	err := c.b.api.call(c.b.ctx, "sendChatAction", sendChatActionParams{ChatID: c.place.chat, MessageThreadID: c.place.topic, Action: "typing"}, nil, requestTimeout)
	if d := retryAfter(err); d > 0 {
		c.nextTyping = now.Add(d)
	}
}

// utf16Len returns s's length as Telegram counts it.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n++
		if r >= 0x10000 {
			n++
		}
	}
	return n
}

// prefix returns the byte length of s's longest prefix of at most limit
// UTF-16 code units.
func prefix(s string, limit int) int {
	n := 0
	for i, r := range s {
		w := 1
		if r >= 0x10000 {
			w = 2
		}
		if n+w > limit {
			return i
		}
		n += w
	}
	return len(s)
}

// split cuts text into messages of at most limit UTF-16 code units; see
// splitRanges.
func split(text string, limit int) []string {
	var parts []string
	for _, r := range splitRanges(text, limit) {
		parts = append(parts, text[r[0]:r[1]])
	}
	return parts
}

// splitRanges cuts text into parts of at most limit UTF-16 code units,
// preferring paragraph breaks, then line breaks, then spaces, and returns
// their byte ranges. The separator at a cut is dropped; blank parts are
// skipped.
func splitRanges(text string, limit int) [][2]int {
	var parts [][2]int
	base := 0
	for {
		rest := text[base:]
		if utf16Len(rest) <= limit {
			if strings.TrimSpace(rest) != "" {
				parts = append(parts, [2]int{base, len(text)})
			}
			return parts
		}
		cut := prefix(rest, limit)
		if cut == 0 { // limit is narrower than one character
			_, cut = utf8.DecodeRuneInString(rest)
		}
		end, next := cut, cut
		for _, sep := range []string{"\n\n", "\n", " "} {
			// Only in the second half, so parts aren't tiny.
			if i := strings.LastIndex(rest[:cut], sep); i > 0 && i >= cut/2 {
				end, next = i, i+len(sep)
				break
			}
		}
		if strings.TrimSpace(rest[:end]) != "" {
			parts = append(parts, [2]int{base, base + end})
		}
		base += next
	}
}

// tail returns s, or if it's over limit UTF-16 code units, "…" and as much
// of its end as fits.
func tail(s string, limit int) string {
	if start := tailStart(s, limit); start > 0 {
		return "…" + s[start:]
	}
	return s
}

// tailStart returns 0 if s fits in limit UTF-16 code units, else where
// the end that fits behind a one-unit "…" starts, in bytes.
func tailStart(s string, limit int) int {
	n := utf16Len(s)
	if n <= limit {
		return 0
	}
	// Drop the first n-(limit-1) units' worth of characters.
	drop := prefix(s, n-limit+1)
	if utf16Len(s[drop:]) > limit-1 { // a surrogate pair straddled the cut
		_, w := utf8.DecodeRuneInString(s[drop:])
		drop += w
	}
	return drop
}

// firstLine returns s's first line, cut to at most n runes.
func firstLine(s string, n int) string {
	s, _, cut := strings.Cut(strings.TrimSpace(s), "\n")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	} else if cut {
		return s + " …"
	}
	return s
}
