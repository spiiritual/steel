// Package anthropic implements llm.Provider for Anthropic's Messages API,
// including the request shaping Claude Pro/Max OAuth subscriptions need.
//
// Anthropic bills an OAuth request as Claude Code only when it carries the
// Claude Code billing header and identity block; without them a normal agent
// prompt is rejected with a misleading "You're out of extra usage" 400. This
// package applies the same recipe as pi-anthropic-auth
// (github.com/gotgenes/pi-anthropic-auth) and opencode-anthropic-auth.
package anthropic

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/spiiritual/steel/internal/llm"
)

// DefaultBaseURL is Anthropic's Messages API.
const DefaultBaseURL = "https://api.anthropic.com/v1"

// DefaultClaudeCodeVersion is reported in the OAuth billing header. Anthropic
// gates new models on this value; raise it when a model rejects it, without
// waiting for a steel release.
const DefaultClaudeCodeVersion = "2.1.323"

// VersionEnv overrides DefaultClaudeCodeVersion. It must be a bare X.Y.Z
// version; a malformed value fails at startup rather than silently reporting
// a client Anthropic has never seen.
const VersionEnv = "STEEL_CLAUDE_CODE_VERSION"

// Identity is the system block Anthropic requires on OAuth requests.
const Identity = "You are Claude Code, Anthropic's official CLI for Claude."

const (
	// fallbackMaxTokens applies when a request leaves the limit to the
	// provider and the Models API couldn't say the model's own: the
	// largest every Claude 4 and later model accepts.
	fallbackMaxTokens = 32000
	// infoRetry is how long a failed model lookup is remembered before
	// it's tried again.
	infoRetry = 10 * time.Minute

	// bindingBeta lets a request say what happens to a thinking block whose
	// conversation changed since it was written.
	bindingBeta   = "thinking-binding-controls-2026-08-01"
	entrypoint    = "sdk-cli"
	salt          = "59cf53e54c78"
	billingMarker = "x-anthropic-billing-header:"

	// usefulPhrase is a known Anthropic classifier trigger when it reaches
	// the API in an agent prompt; pi-anthropic-auth and
	// opencode-anthropic-auth both reword it. steel doesn't emit it, but a
	// user's own system prompt might.
	usefulPhrase = "Here is some useful information about the environment you are running in:"
)

var (
	versionRE    = regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	requiredRE   = regexp.MustCompile(`version (\d+\.\d+\.\d+) or newer is required`)
	positionList = []int{4, 7, 20}

	errVersionOverride = fmt.Errorf("%s must be a bare X.Y.Z version", VersionEnv)
)

// Options configure a Client. Set exactly one of APIKey and AccessToken.
type Options struct {
	BaseURL string
	// APIKey authenticates with an Anthropic API key (x-api-key).
	APIKey string
	// AccessToken supplies an OAuth access token, refreshing it as needed.
	// When set, requests carry Claude Code identity headers and shaping.
	AccessToken func(context.Context) (string, error)
	// Version pins the Claude Code version. Empty reads VersionEnv, then
	// DefaultClaudeCodeVersion.
	Version string
	// CacheTTL is how long Anthropic keeps the prompt cache between
	// requests: "5m" or "1h". Empty is "1h": replies in a chat often come
	// more than five minutes apart.
	CacheTTL string
	HTTP     *http.Client
}

// Client talks to the Messages API.
type Client struct {
	baseURL     string
	apiKey      string
	accessToken func(context.Context) (string, error)
	http        *http.Client
	override    string // explicit version: absolute, so recovery never raises it
	cacheTTL    string // "" for Anthropic's default five minutes

	mu         sync.Mutex
	version    string                   // current pin, raised by a learned floor
	models     map[string]modelInfo     // by model ID
	threads    map[[32]byte]threadEntry // by threadKeys of what they hold
	unthreaded bool                     // the API refused threads: stop asking
}

// modelInfo is what the Models API says about a model, or the fallback
// when it couldn't be asked.
type modelInfo struct {
	maxTokens int
	adaptive  bool      // takes thinking {type: "adaptive"}
	efforts   []string  // effort levels it takes, lowest first; nil: unknown
	retry     time.Time // zero: known; else ask again after this
}

