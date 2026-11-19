// Package anthropicauth signs steel in to a Claude Pro/Max subscription with
// Anthropic's OAuth flow, and keeps the tokens it gets back.
//
// The flow is the copy-code variant, so it works on a headless machine or in
// a container: steel prints a URL, the user signs in on whatever browser they
// have, and pastes back the code Anthropic shows. No callback listener is
// needed.
package anthropicauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/spiiritual/steel/internal/secret"
)

const (
	// ClientID is Claude Code's public OAuth client. Anthropic's OAuth
	// compatibility work depends on presenting the same client Claude Code
	// uses; the value is public, not a secret.
	ClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

	authorizeURL  = "https://claude.ai/oauth/authorize"
	tokenURL      = "https://platform.claude.com/v1/oauth/token"
	redirectURI   = "https://platform.claude.com/oauth/code/callback"
	oauthScope    = "org:create_api_key user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"
	refreshMargin = 5 * time.Minute
	// defaultLifetime stands in for a token response without expires_in, so
	// a missing field doesn't make every request refresh first.
	defaultLifetime = time.Hour
)

// ErrNotLoggedIn means there are no saved tokens yet.
var ErrNotLoggedIn = errors.New("not logged in to Claude: run `steel login`, or send /login on Telegram")

// OAuth holds the endpoints and client a login talks to. Its zero value is
// not usable; use Default. Fields are exported so tests can point it at a
// local server.
type OAuth struct {
	AuthorizeURL string
	TokenURL     string
	ClientID     string
	RedirectURI  string
	Scope        string
	HTTP         *http.Client
}

// Default returns the flow Anthropic's Claude Code uses.
func Default() *OAuth {
	return &OAuth{
		AuthorizeURL: authorizeURL,
		TokenURL:     tokenURL,
		ClientID:     ClientID,
		RedirectURI:  redirectURI,
		Scope:        oauthScope,
		HTTP:         &http.Client{Timeout: 30 * time.Second},
	}
}

// Pending is a login waiting for the code Anthropic shows.
type Pending struct {
	URL string
	// Verifier is the PKCE verifier, which doubles as state, as in Claude
	// Code and pi-ai: Anthropic's copy-code page shows it back after "#".
	Verifier string
}

// Begin makes a PKCE verifier and returns the URL the user opens.
func (o *OAuth) Begin() (Pending, error) {
	verifier, challenge, err := pkce()
	if err != nil {
		return Pending{}, err
	}
	q := url.Values{
		"code":                  {"true"},
		"client_id":             {o.ClientID},
		"response_type":         {"code"},
		"redirect_uri":          {o.RedirectURI},
		"scope":                 {o.Scope},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {verifier},
	}
	return Pending{URL: o.AuthorizeURL + "?" + q.Encode(), Verifier: verifier}, nil
}

// Tokens are what Anthropic returns. ExpiresAt is already pulled in by five
// minutes, so steel refreshes before a request can race the expiry.
type Tokens struct {
	Access, Refresh string
	ExpiresAt       time.Time
}

// parseCode reads what the user pasted. Anthropic shows "code#state"; a full
// redirect URL and a "code=...&state=..." string work too.
func parseCode(input string) (code, state string) {
	s := strings.TrimSpace(input)
	if s == "" {
		return "", ""
	}
	if u, err := url.Parse(s); err == nil && u.Scheme != "" {
		q := u.Query()
		if c := q.Get("code"); c != "" {
			return c, q.Get("state")
		}
	}
	if i := strings.IndexByte(s, '#'); i >= 0 {
		return s[:i], s[i+1:]
	}
	if strings.Contains(s, "code=") {
		if q, err := url.ParseQuery(strings.TrimPrefix(s, "?")); err == nil {
			if c := q.Get("code"); c != "" {
				return c, q.Get("state")
			}
		}
	}
	return s, ""
}

