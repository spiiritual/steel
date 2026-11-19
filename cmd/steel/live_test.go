package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spiiritual/steel/internal/anthropicauth"
	"github.com/spiiritual/steel/internal/llm"
)

type recordingProvider struct{ reqs []llm.Request }

func (p *recordingProvider) Stream(ctx context.Context, req llm.Request, onDelta func(string) error) (llm.Message, error) {
	p.reqs = append(p.reqs, req)
	return llm.Message{}, nil
}

func (p *recordingProvider) models() string {
	var m []string
	for _, r := range p.reqs {
		m = append(m, r.Model)
	}
	return strings.Join(m, ",")
}

func testLive(t *testing.T) (*liveModel, *recordingProvider, *recordingProvider) {
	dir := t.TempDir()
	apiKey, claude := &recordingProvider{}, &recordingProvider{}
	return &liveModel{
		configPath: filepath.Join(dir, "config.json"),
		tokensPath: filepath.Join(dir, ".env"),
		apiKey:     apiKey, claude: claude, keySet: true,
		redact: func(string, string) {},
		auth:   "api-key", model: "gpt-4.1", compactModel: "gpt-4.1-nano",
	}, apiKey, claude
}

// call makes a model call the way sessions and memory do.
func call(t *testing.T, l *liveModel, base func() llm.Request) {
	t.Helper()
	if _, err := l.provider().Stream(context.Background(), base(), nil); err != nil {
		t.Fatal(err)
	}
}

func savedSetting(t *testing.T, l *liveModel, name string) string {
	t.Helper()
	c, err := readConfig(l.configPath)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := settingNamed(name)
	v, _, _ := c.text(s.path)
	return v
}

// A Telegram login switches provider and models at once, and saves them.
func TestLoginSwitchesToClaude(t *testing.T) {
	l, apiKey, claude := testLive(t)
	call(t, l, l.turn)
	text, err := l.useClaude()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, claudeModel) || !strings.Contains(text, claudeCompactModel) {
		t.Errorf("notice = %q", text)
	}
	call(t, l, l.turn)
	call(t, l, l.summary)
	if apiKey.models() != "gpt-4.1" || claude.models() != claudeModel+","+claudeCompactModel {
		t.Errorf("api key got %v, claude got %v", apiKey.models(), claude.models())
	}
	for name, want := range map[string]string{"auth": "claude-oauth", "model": claudeModel, "compact-model": claudeCompactModel} {
		if got := savedSetting(t, l, name); got != want {
			t.Errorf("saved %s = %q, want %q", name, got, want)
		}
	}

	if _, err := l.SetModel("claude-opus-5-5"); err != nil {
		t.Fatal(err)
	}
	call(t, l, l.turn)
	if got := claude.reqs[len(claude.reqs)-1].Model; got != "claude-opus-5-5" {
		t.Errorf("after /model: %q", got)
	}

	text, err = l.Logout()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "/model") {
		t.Errorf("logout notice = %q", text)
	}
	call(t, l, l.turn)
	if apiKey.models() != "gpt-4.1,claude-opus-5-5" {
		t.Errorf("after logout the API key got %v", apiKey.models())
	}
}

// Turns get the effort and cap; memory summaries get neither, except low
// effort on Claude, whose thinking would otherwise eat their cap.
func TestEffortAndCap(t *testing.T) {
	l, apiKey, claude := testLive(t)
	l.maxOutput = 16000
	if _, err := l.SetEffort("xhigh"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.SetEffort("extreme"); err == nil {
		t.Error("an unknown effort was accepted")
	}
	call(t, l, l.turn)
	call(t, l, l.summary)
	if _, err := l.useClaude(); err != nil {
		t.Fatal(err)
	}
	call(t, l, l.summary)
	if _, err := l.SetEffort("default"); err != nil {
		t.Fatal(err)
	}
	call(t, l, l.turn)

	turn, summary := apiKey.reqs[0], apiKey.reqs[1]
	if turn.Effort != "xhigh" || turn.MaxTokens != 16000 || summary.Effort != "" || summary.MaxTokens != 0 {
		t.Errorf("API key: turn %+v, summary %+v", turn, summary)
	}
	if claude.reqs[0].Effort != "low" || claude.reqs[1].Effort != "" {
		t.Errorf("Claude: summary effort %q, turn effort after default %q", claude.reqs[0].Effort, claude.reqs[1].Effort)
	}
	if got := savedSetting(t, l, "effort"); got != "default" {
		t.Errorf("saved effort = %q", got)
	}
}

// A Telegram change saves only what it changed: a model given by a flag
// for one run doesn't become the saved one.
func TestSavesOnlyWhatChanged(t *testing.T) {
	l, _, _ := testLive(t)
	if err := os.WriteFile(l.configPath, []byte(`{"model":"saved-model"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	l.model = "flag-model"
	if _, err := l.SetEffort("high"); err != nil {
		t.Fatal(err)
	}
	if got := savedSetting(t, l, "model"); got != "saved-model" {
		t.Errorf("saved model = %q", got)
	}
}

// A login saved on the host while steel runs on the API key is redacted
// before the next call's scripts can read it.
func TestRedactsLoginSavedWhileRunning(t *testing.T) {
	l, _, _ := testLive(t)
	redacted := map[string]string{}
	l.redact = func(name, value string) { redacted[name] = value }
	call(t, l, l.turn)
	if len(redacted) != 0 {
		t.Fatalf("redacted %v with no login", redacted)
	}
	if err := anthropicauth.WriteTokens(l.tokensPath, anthropicauth.Tokens{Access: "acc", Refresh: "ref", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	call(t, l, l.turn)
	if redacted[anthropicauth.AccessVar] != "acc" || redacted[anthropicauth.RefreshVar] != "ref" {
		t.Errorf("redacted %v", redacted)
	}
}