// effortLevels are Anthropic's effort levels, lowest first.
var effortLevels = []string{"low", "medium", "high", "xhigh", "max"}

// effort returns the level to send for want: want itself if the model takes
// it, else the highest level below it that it does take. "" sends none,
// leaving the model's default.
func (m modelInfo) effort(want string) string {
	if want == "" || !m.adaptive || m.efforts == nil {
		if m.adaptive {
			return want // unknown levels: send it and let the API judge
		}
		return ""
	}
	best := ""
	for _, l := range effortLevels {
		if slices.Contains(m.efforts, l) {
			best = l
		}
		if l == want {
			break
		}
	}
	return best
}

// New returns a Messages API client.
func New(o Options) (*Client, error) {
	version := o.Version
	if version == "" {
		version = strings.TrimSpace(os.Getenv(VersionEnv))
	}
	override := version
	if version == "" {
		version = DefaultClaudeCodeVersion
	} else if !versionRE.MatchString(version) {
		return nil, fmt.Errorf("%w, got %q", errVersionOverride, version)
	}
	base := strings.TrimRight(o.BaseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}
	httpClient := o.HTTP
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	ttl := o.CacheTTL
	switch ttl {
	case "", "1h":
		ttl = "1h"
	case "5m":
		ttl = "" // the default; saying so changes nothing
	default:
		return nil, fmt.Errorf("cache TTL %q: want 5m or 1h", o.CacheTTL)
	}
	return &Client{
		baseURL:     base,
		apiKey:      o.APIKey,
		accessToken: o.AccessToken,
		http:        httpClient,
		override:    override,
		version:     version,
		cacheTTL:    ttl,
		models:      map[string]modelInfo{},
		threads:     map[[32]byte]threadEntry{},
	}, nil
}

func (c *Client) currentVersion() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.version
}

