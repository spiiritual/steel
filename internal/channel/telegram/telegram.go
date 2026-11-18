// Package telegram is a channel that talks to steel through a Telegram bot.
// It long-polls the Bot API for messages from allowed users and streams
// session events back into their chats.
package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/spiiritual/steel/internal/agent"
)

// IDPrefix starts the session ID of every Telegram conversation:
// "telegram:<chat_id>" for a chat, "telegram:<chat_id>/<topic_id>" for a
// topic in it.
const IDPrefix = "telegram:"

// place is where a conversation happens: a chat, or a topic in it. Each
// place has its own session. Topic 0 is the chat outside any topic.
type place struct{ chat, topic int64 }

func (p place) String() string {
	if p.topic == 0 {
		return fmt.Sprintf("chat %d", p.chat)
	}
	return fmt.Sprintf("chat %d topic %d", p.chat, p.topic)
}

func (p place) sessionID() string {
	id := IDPrefix + strconv.FormatInt(p.chat, 10)
	if p.topic != 0 {
		id += "/" + strconv.FormatInt(p.topic, 10)
	}
	return id
}

// parsePlace is sessionID's inverse.
func parsePlace(id string) (place, bool) {
	rest, ok := strings.CutPrefix(id, IDPrefix)
	if !ok {
		return place{}, false
	}
	c, t, hasTopic := strings.Cut(rest, "/")
	var p place
	var err error
	if p.chat, err = strconv.ParseInt(c, 10, 64); err != nil {
		return place{}, false
	}
	if hasTopic {
		if p.topic, err = strconv.ParseInt(t, 10, 64); err != nil || p.topic == 0 {
			return place{}, false
		}
	}
	return p, true
}

// placeOf returns where m was sent.
func placeOf(m *message) place {
	p := place{chat: m.Chat.ID}
	if m.IsTopicMessage {
		p.topic = m.MessageThreadID
	}
	return p
}

const greeting = "Hi, I'm steel. Send me a message to start.\n\n" +
	"/stop stops what I'm doing\n" +
	"/reset clears my scratchpad\n" +
	"/new starts a new conversation"

// Options configure a Bot.
type Options struct {
	Token string
	// AllowedUsers are the Telegram user IDs that may talk to the bot.
	// Everyone else is ignored; an empty list allows nobody.
	AllowedUsers []int64
	APIURL       string       // default DefaultAPIURL
	HTTPClient   *http.Client // default http.DefaultClient
	Log          io.Writer    // default os.Stderr
}

// Bot is a Telegram channel.
type Bot struct {
	api     *client
	allowed map[int64]bool
	log     io.Writer
	logMu   sync.Mutex

	// Tunables; tests shorten them.
	pollTimeout    time.Duration // getUpdates long-poll timeout
	draftInterval  time.Duration // minimum gap between drafts in one chat
	draftRefresh   time.Duration // re-send an unchanged draft before it expires
	typingInterval time.Duration // re-send the typing indicator before it expires
	sendAttempts   int           // give up on a message after this many transient failures
	closeTimeout   time.Duration // how long Close waits for pending messages
	chatIdle       time.Duration // an idle renderer exits after this long

	// ctx bounds outgoing messages; Close cancels it.
	ctx    context.Context
	cancel context.CancelFunc
	// polling is set while Run receives updates, so drafts offer a stop
	// button only while its press can be received.
	polling atomic.Bool

	mu        sync.Mutex
	chats     map[place]*chat // renderers; each removes itself once idle
	unknown   map[int64]bool  // user IDs already logged as not allowed
	closing   bool
	closingCh chan struct{}
	wg        sync.WaitGroup
}

// New returns a Bot. Call Close when done with it, after the sessions that
// emit to it are closed.
func New(o Options) *Bot {
	b := &Bot{
		allowed:        map[int64]bool{},
		log:            o.Log,
		pollTimeout:    50 * time.Second,
		draftInterval:  time.Second,
		draftRefresh:   20 * time.Second, // drafts last 30 seconds
		typingInterval: 4 * time.Second,  // typing lasts 5 seconds
		sendAttempts:   5,
		closeTimeout:   5 * time.Second,
		chatIdle:       10 * time.Minute,
		chats:          map[place]*chat{},
		unknown:        map[int64]bool{},
		closingCh:      make(chan struct{}),
	}
	if b.log == nil {
		b.log = os.Stderr
	}
	apiURL := o.APIURL
	if apiURL == "" {
		apiURL = DefaultAPIURL
	}
	b.api = newClient(strings.TrimSuffix(apiURL, "/"), o.Token, o.HTTPClient, b.logf)
	for _, id := range o.AllowedUsers {
		b.allowed[id] = true
	}
	b.ctx, b.cancel = context.WithCancel(context.Background())
	return b
}

