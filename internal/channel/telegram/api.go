package telegram

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultAPIURL is the Bot API server.
const DefaultAPIURL = "https://api.telegram.org"

// Bot API types, limited to the fields steel uses.
// https://core.telegram.org/bots/api#available-types
type (
	update struct {
		UpdateID                 int64              `json:"update_id"`
		Message                  *message           `json:"message"`
		StoppedMessageGeneration *generationStopped `json:"stopped_message_generation"`
		CallbackQuery            *callbackQuery     `json:"callback_query"`
	}
	message struct {
		MessageID int64   `json:"message_id"`
		From      *user   `json:"from"`
		Chat      chatRef `json:"chat"`
		Text      string  `json:"text"`
	}
	user struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
	}
	chatRef struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	}
	// generationStopped is a MessageGenerationStopped: the user pressed the
	// stop button of a draft sent with can_stop.
	generationStopped struct {
		Chat    chatRef `json:"chat"`
		DraftID flexInt `json:"draft_id"`
	}
)

// callbackQuery is a press of an inline keyboard button.
type callbackQuery struct {
	ID      string   `json:"id"`
	From    *user    `json:"from"`
	Message *message `json:"message"` // absent if the message is too old
	Data    string   `json:"data"`
}

// flexInt is an integer Telegram may send as a JSON number or a string:
// draft_id is documented as Integer but arrives as "123".
type flexInt int64

func (n *flexInt) UnmarshalJSON(b []byte) error {
	var v int64
	if err := json.Unmarshal(bytes.Trim(b, `"`), &v); err != nil {
		return err
	}
	*n = flexInt(v)
	return nil
}

// Method parameters.
type (
	getUpdatesParams struct {
		Offset         int64    `json:"offset,omitempty"`
		Limit          int      `json:"limit,omitempty"`
		Timeout        int      `json:"timeout"`
		AllowedUpdates []string `json:"allowed_updates"`
	}
	sendMessageParams struct {
		ChatID      int64           `json:"chat_id"`
		Text        string          `json:"text"`
		ReplyMarkup *inlineKeyboard `json:"reply_markup,omitempty"`
	}
	// editMessageTextParams without ReplyMarkup removes the message's
	// buttons.
	editMessageTextParams struct {
		ChatID    int64  `json:"chat_id"`
		MessageID int64  `json:"message_id"`
		Text      string `json:"text"`
	}
	editMessageReplyMarkupParams struct {
		ChatID    int64 `json:"chat_id"`
		MessageID int64 `json:"message_id"`
	}
	answerCallbackQueryParams struct {
		CallbackQueryID string `json:"callback_query_id"`
		Text            string `json:"text,omitempty"`
	}
	inlineKeyboard struct {
		InlineKeyboard [][]inlineButton `json:"inline_keyboard"`
	}
	inlineButton struct {
		Text         string `json:"text"`
		CallbackData string `json:"callback_data"`
	}
	sendMessageDraftParams struct {
		ChatID  int64 `json:"chat_id"`
		DraftID int64 `json:"draft_id"`
		// Text may be empty: Telegram then shows a "Thinking…" placeholder.
		Text       string `json:"text"`
		CanStop    bool   `json:"can_stop,omitempty"`
		KeepOnStop bool   `json:"keep_on_stop,omitempty"`
	}
	sendChatActionParams struct {
		ChatID int64  `json:"chat_id"`
		Action string `json:"action"`
	}
)

// apiError is an unsuccessful Bot API response.
type apiError struct {
	Method      string
	Code        int
	Description string
	RetryAfter  time.Duration // flood control: wait this long before retrying
}

func (e *apiError) Error() string {
	return fmt.Sprintf("%s: %d %s", e.Method, e.Code, e.Description)
}

// transient reports whether retrying might succeed: flood control, server
// errors, and another getUpdates poller (which may be an old instance
// shutting down). The other 409, an active webhook, needs the owner to
// delete it, so it isn't retried.
func (e *apiError) transient() bool {
	if e.Code == http.StatusConflict {
		return strings.Contains(e.Description, "terminated by other getUpdates request")
	}
	return e.Code == http.StatusTooManyRequests || e.Code >= 500
}

// retryAfter returns how long flood control asks to wait, or 0.
func retryAfter(err error) time.Duration {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.RetryAfter
	}
	return 0
}

// permanent reports whether err is an API error that retrying won't fix.
func permanent(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && !ae.transient()
}

// client calls Bot API methods.
type client struct {
	base string // ends in "/bot<token>/"
	http *http.Client

	// Retry backoff for transient failures; tests shorten it.
	backoffMin, backoffMax time.Duration
	sleep                  func(ctx context.Context, d time.Duration) error
	logf                   func(format string, args ...any)
}

func newClient(apiURL, token string, hc *http.Client, logf func(string, ...any)) *client {
	return &client{
		base:       apiURL + "/bot" + token + "/",
		http:       cmp.Or(hc, http.DefaultClient),
		backoffMin: time.Second,
		backoffMax: 30 * time.Second,
		sleep:      sleepCtx,
		logf:       logf,
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// call makes one request to method with params as its JSON body and decodes
// the result into out (if non-nil). The request is abandoned after timeout.
// https://core.telegram.org/bots/api#making-requests
func (c *client) call(ctx context.Context, method string, params, out any, timeout time.Duration) error {
	body, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+method, bytes.NewReader(body))
	if err != nil {
		// Only the token can make the URL invalid; don't print it.
		return &apiError{Method: method, Code: http.StatusUnauthorized, Description: "invalid bot token"}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		// The request URL holds the bot token; keep it out of messages.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("%s: %w", method, err)
	}
	defer resp.Body.Close()
	var r struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		ErrorCode   int             `json:"error_code"`
		Description string          `json:"description"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&r); err != nil {
		if resp.StatusCode != http.StatusOK {
			return &apiError{Method: method, Code: resp.StatusCode, Description: http.StatusText(resp.StatusCode)}
		}
		return fmt.Errorf("%s: decoding response: %w", method, err)
	}
	if !r.OK {
		return &apiError{
			Method:      method,
			Code:        cmp.Or(r.ErrorCode, resp.StatusCode),
			Description: r.Description,
			RetryAfter:  time.Duration(r.Parameters.RetryAfter) * time.Second,
		}
	}
	if out != nil {
		if err := json.Unmarshal(r.Result, out); err != nil {
			return fmt.Errorf("%s: decoding result: %w", method, err)
		}
	}
	return nil
}

// callRetry is call, retried until it succeeds, fails permanently, or ctx
// ends. Flood control waits as long as Telegram asks; other transient
// failures (network errors, 5xx) back off exponentially, giving up after
// attempts of them (0 = never).
func (c *client) callRetry(ctx context.Context, method string, params, out any, timeout time.Duration, attempts int) error {
	delay := c.backoffMin
	for failures := 0; ; {
		err := c.call(ctx, method, params, out, timeout)
		if err == nil || ctx.Err() != nil || permanent(err) {
			return err
		}
		wait := retryAfter(err)
		if wait == 0 {
			if failures++; attempts > 0 && failures >= attempts {
				return err
			}
			wait = delay
			delay = min(delay*2, c.backoffMax)
		}
		c.logf("%v; retrying in %s", err, wait)
		if err := c.sleep(ctx, wait); err != nil {
			return err
		}
	}
}