// learn remembers a floor Anthropic named, reporting whether it raised the
// version in use.
func (c *Client) learn(required string) bool {
	if c.override != "" || !versionRE.MatchString(required) {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	next := higherVersion(c.version, required)
	if next == c.version {
		return false
	}
	c.version = next
	return true
}

// Stream implements llm.Provider.
func (c *Client) Stream(ctx context.Context, req llm.Request, onDelta func(string) error) (llm.Message, error) {
	oauth := c.accessToken != nil
	token := ""
	if oauth {
		var err error
		if token, err = c.accessToken(ctx); err != nil {
			return llm.Message{}, err
		}
	}
	version := c.currentVersion()
	info := c.model(ctx, req.Model, version, token, oauth)
	keys, resp, err := c.open(ctx, req, info, version, token, oauth)
	if err != nil {
		if oauth && c.override == "" {
			if re, ok := err.(*responseError); ok {
				if required := requiredVersion(re.body); required != "" && c.learn(required) {
					version = c.currentVersion()
					keys, resp, err = c.open(ctx, req, info, version, token, oauth)
				}
			}
		}
		if err != nil {
			return llm.Message{}, c.explain(err)
		}
	}
	defer resp.Body.Close()
	msg, id, stored, err := readStream(resp.Body, onDelta)
	if err == nil && stored && keys != nil {
		if reply, ok := assistantMessage(msg); ok {
			c.remember(keys, reply, id)
		}
	}
	return msg, err
}

// open sends req, continuing the thread that holds its conversation if
// there is one, and otherwise starting a new thread with all of it. It
// returns the full request and its thread keys, so the reply's thread can
// be remembered; keys is nil when the request went without a thread.
func (c *Client) open(ctx context.Context, req llm.Request, info modelInfo, version, token string, oauth bool) ([][32]byte, *http.Response, error) {
	w := c.params(req, info, version, oauth)
	c.mu.Lock()
	threaded := !c.unthreaded
	c.mu.Unlock()
	if !threaded {
		resp, err := c.send(ctx, w, version, token, oauth)
		return nil, resp, err
	}
	keys, err := threadKeys(w)
	if err != nil {
		return nil, nil, err
	}
	if id, have := c.continuation(w, keys); id != "" {
		resp, err := c.send(ctx, continueRequest(w, id, have, oauth), version, token, oauth)
		switch code := threadError(err); {
		case err == nil:
			return keys, resp, nil
		case code == "thread_unsupported_request":
			return c.unthreadedSend(ctx, w, version, token, oauth)
		case code == "":
			return nil, nil, err
		}
		// Expired, already continued, or changed: replay it all below.
	}
	create := w
	create.Thread = &threadParam{Type: "create"}
	resp, err := c.send(ctx, create, version, token, oauth)
	if code := threadError(err); code == "thread_unsupported_request" {
		return c.unthreadedSend(ctx, w, version, token, oauth)
	}
	return keys, resp, err
}

// unthreadedSend sends w whole, and every later request too: the API said
// threads aren't available here.
func (c *Client) unthreadedSend(ctx context.Context, w wireRequest, version, token string, oauth bool) ([][32]byte, *http.Response, error) {
	c.mu.Lock()
	c.unthreaded = true
	c.mu.Unlock()
	resp, err := c.send(ctx, w, version, token, oauth)
	return nil, resp, err
}

// explain adds a hint to a version rejection that recovery could not fix.
func (c *Client) explain(err error) error {
	var re *responseError
	if !errors.As(err, &re) {
		return err
	}
	required := requiredVersion(re.body)
	if required == "" {
		return err
	}
	if c.override != "" {
		return fmt.Errorf("%w [steel] %s=%s is sent verbatim; set it to %s or newer, or unset it to let steel recover automatically", err, VersionEnv, c.override, required)
	}
	return fmt.Errorf("%w [steel] Anthropic requires Claude Code %s or newer; set %s=%s or upgrade steel", err, required, VersionEnv, required)
}

func (c *Client) send(ctx context.Context, w wireRequest, version, token string, oauth bool) (*http.Response, error) {
	payload, err := json.Marshal(w)
	if err != nil {
		return nil, err
	}
	u := c.baseURL + "/messages"
	if oauth {
		u += "?beta=true"
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	// The set of betas is fixed for a thread, so every request sends both.
	c.authorize(httpReq, version, token, oauth, bindingBeta, threadsBeta)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusOK {
		return resp, nil
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	return nil, &responseError{status: resp.Status, code: resp.StatusCode, body: raw}
}

// authorize sets the headers every request carries: auth, and for OAuth the
// Claude Code identity Anthropic bills it under.
func (c *Client) authorize(r *http.Request, version, token string, oauth bool, betas ...string) {
	r.Header.Set("Accept", "application/json")
	r.Header.Set("anthropic-version", "2023-06-01")
	if oauth {
		betas = append([]string{"claude-code-20250219", "oauth-2025-04-20"}, betas...)
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("user-agent", "claude-cli/"+version)
		r.Header.Set("x-app", "cli")
	} else if c.apiKey != "" {
		r.Header.Set("x-api-key", c.apiKey)
	}
	if len(betas) > 0 {
		r.Header.Set("anthropic-beta", strings.Join(betas, ","))
	}
}

// model returns what the Models API says about id: its real output limit,
// so a reply is never cut short by a guess, and whether it takes adaptive
// thinking. A failed lookup falls back to values every current model
// accepts and is retried later.
func (c *Client) model(ctx context.Context, id, version, token string, oauth bool) modelInfo {
	c.mu.Lock()
	info, ok := c.models[id]
	c.mu.Unlock()
	if ok && (info.retry.IsZero() || time.Now().Before(info.retry)) {
		return info
	}
	info, err := c.fetchModel(ctx, id, version, token, oauth)
	if err != nil {
		info = modelInfo{maxTokens: fallbackMaxTokens, adaptive: true, retry: time.Now().Add(infoRetry)}
	}
	c.mu.Lock()
	c.models[id] = info
	c.mu.Unlock()
	return info
}

func (c *Client) fetchModel(ctx context.Context, id, version, token string, oauth bool) (modelInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	u := c.baseURL + "/models/" + url.PathEscape(id)
	if oauth {
		u += "?beta=true"
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return modelInfo{}, err
	}
	c.authorize(r, version, token, oauth)
	resp, err := c.http.Do(r)
	if err != nil {
		return modelInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return modelInfo{}, fmt.Errorf("models API: %s", resp.Status)
	}
	var m struct {
		MaxTokens    int `json:"max_tokens"`
		Capabilities struct {
			Effort   map[string]json.RawMessage `json:"effort"`
			Thinking struct {
				Types struct {
					Adaptive struct {
						Supported bool `json:"supported"`
					} `json:"adaptive"`
				} `json:"types"`
			} `json:"thinking"`
		} `json:"capabilities"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&m); err != nil {
		return modelInfo{}, err
	}
	if m.MaxTokens <= 0 {
		return modelInfo{}, errors.New("models API: no max_tokens")
	}
	info := modelInfo{maxTokens: m.MaxTokens, adaptive: m.Capabilities.Thinking.Types.Adaptive.Supported}
	if m.Capabilities.Effort != nil {
		info.efforts = []string{}
		for _, l := range effortLevels {
			var leaf struct {
				Supported bool `json:"supported"`
			}
			if json.Unmarshal(m.Capabilities.Effort[l], &leaf) == nil && leaf.Supported {
				info.efforts = append(info.efforts, l)
			}
		}
	}
	return info, nil
}

// responseError is a non-200 answer, kept raw so a version rejection can be
// recognized and retried.
type responseError struct {
	status string
	code   int
	body   []byte
}

func (e *responseError) Error() string {
	var v struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(e.body, &v) == nil && v.Error != nil && v.Error.Message != "" {
		return e.status + ": " + v.Error.Message
	}
	if msg := strings.TrimSpace(string(e.body)); msg != "" {
		return e.status + ": " + msg
	}
	return e.status
}

func requiredVersion(body []byte) string {
	var v struct {
		Error *struct {
			Message string `json:"message"`
			Details *struct {
				ErrorCode string `json:"error_code"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &v) != nil || v.Error == nil {
		return ""
	}
	if v.Error.Details == nil || v.Error.Details.ErrorCode != "claude_code_version_too_old" {
		return ""
	}
	if m := requiredRE.FindStringSubmatch(v.Error.Message); m != nil {
		return m[1]
	}
	return ""
}

type wireRequest struct {
	Model        string        `json:"model"`
	MaxTokens    int           `json:"max_tokens"`
	System       []wireBlock   `json:"system,omitempty"`
	Messages     []wireMessage `json:"messages"`
	Tools        []wireTool    `json:"tools,omitempty"`
	ToolChoice   *toolChoice   `json:"tool_choice,omitempty"`
	Thinking     *thinking     `json:"thinking,omitempty"`
	OutputConfig *outputConfig `json:"output_config,omitempty"`
	// CacheControl caches the conversation up to its newest message,
	// wherever that is, without marking (and so changing) message blocks:
	// a thread can't move a mark once sent.
	CacheControl *cacheControl `json:"cache_control,omitempty"`
	Thread       *threadParam  `json:"thread,omitempty"`
	Stream       bool          `json:"stream"`
}

type wireMessage struct {
	Role    string      `json:"role"`
	Content []wireBlock `json:"content"`
	// raw, if set, is sent as content instead: an assistant reply with
	// thinking, which Anthropic needs back byte for byte.
	raw json.RawMessage
}

func (m wireMessage) MarshalJSON() ([]byte, error) {
	if m.raw != nil {
		return json.Marshal(struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}{m.Role, m.raw})
	}
	type plain wireMessage
	return json.Marshal(plain(m))
}

type wireBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	// Thinking is a pointer because a thinking block's text may be empty
	// (display "omitted") but must still be sent.
	Thinking  *string `json:"thinking,omitempty"`
	Signature string  `json:"signature,omitempty"`
	Data      string  `json:"data,omitempty"`

	CacheControl *cacheControl `json:"cache_control,omitempty"`
}

// cacheControl marks the end of a prompt prefix Anthropic should cache.
type cacheControl struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitempty"`
}