// Exchange trades the pasted code for tokens.
func (o *OAuth) Exchange(ctx context.Context, p Pending, input string) (Tokens, error) {
	code, state := parseCode(input)
	if code == "" {
		return Tokens{}, errors.New("no authorization code given")
	}
	if state != "" && state != p.Verifier {
		return Tokens{}, errors.New("the authorization code is for a different login; start the login again (steel login, or /login on Telegram)")
	}
	if state == "" {
		state = p.Verifier
	}
	return o.token(ctx, map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     o.ClientID,
		"code":          code,
		"state":         state,
		"redirect_uri":  o.RedirectURI,
		"code_verifier": p.Verifier,
	})
}

// Refresh trades a refresh token for a new access token.
func (o *OAuth) Refresh(ctx context.Context, refresh string) (Tokens, error) {
	if refresh == "" {
		return Tokens{}, fmt.Errorf("%w (the saved login has no refresh token)", ErrNotLoggedIn)
	}
	return o.token(ctx, map[string]string{
		"grant_type":    "refresh_token",
		"client_id":     o.ClientID,
		"refresh_token": refresh,
	})
}

func (o *OAuth) token(ctx context.Context, params map[string]string) (Tokens, error) {
	body, err := json.Marshal(params)
	if err != nil {
		return Tokens{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.TokenURL, bytes.NewReader(body))
	if err != nil {
		return Tokens{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("User-Agent", "axios/1.13.6")
	resp, err := o.HTTP.Do(req)
	if err != nil {
		return Tokens{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return Tokens{}, &TokenError{Status: resp.StatusCode, Body: strings.TrimSpace(string(raw))}
	}
	var t struct {
		Access    string `json:"access_token"`
		Refresh   string `json:"refresh_token"`
		ExpiresIn int    `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &t); err != nil {
		return Tokens{}, fmt.Errorf("Claude login: reading the token response: %w", err)
	}
	if t.Access == "" {
		return Tokens{}, errors.New("Claude login: the token response had no access token")
	}
	lifetime := time.Duration(t.ExpiresIn) * time.Second
	if lifetime <= 0 {
		lifetime = defaultLifetime
	}
	return Tokens{
		Access:    t.Access,
		Refresh:   t.Refresh,
		ExpiresAt: time.Now().Add(lifetime - refreshMargin),
	}, nil
}

// TokenError is a non-200 answer from the token endpoint.
type TokenError struct {
	Status int
	Body   string
}

func (e *TokenError) Error() string {
	return fmt.Sprintf("Claude login: %d %s: %s", e.Status, http.StatusText(e.Status), e.Body)
}

// rejected reports whether the server refused the grant itself (an expired,
// revoked or already-used refresh token), as opposed to failing to answer.
func (e *TokenError) rejected() bool {
	return e.Status == http.StatusBadRequest || e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden
}

// Login is one copy-code login in progress.
type Login struct {
	oauth   *OAuth
	path    string
	pending Pending
}

// NewLogin starts a login whose tokens are saved in path.
func NewLogin(o *OAuth, path string) (*Login, error) {
	p, err := o.Begin()
	if err != nil {
		return nil, err
	}
	return &Login{oauth: o, path: path, pending: p}, nil
}

// URL is the address the user opens to sign in.
func (l *Login) URL() string { return l.pending.URL }

// Finish exchanges the pasted code and saves the tokens.
func (l *Login) Finish(ctx context.Context, input string) error {
	t, err := l.oauth.Exchange(ctx, l.pending, input)
	if err != nil {
		return err
	}
	return WriteTokens(l.path, t)
}

// The variables a login is saved under, in steel's own .env with its other
// tokens. The expiry is RFC 3339.
const (
	AccessVar  = "CLAUDE_OAUTH_ACCESS_TOKEN"
	RefreshVar = "CLAUDE_OAUTH_REFRESH_TOKEN"
	ExpiresVar = "CLAUDE_OAUTH_EXPIRES_AT"
)

// Vars are all the variables a login is saved under.
var Vars = []string{AccessVar, RefreshVar, ExpiresVar}

// ReadTokens reads the login saved in the .env file at path. With none
// saved, the error is fs.ErrNotExist.
func ReadTokens(path string) (Tokens, error) {
	d, err := secret.ReadDotenv(path)
	if err != nil {
		return Tokens{}, err
	}
	v := d.Vars()
	if v[AccessVar] == "" {
		return Tokens{}, fs.ErrNotExist
	}
	// An unreadable expiry reads as expired: the next request refreshes.
	expires, _ := time.Parse(time.RFC3339, v[ExpiresVar])
	return Tokens{Access: v[AccessVar], Refresh: v[RefreshVar], ExpiresAt: expires}, nil
}

// WriteTokens saves a login in the .env file at path, keeping its other
// variables. The file is readable only by its owner.
func WriteTokens(path string, t Tokens) error {
	d, err := secret.ReadDotenv(path)
	if err != nil {
		return err
	}
	d.Set(AccessVar, t.Access)
	d.Set(RefreshVar, t.Refresh)
	d.Set(ExpiresVar, t.ExpiresAt.UTC().Format(time.RFC3339))
	return d.Write(path)
}

// RemoveTokens forgets the login saved in the .env file at path. No saved
// login is not an error.
func RemoveTokens(path string) error {
	d, err := secret.ReadDotenv(path)
	if err != nil {
		return err
	}
	removed := false
	for _, v := range Vars {
		if d.Unset(v) {
			removed = true
		}
	}
	if !removed {
		return nil
	}
	return d.Write(path)
}

// Source supplies access tokens to the model client, refreshing them when
// they expire. It's safe for concurrent use: requests share one refresh.
type Source struct {
	Path  string
	OAuth *OAuth
	// Seen, if set, is called with every set of tokens the source uses, so
	// the caller can keep them out of tool results.
	Seen func(Tokens)

	mu sync.Mutex
	// unsaved holds refreshed tokens that couldn't be written. Anthropic
	// rotates refresh tokens, so the file's copy no longer works; these are
	// the login until a write succeeds or a new login replaces the file.
	unsaved *Tokens
	rotated string // the refresh token unsaved replaced
}

// AccessToken returns a usable access token, refreshing and saving it first
// if it has expired.
func (s *Source) AccessToken(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := ReadTokens(s.Path)
	if errors.Is(err, fs.ErrNotExist) {
		s.unsaved = nil // logged out
		return "", ErrNotLoggedIn
	}
	if s.unsaved != nil {
		if err == nil && t.Refresh != s.rotated {
			s.unsaved = nil // a new login replaced the file
		} else {
			t, err = *s.unsaved, nil
			if WriteTokens(s.Path, t) == nil {
				s.unsaved = nil
			}
		}
	}
	if err != nil {
		return "", err
	}
	s.seen(t)
	if time.Now().Before(t.ExpiresAt) {
		return t.Access, nil
	}
	fresh, err := s.OAuth.Refresh(ctx, t.Refresh)
	if err != nil {
		var te *TokenError
		if errors.As(err, &te) && te.rejected() {
			return "", fmt.Errorf("%w (the saved login was refused: %v)", ErrNotLoggedIn, err)
		}
		return "", fmt.Errorf("refreshing the Claude login: %w", err)
	}
	if fresh.Refresh == "" {
		fresh.Refresh = t.Refresh // some servers rotate only the access token
	}
	s.seen(fresh)
	if err := WriteTokens(s.Path, fresh); err != nil {
		// Keep the login working from memory; the next call retries the write.
		s.unsaved, s.rotated = &fresh, t.Refresh
	}
	return fresh.Access, nil
}

func (s *Source) seen(t Tokens) {
	if s.Seen != nil {
		s.Seen(t)
	}
}

func pkce() (verifier, challenge string, err error) {
	var b [64]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(b[:])
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}
