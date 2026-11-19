package anthropicauth

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestBeginHasPKCEAndCopyCodeRedirect(t *testing.T) {
	o := Default()
	p, err := o.Begin()
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(p.URL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if got := q.Get("client_id"); got != ClientID {
		t.Errorf("client_id = %q", got)
	}
	if got := q.Get("redirect_uri"); got != redirectURI {
		t.Errorf("redirect_uri = %q", got)
	}
	if got := q.Get("code_challenge_method"); got != "S256" {
		t.Errorf("code_challenge_method = %q", got)
	}
	if q.Get("code_challenge") == "" || p.Verifier == "" || q.Get("state") != p.Verifier {
		t.Errorf("missing PKCE or state: %+v", p)
	}
	if q.Get("scope") != oauthScope {
		t.Errorf("scope = %q", q.Get("scope"))
	}
}

func TestParseCode(t *testing.T) {
	for _, tc := range []struct {
		in, code, state string
	}{
		{"abc123#st4te", "abc123", "st4te"},
		{"  abc123#st4te\n", "abc123", "st4te"},
		{"https://platform.claude.com/oauth/code/callback?code=abc&state=st", "abc", "st"},
		{"code=abc&state=st", "abc", "st"},
		{"abc", "abc", ""},
	} {
		code, state := parseCode(tc.in)
		if code != tc.code || state != tc.state {
			t.Errorf("parseCode(%q) = %q, %q; want %q, %q", tc.in, code, state, tc.code, tc.state)
		}
	}
}

func TestExchangeSendsPKCE(t *testing.T) {
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("body: %v", err)
		}
		if r.Header.Get("User-Agent") != "axios/1.13.6" {
			t.Errorf("user-agent = %q", r.Header.Get("User-Agent"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}`))
	}))
	defer srv.Close()

	o := &OAuth{AuthorizeURL: "https://example/authorize", TokenURL: srv.URL, ClientID: "cid", RedirectURI: "https://example/callback", Scope: "s", HTTP: srv.Client()}
	p, err := o.Begin()
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := o.Exchange(context.Background(), p, "code123#"+p.Verifier)
	if err != nil {
		t.Fatal(err)
	}
	if tokens.Access != "new-access" || tokens.Refresh != "new-refresh" {
		t.Errorf("tokens = %+v", tokens)
	}
	if want := p.Verifier; got["state"] != want {
		t.Errorf("state = %q, want %q", got["state"], want)
	}
	if got["grant_type"] != "authorization_code" || got["code"] != "code123" || got["code_verifier"] != p.Verifier || got["redirect_uri"] != "https://example/callback" {
		t.Errorf("body = %v", got)
	}
	if d := time.Until(tokens.ExpiresAt); d < 50*time.Minute {
		t.Errorf("expiry %v is not pulled in by the refresh margin", d)
	}
}

func TestExchangeRejectsAnotherLogin(t *testing.T) {
	o := &OAuth{TokenURL: "http://unused", ClientID: "cid", RedirectURI: "r", HTTP: http.DefaultClient}
	p, err := o.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.Exchange(context.Background(), p, "code#wrong-state"); err == nil || !strings.Contains(err.Error(), "different login") {
		t.Fatalf("err = %v", err)
	}
}

func TestSourceRefreshesExpiredToken(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["grant_type"] != "refresh_token" || body["refresh_token"] != "old-refresh" {
			t.Errorf("body = %v", body)
		}
		w.Write([]byte(`{"access_token":"fresh","refresh_token":"next-refresh","expires_in":3600}`))
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "auth.json")
	if err := WriteTokens(path, Tokens{Access: "stale", Refresh: "old-refresh", ExpiresAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	src := &Source{Path: path, OAuth: &OAuth{TokenURL: srv.URL, ClientID: "cid", HTTP: srv.Client()}}
	got, err := src.AccessToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "fresh" || calls != 1 {
		t.Errorf("access = %q after %d refreshes", got, calls)
	}
	saved, err := ReadTokens(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Access != "fresh" || saved.Refresh != "next-refresh" {
		t.Errorf("saved = %+v", saved)
	}
}

func TestSourceWithoutLogin(t *testing.T) {
	src := &Source{Path: filepath.Join(t.TempDir(), "missing.json"), OAuth: Default()}
	_, err := src.AccessToken(context.Background())
	if !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("err = %v", err)
	}
	if _, err := ReadTokens(src.Path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ReadTokens err = %v", err)
	}
}

func TestRemoveTokensAllowsMissing(t *testing.T) {
	if err := RemoveTokens(filepath.Join(t.TempDir(), "missing.json")); err != nil {
		t.Fatal(err)
	}
}

func tokenServer(t *testing.T, status int, body string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func expiredSource(t *testing.T, srv *httptest.Server) *Source {
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := WriteTokens(path, Tokens{Access: "stale", Refresh: "old-refresh", ExpiresAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	return &Source{Path: path, OAuth: &OAuth{TokenURL: srv.URL, ClientID: "cid", HTTP: srv.Client()}}
}

// A token response without expires_in mustn't make every request refresh.
func TestMissingExpiryGetsDefaultLifetime(t *testing.T) {
	srv := tokenServer(t, http.StatusOK, `{"access_token":"a","refresh_token":"r"}`)
	o := &OAuth{TokenURL: srv.URL, ClientID: "cid", HTTP: srv.Client()}
	tokens, err := o.Refresh(context.Background(), "r0")
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Until(tokens.ExpiresAt); d < 50*time.Minute {
		t.Errorf("expires in %v", d)
	}
}

func TestRefreshFailures(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		notLoggedIn bool
	}{
		{"refused", http.StatusBadRequest, true},
		{"server error", http.StatusBadGateway, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := expiredSource(t, tokenServer(t, tc.status, `{"error":"x"}`))
			_, err := src.AccessToken(context.Background())
			if err == nil {
				t.Fatal("no error")
			}
			if errors.Is(err, ErrNotLoggedIn) != tc.notLoggedIn {
				t.Errorf("err = %v", err)
			}
		})
	}
}

// Refresh tokens rotate, so refreshed tokens that couldn't be saved must
// still be used, and saved once the file can be written.
func TestRefreshKeptWhenSaveFails(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Write([]byte(`{"access_token":"fresh","refresh_token":"next-refresh","expires_in":3600}`))
	}))
	defer srv.Close()
	src := expiredSource(t, srv)
	var seen []string
	src.Seen = func(t Tokens) { seen = append(seen, t.Refresh) }
	dir := filepath.Dir(src.Path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)

	for range 2 {
		got, err := src.AccessToken(context.Background())
		if err != nil || got != "fresh" {
			t.Fatalf("access = %q, %v", got, err)
		}
	}
	if calls != 1 {
		t.Errorf("%d refreshes", calls)
	}
	if !slices.Contains(seen, "next-refresh") {
		t.Errorf("seen = %v", seen)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := src.AccessToken(context.Background()); err != nil {
		t.Fatal(err)
	}
	if saved, err := ReadTokens(src.Path); err != nil || saved.Refresh != "next-refresh" {
		t.Errorf("saved = %+v, %v", saved, err)
	}
}