func (b *Bot) logf(format string, args ...any) {
	b.logMu.Lock()
	defer b.logMu.Unlock()
	fmt.Fprintf(b.log, "steel: telegram: "+format+"\n", args...)
}

// sender is the part of *agent.Sessions Run uses.
type sender interface {
	Send(id, text string)
	Answer(id, approvalID string, allow bool) bool
}

var _ sender = (*agent.Sessions)(nil)

// Run receives messages and hands them to their chats' sessions until ctx
// is cancelled. Updates not yet handed over when ctx ends are left
// unconfirmed, so Telegram delivers them again after a restart. Run returns
// an error if the API rejects the bot, e.g. for an invalid token.
func (b *Bot) Run(ctx context.Context, ss *agent.Sessions) error {
	return b.run(ctx, ss)
}

func (b *Bot) run(ctx context.Context, ss sender) error {
	if len(b.allowed) == 0 {
		b.logf("no allowed users; every message will be ignored. Add user IDs to \"telegram\": {\"allowedUsers\": [...]} in the config")
	}
	var me user
	if err := b.api.callRetry(ctx, "getMe", struct{}{}, &me, 30*time.Second, 0); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	b.logf("running as @%s", me.Username)

	b.polling.Store(true)
	defer b.polling.Store(false)
	// offset is the next update to receive; Telegram has confirmed every
	// update before acked.
	var offset, acked int64
	err := func() error {
		for ctx.Err() == nil {
			// Decoded one by one below, so an update steel can't read is
			// skipped instead of blocking every update behind it.
			var raw []json.RawMessage
			err := b.api.callRetry(ctx, "getUpdates", getUpdatesParams{
				Offset:         offset,
				Timeout:        int(b.pollTimeout / time.Second),
				AllowedUpdates: []string{"message", "stopped_message_generation", "callback_query"},
			}, &raw, b.pollTimeout+15*time.Second, 0)
			if ctx.Err() != nil {
				return nil
			}
			if err != nil {
				return err
			}
			acked = offset
			for _, r := range raw {
				if ctx.Err() != nil {
					return nil
				}
				var u update
				if err := json.Unmarshal(r, &u); err != nil {
					var id struct {
						UpdateID int64 `json:"update_id"`
					}
					if json.Unmarshal(r, &id) != nil || id.UpdateID == 0 {
						return fmt.Errorf("getUpdates: update without update_id: %.200s", r)
					}
					b.logf("skipping update %d: %v", id.UpdateID, err)
					offset = id.UpdateID + 1
					continue
				}
				b.handle(ss, me.Username, u)
				offset = u.UpdateID + 1
			}
		}
		return nil
	}()
	if offset > acked {
		// Confirm what was handed over; the update this returns, if any,
		// stays unconfirmed.
		ackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if aerr := b.api.call(ackCtx, "getUpdates", getUpdatesParams{Offset: offset, Limit: 1}, nil, 5*time.Second); aerr != nil {
			b.logf("confirming received messages: %v; they may be delivered again", aerr)
		}
		cancel()
	}
	return err
}

// handle routes one update.
func (b *Bot) handle(ss sender, botName string, u update) {
	if q := u.CallbackQuery; q != nil {
		b.answer(ss, q)
		return
	}
	if s := u.StoppedMessageGeneration; s != nil {
		// Drafts exist only in private chats, whose ID is the user's.
		if s.Chat.Type == "private" && b.allowed[s.Chat.ID] {
			b.stop(ss, place{chat: s.Chat.ID, topic: draftTopic(int64(s.DraftID))}, "/stop")
		}
		return
	}
	m := u.Message
	if m == nil || m.From == nil {
		return
	}
	if !b.allowed[m.From.ID] {
		b.mu.Lock()
		first := !b.unknown[m.From.ID]
		b.unknown[m.From.ID] = true
		b.mu.Unlock()
		if first {
			name := ""
			if m.From.Username != "" {
				name = " (@" + m.From.Username + ")"
			}
			b.logf("ignoring user %d%s; to allow them, add %d to \"telegram\": {\"allowedUsers\": [...]} in the config", m.From.ID, name, m.From.ID)
		}
		return
	}
	p := placeOf(m)
	if m.service() {
		return
	}
	if m.Text == "" {
		if m.Chat.Type == "private" {
			b.push(p, agent.Notice{Text: "I can only read text messages for now."})
		}
		return
	}
	cmd, rest, mine := command(m.Text, botName)
	if !mine {
		return // a command for another bot in the same group
	}
	sid := p.sessionID()
	switch cmd {
	case "/start":
		b.push(p, agent.Notice{Text: greeting})
		return
	case "/stop", "/new":
		b.stop(ss, p, cmd)
	case "/reset":
		ss.Send(sid, cmd)
	default:
		b.push(p, userSent{})
		ss.Send(sid, strings.TrimSpace(cmd+" "+rest))
		return
	}
	if rest != "" {
		b.push(p, userSent{})
		ss.Send(sid, rest)
	}
}