type wireTool struct {
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema"`
	CacheControl *cacheControl   `json:"cache_control,omitempty"`
}

type toolChoice struct {
	Type string `json:"type"`
}

// thinking is adaptive: the model decides how much to think. Its signed
// blocks come back in llm.Message.Blocks and are replayed unchanged.
type thinking struct {
	Type         string        `json:"type"`
	BlockBinding *blockBinding `json:"block_binding,omitempty"`
}

// blockBinding says what to do with a thinking block whose conversation
// changed since it was written (a /model switch, an edited history): drop
// it and carry on, rather than fail the turn.
type blockBinding struct {
	PrefixMismatchBehavior string `json:"prefix_mismatch_behavior"`
}

type outputConfig struct {
	Effort string `json:"effort,omitempty"`
}

func (c *Client) params(req llm.Request, info modelInfo, version string, oauth bool) wireRequest {
	var system []wireBlock
	if oauth {
		if h := billingHeader(req.Messages, version); h != "" {
			system = append(system, wireBlock{Type: "text", Text: h})
		}
		system = append(system, wireBlock{Type: "text", Text: Identity})
	}
	for _, m := range req.Messages {
		if m.Role == llm.RoleSystem && m.Content != "" {
			text := m.Content
			if oauth {
				text = sanitizeSystem(text)
			}
			system = append(system, wireBlock{Type: "text", Text: text})
		}
	}

	var messages []wireMessage
	addUser := func(blocks ...wireBlock) {
		if n := len(messages); n > 0 && messages[n-1].Role == "user" {
			messages[n-1].Content = append(messages[n-1].Content, blocks...)
			return
		}
		messages = append(messages, wireMessage{Role: "user", Content: blocks})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case llm.RoleSystem:
			continue
		case llm.RoleUser:
			text := m.Content
			if text == "" {
				text = "(empty message)" // Anthropic rejects a text block without text
			}
			addUser(wireBlock{Type: "text", Text: text})
		case llm.RoleAssistant:
			if wm, ok := assistantMessage(m); ok {
				messages = append(messages, wm)
			}
		case llm.RoleTool:
			content := m.Content
			if content == "" {
				content = "(no output)"
			}
			addUser(wireBlock{Type: "tool_result", ToolUseID: m.ToolCallID, Content: content})
		}
	}

	var tools []wireTool
	for _, t := range req.Tools {
		schema := t.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		tools = append(tools, wireTool{Name: t.Name, Description: t.Description, InputSchema: schema})
	}
	// Three cache marks, in Anthropic's prefix order. The last tool: the
	// tools are the same in every conversation, while the system prompt
	// opens with a billing block that differs per conversation, so this is
	// the only mark a new conversation can read. The system prompt. And
	// the top-level mark, everything up to the newest message, so each
	// turn pays only for what's new. Longer TTLs must come first; all are
	// the same.
	cache := &cacheControl{Type: "ephemeral", TTL: c.cacheTTL}
	if n := len(tools); n > 0 {
		tools[n-1].CacheControl = cache
	}
	if n := len(system); n > 0 {
		system[n-1].CacheControl = cache
	}
	w := wireRequest{
		Model:        req.Model,
		MaxTokens:    req.MaxTokens,
		System:       system,
		Messages:     messages,
		Tools:        tools,
		CacheControl: cache,
		Stream:       true,
	}
	// Models without adaptive thinking (Haiku 4.5 and older) run without
	// any: their budget_tokens mode needs a budget steel has no basis for.
	if info.adaptive {
		w.Thinking = &thinking{Type: "adaptive", BlockBinding: &blockBinding{PrefixMismatchBehavior: "drop_block"}}
	}
	if w.MaxTokens == 0 {
		w.MaxTokens = info.maxTokens
	}
	if e := info.effort(req.Effort); e != "" {
		w.OutputConfig = &outputConfig{Effort: e}
	}
	if req.NoToolCalls && len(tools) > 0 {
		w.ToolChoice = &toolChoice{Type: "none"}
	}
	return w
}

