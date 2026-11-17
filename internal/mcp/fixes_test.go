package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func pingServer(instructions string) *sdk.Server {
	srv := sdk.NewServer(&sdk.Implementation{Name: "ping", Version: "1"}, &sdk.ServerOptions{Instructions: instructions})
	srv.AddTool(&sdk.Tool{Name: "ping", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "pong"}}}, nil
		})
	return srv
}

// Review #1: SSE sessions died once connect's timeout context was cancelled.
func TestSSEStaysConnectedAfterConnect(t *testing.T) {
	srv := pingServer("")
	hs := httptest.NewServer(sdk.NewSSEHandler(func(*http.Request) *sdk.Server { return srv }, nil))
	defer hs.Close()

	s, _, err := dial(context.Background(), "s", ServerConfig{Type: "sse", URL: hs.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer s.session.Close()
	if out, err := s.tools[0].Run(context.Background(), json.RawMessage(`{}`)); err != nil || out != "pong" {
		t.Errorf("call after connect = %v, %v", out, err)
	}
}

// Review #3: endpoint paths and queries can hold credentials.
func TestRedactURLs(t *testing.T) {
	err := redactURLs(errors.New(`rejected by transport: Post "https://mcp.example.com/s/SECRET-TOKEN?api_key=K": dial tcp: refused`))
	if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "api_key") || !strings.Contains(err.Error(), "https://mcp.example.com/") {
		t.Errorf("redacted = %q", err)
	}
}

// Review #4: configured headers must not follow redirects to other hosts.
func TestHeadersOnlyForConfiguredHost(t *testing.T) {
	var got []string
	rt := headerTransport{host: "a.example", headers: map[string]string{"Authorization": "Bearer s3cret"},
		base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			got = append(got, r.URL.Host+"="+r.Header.Get("Authorization"))
			return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
		})}
	for _, host := range []string{"a.example", "b.example"} {
		rt.RoundTrip(&http.Request{URL: &url.URL{Scheme: "https", Host: host}, Header: http.Header{}})
	}
	if strings.Join(got, ",") != "a.example=Bearer s3cret,b.example=" {
		t.Errorf("headers sent = %v", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Review #5: ${VAR} and ${VAR:-default} in every field; bare $ is literal.
func TestExpandVars(t *testing.T) {
	t.Setenv("STEEL_T_KEY", "k1")
	t.Setenv("STEEL_T_EMPTY", "")
	for in, want := range map[string]string{
		"Bearer ${STEEL_T_KEY}":                "Bearer k1",
		"Bearer ${STEEL_T_UNSET:-x}":           "Bearer x",
		"${STEEL_T_EMPTY:-https://a.example}/": "https://a.example/",
		"tok$en and $HOME":                     "tok$en and $HOME",
	} {
		if got, err := expandVars(in); err != nil || got != want {
			t.Errorf("expandVars(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := expandVars("${STEEL_T_UNSET}"); err == nil || !strings.Contains(err.Error(), "STEEL_T_UNSET is not set") {
		t.Errorf("unset var err = %v", err)
	}
	cfg, err := ServerConfig{Command: "${STEEL_T_KEY}", Args: []string{"--k=${STEEL_T_KEY}"}, Env: map[string]string{"T": "${STEEL_T_KEY}"},
		URL: "https://${STEEL_T_KEY}.example", Headers: map[string]string{"X": "${STEEL_T_KEY}"}}.expanded()
	if err != nil || cfg.Command != "k1" || cfg.Args[0] != "--k=k1" || cfg.Env["T"] != "k1" || cfg.URL != "https://k1.example" || cfg.Headers["X"] != "k1" {
		t.Errorf("expanded = %+v, %v", cfg, err)
	}
}

// Review #12: steel's own secrets stay out of stdio servers' environments.
func TestServerEnvAllowlist(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "secret")
	env := strings.Join(serverEnv(map[string]string{"GITHUB_TOKEN": "g"}), "\n")
	if strings.Contains(env, "OPENAI_API_KEY") || !strings.Contains(env, "GITHUB_TOKEN=g") || !strings.Contains(env, "PATH=") {
		t.Errorf("env = %s", env)
	}
}

// Review #7 and #8: bad tool names are skipped individually; a server left
// with no tools is dropped.
func TestToolNameProblemsSkipToolsNotServers(t *testing.T) {
	srv := sdk.NewServer(&sdk.Implementation{Name: "x", Version: "1"}, nil)
	empty := json.RawMessage(`{"type":"object"}`)
	h := func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{}, nil
	}
	for _, name := range []string{"搜索", "get-user", "get.user", "!!!"} {
		srv.AddTool(&sdk.Tool{Name: name, InputSchema: empty}, h)
	}
	ct, st := sdk.NewInMemoryTransports()
	ss, _ := srv.Connect(context.Background(), st, nil)
	defer ss.Close()
	s, warnings, err := connect(context.Background(), "cn", ct)
	if err != nil {
		t.Fatal(err)
	}
	defer s.session.Close()
	var names []string
	for _, tl := range s.tools {
		names = append(names, tl.Name)
	}
	if len(names) != 2 || !strings.Contains(strings.Join(names, ","), "cn.搜索") || len(warnings) != 2 {
		t.Errorf("tools = %v, warnings = %v", names, warnings)
	}

	noTools := sdk.NewServer(&sdk.Implementation{Name: "docs", Version: "1"}, &sdk.ServerOptions{Instructions: "Read docs://x"})
	ct2, st2 := sdk.NewInMemoryTransports()
	ss2, _ := noTools.Connect(context.Background(), st2, nil)
	defer ss2.Close()
	if _, _, err := connect(context.Background(), "docs", ct2); err == nil {
		t.Error("server without tools was kept")
	}
}

// Review #13: instructions are capped.
func TestGuidanceCapped(t *testing.T) {
	c := &Client{servers: []*server{
		{name: "my-srv", ns: "my_srv", instructions: "Use x"},
		{name: "b", ns: "b", instructions: strings.Repeat("y", 3*maxInstructions)},
		{name: "c", ns: "c"},
	}}
	got := c.Guidance()
	if len(got) != 2 || got["my_srv"] != "Use x" {
		t.Errorf("guidance = %q", got)
	}
	if b := got["b"]; len(b) > maxInstructions+20 || !strings.HasSuffix(b, "[truncated]") {
		t.Errorf("not capped: %d bytes", len(b))
	}
}

// Review #15: structured-only results without an output schema aren't lost.
func TestStructuredWithoutSchemaFallsBack(t *testing.T) {
	srv := sdk.NewServer(&sdk.Implementation{Name: "x", Version: "1"}, nil)
	srv.AddTool(&sdk.Tool{Name: "data", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{StructuredContent: map[string]any{"n": 1}}, nil
		})
	ct, st := sdk.NewInMemoryTransports()
	ss, _ := srv.Connect(context.Background(), st, nil)
	defer ss.Close()
	s, _, err := connect(context.Background(), "x", ct)
	if err != nil {
		t.Fatal(err)
	}
	defer s.session.Close()
	out, err := s.tools[0].Run(context.Background(), json.RawMessage(`{}`))
	if m, ok := out.(map[string]any); err != nil || !ok || m["n"] != float64(1) {
		t.Errorf("out = %#v, %v", out, err)
	}
}

// A wrong guess fails locally, before any request, with the signature in
// the error so the next attempt can be right.
func TestBadArgumentsFailLocallyWithSignature(t *testing.T) {
	var calls atomic.Int32
	srv := sdk.NewServer(&sdk.Implementation{Name: "x", Version: "1"}, nil)
	srv.AddTool(&sdk.Tool{Name: "read", InputSchema: json.RawMessage(`{"type":"object","properties":{"repoName":{"type":"string"}},"required":["repoName"]}`)},
		func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			calls.Add(1)
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "ok"}}}, nil
		})
	ct, st := sdk.NewInMemoryTransports()
	ss, _ := srv.Connect(context.Background(), st, nil)
	defer ss.Close()
	s, _, err := connect(context.Background(), "wiki", ct)
	if err != nil {
		t.Fatal(err)
	}
	defer s.session.Close()

	_, err = s.tools[0].Run(context.Background(), json.RawMessage(`{"owner":"a","repo":"b"}`))
	if err == nil || !strings.Contains(err.Error(), "repoName") || !strings.Contains(err.Error(), "Expected: tools.wiki.read(args: { repoName: string }): Promise<string>") {
		t.Errorf("err = %v", err)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("server was called %d times with invalid arguments", n)
	}
	if out, err := s.tools[0].Run(context.Background(), json.RawMessage(`{"repoName":"a/b"}`)); err != nil || out != "ok" || calls.Load() != 1 {
		t.Errorf("valid call = %v, %v (calls %d)", out, err, calls.Load())
	}
}
