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

// IDPrefix starts the session ID of every Telegram chat: "telegram:<chat_id>".
const IDPrefix = "telegram:"

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

	// ctx bounds outgoing messages; Close cancels it.
	ctx    context.Context
	cancel context.CancelFunc
	// polling is set while Run receives updates, so drafts offer a stop
	// button only while its press can be received.
	polling atomic.Bool

	mu        sync.Mutex
	chats     map[int64]*chat
	unknown   map[int64]bool // user IDs already logged as not allowed
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
		chats:          map[int64]*chat{},
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
				AllowedUpdates: []string{"message", "stopped_message_generation"},
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
	if s := u.StoppedMessageGeneration; s != nil {
		// Drafts exist only in private chats, whose ID is the user's.
		if s.Chat.Type == "private" && b.allowed[s.Chat.ID] {
			b.stop(ss, s.Chat.ID, "/stop")
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
	id := m.Chat.ID
	if m.Text == "" {
		if m.Chat.Type == "private" {
			b.push(id, agent.Notice{Text: "I can only read text messages for now."})
		}
		return
	}
	cmd, rest, mine := command(m.Text, botName)
	if !mine {
		return // a command for another bot in the same group
	}
	sid := IDPrefix + strconv.FormatInt(id, 10)
	switch cmd {
	case "/start":
		b.push(id, agent.Notice{Text: greeting})
		return
	case "/stop", "/new":
		b.stop(ss, id, cmd)
	case "/reset":
		ss.Send(sid, cmd)
	default:
		b.push(id, userSent{})
		ss.Send(sid, strings.TrimSpace(cmd+" "+rest))
		return
	}
	if rest != "" {
		b.push(id, userSent{})
		ss.Send(sid, rest)
	}
}

// stop sends a stopping command (/stop or /new) to chat id's session. The
// chat's renderer hears first, so the reply streamed so far is kept ahead
// of the session's acknowledgement.
func (b *Bot) stop(ss sender, id int64, cmd string) {
	b.push(id, stopping{})
	ss.Send(IDPrefix+strconv.FormatInt(id, 10), cmd)
}

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
	n, ok := strings.CutPrefix(id, IDPrefix)
	if !ok {
		return
	}
	cid, err := strconv.ParseInt(n, 10, 64)
	if err != nil {
		return
	}
	b.push(cid, e)
}

// push queues e for chat id's renderer, starting it if needed.
func (b *Bot) push(id int64, e any) {
	b.mu.Lock()
	if b.closing {
		b.mu.Unlock()
		return
	}
	c := b.chats[id]
	if c == nil {
		c = newChat(b, id)
		b.chats[id] = c
		b.wg.Add(1)
		go c.run()
	}
	b.mu.Unlock()
	c.push(e)
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
