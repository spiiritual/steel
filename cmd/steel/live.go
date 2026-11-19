package main

import (
	"cmp"
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/spiiritual/steel/internal/anthropicauth"
	"github.com/spiiritual/steel/internal/channel/telegram"
	"github.com/spiiritual/steel/internal/llm"
	"github.com/spiiritual/steel/internal/llm/anthropic"
	"github.com/spiiritual/steel/internal/llm/openai"
)

// Models a Claude login switches to when the models in use aren't Claude's.
const (
	claudeModel        = "claude-sonnet-5-5"
	claudeCompactModel = "claude-haiku-5-5"
)

// liveModel is the provider and models steel uses. /login, /logout and
// /model change them while steel runs, and save what they change in
// config.json so the next start agrees.
type liveModel struct {
	configPath string
	tokensPath string       // the .env holding the Claude login
	apiKey     llm.Provider // the OpenAI-compatible provider at base-url
	claude     llm.Provider // Anthropic, with the saved Claude login
	keySet     bool         // an API key is configured
	maxOutput  int          // caps turns' replies; 0 leaves it to the model
	// redact hides a value from tool results from now on.
	redact func(name, value string)

	mu                  sync.Mutex
	auth                string
	model, compactModel string // compactModel "" follows model
	effort              string // "" leaves it to the model
}

// newLiveModel builds both providers, so switching needs no restart.
func newLiveModel(o *options, apiKey string, redact func(name, value string)) (*liveModel, error) {
	l := &liveModel{
		configPath:   *o.configPath,
		tokensPath:   oauthPath(*o.configPath),
		apiKey:       openai.New(*o.baseURL, apiKey),
		keySet:       apiKey != "",
		maxOutput:    *o.maxOutputTokens,
		redact:       redact,
		auth:         o.auth.value,
		model:        *o.model,
		compactModel: *o.compactModel,
		effort:       effortValue(o.effort.value),
	}
	l.redactSaved() // before any script can read the file
	source := &anthropicauth.Source{Path: l.tokensPath, OAuth: anthropicauth.Default(), Seen: l.redactTokens}
	// A Claude login only ever goes to Anthropic: base-url is for the
	// OpenAI-compatible provider, and an old value pointing elsewhere would
	// hand the subscription token to that server.
	claude, err := anthropic.New(anthropic.Options{BaseURL: anthropic.DefaultBaseURL, AccessToken: source.AccessToken, CacheTTL: o.cacheTTL.value})
	if err != nil {
		return nil, err
	}
	l.claude = claude
	return l, nil
}

// effortValue is the effort setting as requests carry it.
func effortValue(setting string) string {
	if setting == "default" {
		return ""
	}
	return setting
}

func (l *liveModel) redactTokens(t anthropicauth.Tokens) {
	l.redact(anthropicauth.AccessVar, t.Access)
	l.redact(anthropicauth.RefreshVar, t.Refresh)
}

// redactSaved redacts the saved Claude login's tokens, if there is one.
// Whichever auth is in use, `steel login` on the host can save one, and
// scripts can read the file.
func (l *liveModel) redactSaved() {
	if t, err := anthropicauth.ReadTokens(l.tokensPath); err == nil {
		l.redactTokens(t)
	}
}

// provider routes each call to the provider auth selects at that moment.
func (l *liveModel) provider() llm.Provider { return routed{l} }

type routed struct{ l *liveModel }

func (r routed) Stream(ctx context.Context, req llm.Request, onDelta func(string) error) (llm.Message, error) {
	r.l.mu.Lock()
	claude := r.l.auth == "claude-oauth"
	r.l.mu.Unlock()
	if claude {
		return r.l.claude.Stream(ctx, req, onDelta) // its token source redacts
	}
	r.l.redactSaved()
	return r.l.apiKey.Stream(ctx, req, onDelta)
}

// turn is the model call each turn starts from.
func (l *liveModel) turn() llm.Request {
	l.mu.Lock()
	defer l.mu.Unlock()
	return llm.Request{Model: l.model, MaxTokens: l.maxOutput, Effort: l.effort}
}

// summary is the model call each memory summary starts from.
func (l *liveModel) summary() llm.Request {
	l.mu.Lock()
	defer l.mu.Unlock()
	req := llm.Request{Model: cmp.Or(l.compactModel, l.model)}
	if l.auth == "claude-oauth" {
		// A summary is short work: Claude's adaptive thinking at the
		// model's default effort would eat its cap. OpenAI-compatible
		// models may not take an effort at all.
		req.Effort = "low"
	}
	return req
}