// assistantMessage is m as Anthropic wants it back: its blocks exactly as
// they came when it has thinking, else rebuilt from its text and tool
// calls. ok is false for an empty reply, which can't be sent.
func assistantMessage(m llm.Message) (wireMessage, bool) {
	if len(m.Blocks) > 0 && json.Valid(m.Blocks) {
		return wireMessage{Role: "assistant", raw: m.Blocks}, true
	}
	blocks := make([]wireBlock, 0, len(m.ToolCalls)+1)
	if m.Content != "" {
		blocks = append(blocks, wireBlock{Type: "text", Text: m.Content})
	}
	for _, tc := range m.ToolCalls {
		blocks = append(blocks, wireBlock{Type: "tool_use", ID: tc.ID, Name: tc.Name, Input: toolInput(tc.Arguments)})
	}
	return wireMessage{Role: "assistant", Content: blocks}, len(blocks) > 0
}

func toolInput(args string) json.RawMessage {
	raw := json.RawMessage(args)
	if len(raw) == 0 || !json.Valid(raw) || !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
		return json.RawMessage(`{}`)
	}
	return raw
}

// billingHeader builds the x-anthropic-billing-header value from the first
// user message, exactly as Claude Code does.
func billingHeader(messages []llm.Message, version string) string {
	text := firstUserText(messages)
	if text == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(text))
	cch := hex.EncodeToString(sum[:])[:5]
	suffixSum := sha256.Sum256([]byte(salt + sampleUnits(text, positionList) + version))
	suffix := hex.EncodeToString(suffixSum[:])[:3]
	return fmt.Sprintf("%s cc_version=%s.%s; cc_entrypoint=%s; cch=%s;", billingMarker, version, suffix, entrypoint, cch)
}