// Callback data of approval buttons: a prefix and the request's ID.
const (
	allowPrefix  = "allow:"
	refusePrefix = "refuse:"
)

// answer handles a press of an approval button. Only allowed users'
// presses count, so in a group nobody else can approve.
func (b *Bot) answer(ss sender, q *callbackQuery) {
	reply := func(text string) {
		ctx, cancel := context.WithTimeout(b.ctx, requestTimeout)
		defer cancel()
		if err := b.api.call(ctx, "answerCallbackQuery", answerCallbackQueryParams{CallbackQueryID: q.ID, Text: text}, nil, requestTimeout); err != nil && b.ctx.Err() == nil {
			b.logf("answering a button press: %v", err)
		}
	}
	if q.From == nil || !b.allowed[q.From.ID] || q.Message == nil {
		reply("")
		return
	}
	id, allow := strings.CutPrefix(q.Data, allowPrefix)
	if !allow {
		var ok bool
		if id, ok = strings.CutPrefix(q.Data, refusePrefix); !ok {
			reply("")
			return
		}
	}
	chat := q.Message.Chat.ID
	if ss.Answer(placeOf(q.Message).sessionID(), id, allow) {
		reply("") // the request's ApprovalDone updates the message
		return
	}
	// From before a restart, or already answered: drop the stale buttons.
	reply("This request isn't waiting anymore.")
	ctx, cancel := context.WithTimeout(b.ctx, requestTimeout)
	defer cancel()
	b.api.call(ctx, "editMessageReplyMarkup", editMessageReplyMarkupParams{ChatID: chat, MessageID: q.Message.MessageID}, nil, requestTimeout)
}

// stop sends a stopping command (/stop or /new) to p's session. p's
// renderer hears first, so the reply streamed so far is kept ahead of the
// session's acknowledgement.
func (b *Bot) stop(ss sender, p place, cmd string) {
	b.push(p, stopping{})
	ss.Send(p.sessionID(), cmd)
}

// A draft ID carries its topic, since a stop button press names only the
// chat and draft: the topic in the high 32 bits, a counter in the low.
// Telegram accepts draft IDs beyond 2^53. Topic IDs are message IDs, which
// fit in 31 bits.
func draftID(topic int64, seq uint32) int64 { return topic<<32 | int64(seq) }
func draftTopic(draft int64) int64          { return draft >> 32 }

// commandRE matches Telegram's bot command syntax: "/" and up to 32
// letters, digits, or underscores, optionally "@botname".
// https://core.telegram.org/bots/features#commands
var commandRE = regexp.MustCompile(`^/([A-Za-z0-9_]{1,32})(?:@([A-Za-z0-9_]+))?$`)

// command splits a leading bot command off text, lowercased and without its
// "@botname" suffix: "/Stop@steel_bot now" gives "/stop", "now". Text that
// isn't a command, including paths like "/Users/me/notes.txt", comes back
// whole as rest. mine is false for a command addressed to another bot.
func command(text, botName string) (cmd, rest string, mine bool) {
	text = strings.TrimSpace(text)
	i := strings.IndexFunc(text, unicode.IsSpace)
	if i < 0 {
		i = len(text)
	}
	m := commandRE.FindStringSubmatch(text[:i])
	if m == nil {
		return "", text, true
	}
	if m[2] != "" && !strings.EqualFold(m[2], botName) {
		return "", "", false
	}
	return "/" + strings.ToLower(m[1]), strings.TrimSpace(text[i:]), true
}

// Emit renders one session event in its chat. It never blocks on the
// network and is safe for concurrent use.
func (b *Bot) Emit(id string, e agent.Event) {
	if p, ok := parsePlace(id); ok {
		b.push(p, e)
	}
}

// push queues e for p's renderer, starting it if needed.
func (b *Bot) push(p place, e any) {
	b.mu.Lock()
	if b.closing {
		b.mu.Unlock()
		return
	}
	c := b.chats[p]
	if c == nil {
		c = newChat(b, p)
		b.chats[p] = c
		b.wg.Add(1)
		go c.run()
	}
	// Queued under b.mu, so a renderer removing itself sees it.
	c.push(e)
	b.mu.Unlock()
}

// Close delivers queued messages, waiting up to a few seconds, and stops
// rendering. Events emitted afterwards are dropped.
func (b *Bot) Close() {
	b.mu.Lock()
	if b.closing {
		b.mu.Unlock()
		return
	}
	b.closing = true
	close(b.closingCh)
	b.mu.Unlock()
	done := make(chan struct{})
	go func() {
		b.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(b.closeTimeout):
		b.logf("gave up delivering messages after %s", b.closeTimeout)
		b.cancel()
		<-done
	}
	b.cancel()
}