// Describe implements telegram.Models.
func (l *liveModel) Describe() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	how := "your API key"
	if l.auth == "claude-oauth" {
		how = "your Claude subscription"
	}
	compact := cmp.Or(l.compactModel, l.model+" (same as the model)")
	effort := cmp.Or(l.effort, "the model's default")
	return fmt.Sprintf("Model: %s\nEffort: %s\nMemory summaries: %s\nThrough %s.\n\n/model NAME changes the model, /model effort LEVEL the effort (%s), /model compact NAME the summaries' model.", l.model, effort, compact, how, strings.Join(effortNames, ", "))
}

// SetModel implements telegram.Models.
func (l *liveModel) SetModel(name string) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.saveLocked(map[string]any{"model": name}); err != nil {
		return "", err
	}
	l.model = name
	return "Model set to " + name + ", from the next message on.", nil
}

// SetCompactModel implements telegram.Models.
func (l *liveModel) SetCompactModel(name string) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.saveLocked(map[string]any{"compact-model": name}); err != nil {
		return "", err
	}
	l.compactModel = name
	return "Memory summaries' model set to " + name + ".", nil
}

// SetEffort implements telegram.Models.
func (l *liveModel) SetEffort(level string) (string, error) {
	v, err := settingJSON("effort", level) // the same check as steel config set
	if err != nil {
		return "", err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.saveLocked(map[string]any{"effort": v}); err != nil {
		return "", err
	}
	l.effort = effortValue(level)
	return "Effort set to " + level + ", from the next message on.", nil
}

// useClaude switches to the Claude login just saved, moving off models
// another provider named.
func (l *liveModel) useClaude() (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	change := map[string]any{"auth": "claude-oauth"}
	var changed []string
	model, compact := l.model, l.compactModel
	if !isClaude(model) {
		model = claudeModel
		change["model"] = model
		changed = append(changed, "model "+model)
	}
	if compact != "" && !isClaude(compact) {
		compact = claudeCompactModel
		change["compact-model"] = compact
		changed = append(changed, "memory summaries "+compact)
	}
	if err := l.saveLocked(change); err != nil {
		return "", err
	}
	l.auth, l.model, l.compactModel = "claude-oauth", model, compact
	text := "Logged in to Claude; steel uses your subscription from the next message on."
	if len(changed) > 0 {
		text += " Switched to " + strings.Join(changed, " and ") + "."
	}
	return text + " /model changes the model.", nil
}

// Begin implements telegram.Login: the same headless flow as `steel login`,
// then a switch to the subscription without a restart.
func (l *liveModel) Begin() (telegram.LoginFlow, error) {
	login, err := anthropicauth.NewLogin(anthropicauth.Default(), l.tokensPath)
	if err != nil {
		return nil, err
	}
	return loginFlow{login, l}, nil
}

type loginFlow struct {
	login *anthropicauth.Login
	live  *liveModel
}

func (f loginFlow) URL() string { return f.login.URL() }

func (f loginFlow) Finish(ctx context.Context, code string) (string, error) {
	if err := f.login.Finish(ctx, code); err != nil {
		return "", err
	}
	f.live.redactSaved()
	return f.live.useClaude()
}

// Logout implements telegram.Login: it forgets the Claude login and goes
// back to the API key.
func (l *liveModel) Logout() (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := anthropicauth.RemoveTokens(l.tokensPath); err != nil {
		return "", err
	}
	if l.auth != "claude-oauth" {
		return "Logged out of Claude.", nil
	}
	if err := l.saveLocked(map[string]any{"auth": "api-key"}); err != nil {
		return "", err
	}
	l.auth = "api-key"
	text := "Logged out of Claude; back on your API key."
	if !l.keySet {
		text += " No API key is set: run `steel config set openai-api-key` on the host."
	}
	if isClaude(l.model) {
		text += " The model is still " + l.model + ": set one your provider serves with /model NAME."
	}
	return text, nil
}

// saveLocked writes settings (by name, as in steel config) to config.json,
// leaving the rest alone: a value a flag or the environment gave for one
// run stays unsaved. "" saves no setting.
func (l *liveModel) saveLocked(settings map[string]any) error {
	c, err := readConfig(l.configPath)
	if err != nil {
		return err
	}
	for name, v := range settings {
		if v == "" {
			v = nil
		}
		s, _ := settingNamed(name)
		if err := c.put(s.path, v); err != nil {
			return err
		}
	}
	return c.write()
}

func isClaude(model string) bool { return strings.HasPrefix(model, "claude-") }
