package anthropic

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"hash"
	"net/http"
	"strings"
	"time"
)

// Message threads let Anthropic keep a conversation for a few minutes, so
// each later request sends only the new messages: smaller requests and a
// faster first token in long tool loops, at the same price.
//
// steel's history stays the source of truth. A thread is only an
// optimization: the client remembers, by a hash of everything a thread
// holds, which response ID continues which conversation, and any thread
// error falls back to sending the whole conversation in a new thread.

const (
	threadsBeta = "message-threads-2026-08-12"
	// threadLife is how long a thread lasts after its last request. Past
	// it, continuing would only earn a 404, so a new thread starts at once.
	threadLife = 5 * time.Minute
)

type threadParam struct {
	Type              string `json:"type"`
	PreviousMessageID string `json:"previous_message_id,omitempty"`
}

type threadEntry struct {
	id   string
	used time.Time
}

// threadKeys returns, for each message in w, a key naming the conversation
// up to and including it, together with the fields a thread fixes. Equal
// keys mean a thread holding that conversation can be continued.
func threadKeys(w wireRequest) ([][32]byte, error) {
	fixed, err := json.Marshal(struct {
		Model        string        `json:"model"`
		System       []wireBlock   `json:"system"`
		Tools        []wireTool    `json:"tools"`
		ToolChoice   *toolChoice   `json:"tool_choice"`
		Thinking     *thinking     `json:"thinking"`
		OutputConfig *outputConfig `json:"output_config"`
		CacheControl *cacheControl `json:"cache_control"`
	}{w.Model, w.System, w.Tools, w.ToolChoice, w.Thinking, w.OutputConfig, w.CacheControl})
	if err != nil {
		return nil, err
	}
	keys := make([][32]byte, len(w.Messages))
	h, hash := sha256.Sum256(fixed), sha256.New()
	for i, m := range w.Messages {
		if h, err = chain(hash, h, m); err != nil {
			return nil, err
		}
		keys[i] = h
	}
	return keys, nil
}

// chain returns the key of a conversation keyed prev, plus m. hash is
// reset and reused.
func chain(hash hash.Hash, prev [32]byte, m wireMessage) (key [32]byte, err error) {
	b, err := json.Marshal(m)
	if err != nil {
		return prev, err
	}
	hash.Reset()
	hash.Write(prev[:])
	hash.Write(b)
	hash.Sum(key[:0])
	return key, nil
}

// continuation finds a thread holding w's conversation up to its last
// assistant message, returning the response ID to continue from and how
// many of w's messages the thread already has. The entry is taken: a
// response can be continued once.
func (c *Client) continuation(w wireRequest, keys [][32]byte) (id string, have int) {
	last := -1
	for i, m := range w.Messages {
		if m.Role == "assistant" {
			last = i
		}
	}
	if last < 0 || last == len(w.Messages)-1 {
		return "", 0 // nothing stored yet, or nothing new to send
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.threads[keys[last]]
	if !ok || time.Since(e.used) > threadLife {
		return "", 0
	}
	delete(c.threads, keys[last])
	return e.id, last + 1
}

// remember records that response id holds the conversation keyed by keys
// plus reply.
func (c *Client) remember(keys [][32]byte, reply wireMessage, id string) {
	if id == "" || len(keys) == 0 {
		return
	}
	key, err := chain(sha256.New(), keys[len(keys)-1], reply)
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for k, e := range c.threads {
		if now.Sub(e.used) > threadLife {
			delete(c.threads, k)
		}
	}
	c.threads[key] = threadEntry{id: id, used: now}
}

// continueRequest is w cut down to a continue request: the new messages,
// no tools, and for OAuth only the billing block of system. Claude Code's
// continuations keep that block, Anthropic leaves it out of the thread's
// fingerprint, and without it the request isn't billed as Claude Code.
func continueRequest(w wireRequest, id string, have int, oauth bool) wireRequest {
	var billing []wireBlock
	if oauth {
		for _, b := range w.System {
			if strings.HasPrefix(b.Text, billingMarker) {
				b.CacheControl = nil
				billing = append(billing, b)
			}
		}
	}
	w.Messages = w.Messages[have:]
	w.System = billing
	w.Tools = nil
	w.Thread = &threadParam{Type: "continue", PreviousMessageID: id}
	return w
}

// threadError returns the error code of a thread failure, or "" if err
// isn't one.
func threadError(err error) string {
	var re *responseError
	if !errors.As(err, &re) {
		return ""
	}
	var v struct {
		Error *struct {
			Details *struct {
				ErrorCode string `json:"error_code"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(re.body, &v) != nil || v.Error == nil || v.Error.Details == nil {
		return ""
	}
	code := v.Error.Details.ErrorCode
	if !strings.HasPrefix(code, "thread_") {
		return ""
	}
	if code == "thread_not_found" && re.code != http.StatusNotFound {
		return ""
	}
	return code
}