func firstUserText(messages []llm.Message) string {
	for _, m := range messages {
		if m.Role == llm.RoleUser {
			return m.Content
		}
	}
	return ""
}

func sanitizeSystem(s string) string {
	return strings.ReplaceAll(s, usefulPhrase, "Environment context you are running in:")
}

// sampleUnits joins the characters at UTF-16 indexes at (ascending), as
// JavaScript indexes a string; "0" past the end. A lone surrogate (half of
// an astral character) hashes as U+FFFD, which is what Node's UTF-8
// encoding of it produces. It reads only as far as the last index.
func sampleUnits(s string, at []int) string {
	var b strings.Builder
	i, next := 0, 0 // i: UTF-16 index of r
	for _, r := range s {
		if next == len(at) {
			break
		}
		width := utf16.RuneLen(r)
		for next < len(at) && at[next] < i+width {
			if width == 1 {
				b.WriteRune(r)
			} else {
				b.WriteRune('\uFFFD')
			}
			next++
		}
		i += width
	}
	for ; next < len(at); next++ {
		b.WriteString("0")
	}
	return b.String()
}

type streamEvent struct {
	Type         string `json:"type"`
	Index        int    `json:"index"`
	ContentBlock *struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
		Data string `json:"data"` // redacted_thinking
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		Thinking    string `json:"thinking"`
		Signature   string `json:"signature"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Message *struct {
		ID    string     `json:"id"`
		Usage *wireUsage `json:"usage"`
	} `json:"message"`
	Usage *wireUsage `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type wireUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

func (u *wireUsage) usage() *llm.Usage {
	if u == nil {
		return nil
	}
	return &llm.Usage{
		Input:  u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens,
		Cached: u.CacheReadInputTokens,
		Output: u.OutputTokens,
	}
}

// streamBlock is one content block as it streams in.
type streamBlock struct {
	typ, id, name, data  string
	text, args, thinking strings.Builder
	signature            string
}

// readStream reads a reply. id is the response's, and stored reports
// whether a thread holds it: it ended normally, not refused or cut off.
func readStream(r io.Reader, onDelta func(string) error) (msg llm.Message, id string, stored bool, err error) {
	var (
		blocks   []*streamBlock
		byIdx    = map[int]*streamBlock{}
		usage    *wireUsage
		stop     string
		done     bool
		thinking bool // the reply has thinking blocks to keep
	)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data:")
		if !ok {
			continue // event:, id:, comments and blank separators
		}
		data = strings.TrimSpace(data)
		if data == "" {
			continue
		}
		var e streamEvent
		if err := json.Unmarshal([]byte(data), &e); err != nil {
			return llm.Message{}, "", false, fmt.Errorf("decode stream event: %w", err)
		}
		switch e.Type {
		case "message_start":
			if e.Message != nil {
				id = e.Message.ID
				if e.Message.Usage != nil {
					usage = e.Message.Usage
				}
			}
		case "content_block_start":
			if e.ContentBlock == nil {
				continue
			}
			b := &streamBlock{typ: e.ContentBlock.Type, id: e.ContentBlock.ID, name: e.ContentBlock.Name, data: e.ContentBlock.Data}
			if b.typ == "thinking" || b.typ == "redacted_thinking" {
				thinking = true
			}
			byIdx[e.Index] = b
			blocks = append(blocks, b)
		case "content_block_delta":
			if e.Delta == nil {
				continue
			}
			b := byIdx[e.Index]
			if b == nil { // a delta without its start: keep its text
				b = &streamBlock{typ: "text"}
				byIdx[e.Index] = b
				blocks = append(blocks, b)
			}
			switch e.Delta.Type {
			case "text_delta":
				if e.Delta.Text != "" {
					b.text.WriteString(e.Delta.Text)
					if err := onDelta(e.Delta.Text); err != nil {
						return llm.Message{}, "", false, err
					}
				}
			case "input_json_delta":
				b.args.WriteString(e.Delta.PartialJSON)
			case "thinking_delta":
				b.thinking.WriteString(e.Delta.Thinking)
			case "signature_delta":
				b.signature += e.Delta.Signature
			}
		case "message_delta":
			if e.Delta != nil && e.Delta.StopReason != "" {
				stop = e.Delta.StopReason
			}
			if e.Usage != nil && e.Usage.OutputTokens != 0 {
				if usage == nil {
					usage = &wireUsage{}
				}
				usage.OutputTokens = e.Usage.OutputTokens
			}
		case "message_stop":
			done = true
		case "error":
			if e.Error != nil {
				return llm.Message{}, "", false, fmt.Errorf("api error: %s", e.Error.Message)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return llm.Message{}, "", false, err
	}
	if !done && stop == "" {
		return llm.Message{}, "", false, fmt.Errorf("stream ended before completion: %w", io.ErrUnexpectedEOF)
	}

	msg = llm.Message{Role: llm.RoleAssistant, Usage: usage.usage()}
	var text strings.Builder
	var raw []wireBlock
	for i, b := range blocks {
		switch b.typ {
		case "text":
			if b.text.Len() == 0 {
				continue // Anthropic rejects an empty text block
			}
			text.WriteString(b.text.String())
			raw = append(raw, wireBlock{Type: "text", Text: b.text.String()})
		case "tool_use":
			if b.id == "" {
				b.id = fmt.Sprintf("call_%d", i)
			}
			args := b.args.String()
			if args == "" {
				args = "{}"
			}
			msg.ToolCalls = append(msg.ToolCalls, llm.ToolCall{ID: b.id, Name: b.name, Arguments: args})
			raw = append(raw, wireBlock{Type: "tool_use", ID: b.id, Name: b.name, Input: toolInput(args)})
		case "thinking":
			raw = append(raw, wireBlock{Type: "thinking", Thinking: ptr(b.thinking.String()), Signature: b.signature})
		case "redacted_thinking":
			raw = append(raw, wireBlock{Type: "redacted_thinking", Data: b.data})
		}
	}
	msg.Content = text.String()
	// Only a reply with thinking needs its blocks kept: the rest rebuilds
	// exactly from Content and ToolCalls.
	if thinking {
		b, err := json.Marshal(raw)
		if err != nil {
			return llm.Message{}, "", false, err
		}
		msg.Blocks = b
	}
	switch stop {
	case "max_tokens":
		return msg, id, false, llm.ErrTruncated
	case "refusal":
		return llm.Message{}, "", false, errors.New("Anthropic's safety classifier declined this request")
	}
	return msg, id, done, nil
}

func ptr[T any](v T) *T { return &v }

// Both must match versionRE.
func higherVersion(a, b string) string {
	ap, bp := strings.Split(a, "."), strings.Split(b, ".")
	for i := range ap {
		x, _ := strconv.Atoi(ap[i])
		y, _ := strconv.Atoi(bp[i])
		if x != y {
			if y > x {
				return b
			}
			return a
		}
	}
	return a
}
