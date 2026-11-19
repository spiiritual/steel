package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spiiritual/steel/internal/llm"
)

func TestOAuthRequestIsShaped(t *testing.T) {
	var got map[string]any
	var gotURL *http.Request
	srv := httptest.NewServer(messages(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r
		json.NewDecoder(r.Body).Decode(&got)
		sse(w,
			`{"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":0}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
			`{"type":"message_stop"}`,
		)
	}))
	defer srv.Close()

	c, err := New(Options{
		BaseURL:     srv.URL,
		AccessToken: func(context.Context) (string, error) { return "sk-ant-oat-test", nil },
		Version:     "2.1.300",
		HTTP:        srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	req := llm.Request{
		Model: "claude-sonnet-4-5",
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: "steel system"},
			{Role: llm.RoleUser, Content: "hello there this is a prompt"},
			{Role: llm.RoleAssistant, Content: "calling", ToolCalls: []llm.ToolCall{{ID: "tu_1", Name: "run_code", Arguments: `{"code":"1"}`}}},
			{Role: llm.RoleTool, ToolCallID: "tu_1", Content: "done"},
			{Role: llm.RoleUser, Content: "and again"},
		},
		Tools:       []llm.ToolDef{{Name: "run_code", Description: "run", Parameters: json.RawMessage(`{"type":"object"}`)}},
		NoToolCalls: true,
	}
	var deltas strings.Builder
	msg, err := c.Stream(context.Background(), req, func(s string) error { deltas.WriteString(s); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if msg.Content != "hi" || deltas.String() != "hi" {
		t.Errorf("content = %q, deltas = %q", msg.Content, deltas.String())
	}
	if msg.Usage == nil || msg.Usage.Input != 10 || msg.Usage.Output != 2 {
		t.Errorf("usage = %+v", msg.Usage)
	}

	if gotURL.Header.Get("Authorization") != "Bearer sk-ant-oat-test" {
		t.Errorf("authorization = %q", gotURL.Header.Get("Authorization"))
	}
	if h := gotURL.Header.Get("anthropic-beta"); h != "claude-code-20250219,oauth-2025-04-20,thinking-binding-controls-2026-08-01,message-threads-2026-08-12" {
		t.Errorf("anthropic-beta = %q", h)
	}
	if gotURL.Header.Get("user-agent") != "claude-cli/2.1.300" || gotURL.Header.Get("x-app") != "cli" {
		t.Errorf("headers = %v", gotURL.Header)
	}
	if gotURL.URL.Query().Get("beta") != "true" {
		t.Errorf("url = %s", gotURL.URL)
	}

	system := blocks(t, got["system"])
	if len(system) != 3 {
		t.Fatalf("system = %v", system)
	}
	billing, _ := system[0]["text"].(string)
	if !strings.HasPrefix(billing, "x-anthropic-billing-header: cc_version=2.1.300.") {
		t.Errorf("billing = %q", billing)
	}
	if !strings.Contains(billing, "cc_entrypoint=sdk-cli;") {
		t.Errorf("billing = %q", billing)
	}
	if system[1]["text"] != Identity || system[2]["text"] != "steel system" {
		t.Errorf("system = %v", system)
	}

	msgs := list(t, got["messages"])
	if len(msgs) != 3 {
		t.Fatalf("messages = %v", msgs)
	}
	if msgs[0]["role"] != "user" || msgs[1]["role"] != "assistant" || msgs[2]["role"] != "user" {
		t.Errorf("roles = %v", msgs)
	}
	asst := blocks(t, msgs[1]["content"])
	if len(asst) != 2 || asst[1]["type"] != "tool_use" || asst[1]["name"] != "run_code" {
		t.Errorf("assistant blocks = %v", asst)
	}
	if input, ok := asst[1]["input"].(map[string]any); !ok || input["code"] != "1" {
		t.Errorf("tool input = %v", asst[1]["input"])
	}
	user := blocks(t, msgs[2]["content"])
	if len(user) != 2 || user[0]["type"] != "tool_result" || user[0]["tool_use_id"] != "tu_1" || user[1]["text"] != "and again" {
		t.Errorf("tool result blocks = %v", user)
	}
	if tc, ok := got["tool_choice"].(map[string]any); !ok || tc["type"] != "none" {
		t.Errorf("tool_choice = %v", got["tool_choice"])
	}
	if th, ok := got["thinking"].(map[string]any); !ok || th["type"] != "adaptive" {
		t.Errorf("thinking = %v", got["thinking"])
	}
}

func TestAPIKeyRequestIsNotShaped(t *testing.T) {
	var got map[string]any
	var headers http.Header
	srv := httptest.NewServer(messages(func(w http.ResponseWriter, r *http.Request) {
		headers = r.Header
		json.NewDecoder(r.Body).Decode(&got)
		sse(w, `{"type":"message_start","message":{"usage":{"input_tokens":1}}}`, `{"type":"message_stop"}`)
	}))
	defer srv.Close()

	c, err := New(Options{BaseURL: srv.URL, APIKey: "key", HTTP: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Stream(context.Background(), llm.Request{Model: "m", Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}}}, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if headers.Get("x-api-key") != "key" || headers.Get("Authorization") != "" {
		t.Errorf("headers = %v", headers)
	}
	if headers.Get("anthropic-beta") != "thinking-binding-controls-2026-08-01,message-threads-2026-08-12" {
		t.Errorf("anthropic-beta = %q", headers.Get("anthropic-beta"))
	}
	if _, ok := got["system"]; ok {
		t.Errorf("system = %v", got["system"])
	}
}

func TestStreamsToolCalls(t *testing.T) {
	srv := httptest.NewServer(messages(func(w http.ResponseWriter, r *http.Request) {
		sse(w,
			`{"type":"message_start","message":{"usage":{"input_tokens":5}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tu_9","name":"run_code"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"code\":\""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"1+1\"}"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":7}}`,
			`{"type":"message_stop"}`,
		)
	}))
	defer srv.Close()

	c, _ := New(Options{BaseURL: srv.URL, APIKey: "k", HTTP: srv.Client()})
	msg, err := c.Stream(context.Background(), llm.Request{Model: "m", Messages: []llm.Message{{Role: llm.RoleUser, Content: "go"}}}, func(string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v", msg.ToolCalls)
	}
	tc := msg.ToolCalls[0]
	if tc.ID != "tu_9" || tc.Name != "run_code" || tc.Arguments != `{"code":"1+1"}` {
		t.Errorf("tool call = %+v", tc)
	}
}

func TestTruncatedReply(t *testing.T) {
	srv := httptest.NewServer(messages(func(w http.ResponseWriter, r *http.Request) {
		sse(w,
			`{"type":"message_start","message":{"usage":{"input_tokens":5}}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"part"}}`,
			`{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":5}}`,
			`{"type":"message_stop"}`,
		)
	}))
	defer srv.Close()

	c, _ := New(Options{BaseURL: srv.URL, APIKey: "k", HTTP: srv.Client()})
	msg, err := c.Stream(context.Background(), llm.Request{Model: "m", MaxTokens: 5, Messages: []llm.Message{{Role: llm.RoleUser, Content: "go"}}}, func(string) error { return nil })
	if !errors.Is(err, llm.ErrTruncated) {
		t.Fatalf("err = %v", err)
	}
	if msg.Content != "part" {
		t.Errorf("content = %q", msg.Content)
	}
}

func TestLearnsVersionFloorFromRejection(t *testing.T) {
	t.Setenv(VersionEnv, "")
	var versions []string
	srv := httptest.NewServer(messages(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			System []struct {
				Text string `json:"text"`
			} `json:"system"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		versions = append(versions, body.System[0].Text)
		if len(versions) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"type":"error","error":{"type":"invalid_request_error","message":"Claude Code 2.1.323 does not support this model; version 2.1.400 or newer is required.","details":{"error_code":"claude_code_version_too_old"}}}`)
			return
		}
		sse(w, `{"type":"message_start","message":{"usage":{"input_tokens":1}}}`, `{"type":"message_stop"}`)
	}))
	defer srv.Close()

	c, err := New(Options{
		BaseURL:     srv.URL,
		AccessToken: func(context.Context) (string, error) { return "oat", nil },
		HTTP:        srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	req := llm.Request{Model: "m", Messages: []llm.Message{{Role: llm.RoleUser, Content: "hello there"}}}
	if _, err := c.Stream(context.Background(), req, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(versions) != 2 {
		t.Fatalf("requests = %d", len(versions))
	}
	if !strings.Contains(versions[1], "cc_version=2.1.400.") {
		t.Errorf("retry header = %q", versions[1])
	}
}

func TestVersionOverrideIsAbsolute(t *testing.T) {
	var first string
	srv := httptest.NewServer(messages(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			System []struct {
				Text string `json:"text"`
			} `json:"system"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		first = body.System[0].Text
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"type":"error","error":{"type":"invalid_request_error","message":"version 2.1.500 or newer is required","details":{"error_code":"claude_code_version_too_old"}}}`)
	}))
	defer srv.Close()

	c, err := New(Options{
		BaseURL:     srv.URL,
		AccessToken: func(context.Context) (string, error) { return "oat", nil },
		Version:     "2.1.400",
		HTTP:        srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	req := llm.Request{Model: "m", Messages: []llm.Message{{Role: llm.RoleUser, Content: "hello there"}}}
	_, err = c.Stream(context.Background(), req, func(string) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "STEEL_CLAUDE_CODE_VERSION=2.1.400") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(first, "cc_version=2.1.400.") {
		t.Errorf("header = %q", first)
	}
}

func TestRejectsMalformedVersion(t *testing.T) {
	if _, err := New(Options{Version: "2.1"}); err == nil {
		t.Fatal("want an error")
	}
}

func TestBillingHeaderMatchesClaudeCodeRecipe(t *testing.T) {
	// Salted suffix from the recipe used by Claude Code, pi and OpenCode.
	got := billingHeader([]llm.Message{{Role: llm.RoleUser, Content: "hello there this is a prompt"}}, "2.1.300")
	if !strings.HasPrefix(got, "x-anthropic-billing-header: cc_version=2.1.300.") {
		t.Fatalf("header = %q", got)
	}
	if !strings.Contains(got, "cch=") || !strings.HasSuffix(got, ";") {
		t.Fatalf("header = %q", got)
	}
}

func sse(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, e := range events {
		fmt.Fprintf(w, "event: x\ndata: %s\n\n", e)
	}
}

func blocks(t *testing.T, v any) []map[string]any {
	t.Helper()
	raw, ok := v.([]any)
	if !ok {
		t.Fatalf("not a list: %#v", v)
	}
	out := make([]map[string]any, len(raw))
	for i, b := range raw {
		out[i], ok = b.(map[string]any)
		if !ok {
			t.Fatalf("block %d is %#v", i, b)
		}
	}
	return out
}

func list(t *testing.T, v any) []map[string]any {
	t.Helper()
	return blocks(t, v)
}

func TestMarksPromptForCaching(t *testing.T) {
	var got wireRequest
	srv := httptest.NewServer(messages(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		sse(w, `{"type":"message_start","message":{"usage":{"input_tokens":1}}}`, `{"type":"message_stop"}`)
	}))
	defer srv.Close()

	c, err := New(Options{BaseURL: srv.URL, APIKey: "key", HTTP: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	req := llm.Request{Model: "m", Messages: []llm.Message{
		{Role: llm.RoleSystem, Content: "sys"},
		{Role: llm.RoleUser, Content: "hi"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "t1", Name: "run_code", Arguments: `{}`}}},
		{Role: llm.RoleTool, ToolCallID: "t1", Content: "out"},
	}}
	if _, err := c.Stream(context.Background(), req, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	want := cacheControl{Type: "ephemeral", TTL: "1h"}
	if n := len(got.System); n == 0 || got.System[n-1].CacheControl == nil || *got.System[n-1].CacheControl != want {
		t.Errorf("system = %+v", got.System)
	}
	// The conversation is cached by the top-level mark: message blocks
	// stay unmarked, so they're the same bytes in every request.
	if got.CacheControl == nil || *got.CacheControl != want {
		t.Errorf("cache_control = %+v", got.CacheControl)
	}
	for _, m := range got.Messages {
		for _, b := range m.Content {
			if b.CacheControl != nil {
				t.Errorf("marked block %+v", b)
			}
		}
	}
	if got.MaxTokens != 128000 {
		t.Errorf("max_tokens = %d", got.MaxTokens)
	}
}

// messages serves h for the Messages API, and answers model lookups as the
// Models API does for a current model.
func messages(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/models/") {
			w.Write([]byte(`{"id":"m","max_tokens":128000,"capabilities":{"thinking":{"supported":true,"types":{"adaptive":{"supported":true}}}}}`))
			return
		}
		h(w, r)
	})
}

// A reply's signed thinking goes back exactly as it came, with its tool call.
func TestThinkingIsReplayed(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(messages(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		bodies = append(bodies, b)
		sse(w,
			`{"type":"message_start","message":{"usage":{"input_tokens":1}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"plan"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig=="}}`,
			`{"type":"content_block_start","index":1,"content_block":{"type":"redacted_thinking","data":"opaque"}}`,
			`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"tu_1","name":"run_code"}}`,
			`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"code\":\"1\"}"}}`,
			`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":3}}`,
			`{"type":"message_stop"}`,
		)
	}))
	defer srv.Close()
	c, err := New(Options{BaseURL: srv.URL, APIKey: "k", HTTP: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	history := []llm.Message{{Role: llm.RoleUser, Content: "go"}}
	reply, err := c.Stream(context.Background(), llm.Request{Model: "m", Messages: history}, func(string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(reply.ToolCalls) != 1 || reply.ToolCalls[0].Arguments != `{"code":"1"}` || len(reply.Blocks) == 0 {
		t.Fatalf("reply = %+v", reply)
	}
	// Saved with history and read back, as a session does.
	saved, err := json.Marshal(reply)
	if err != nil {
		t.Fatal(err)
	}
	var loaded llm.Message
	if err := json.Unmarshal(saved, &loaded); err != nil {
		t.Fatal(err)
	}
	history = append(history, loaded, llm.Message{Role: llm.RoleTool, ToolCallID: "tu_1", Content: "1"})
	if _, err := c.Stream(context.Background(), llm.Request{Model: "m", Messages: history}, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	asst := blocks(t, bodies[1]["messages"].([]any)[1].(map[string]any)["content"])
	want := []map[string]any{
		{"type": "thinking", "thinking": "plan", "signature": "sig=="},
		{"type": "redacted_thinking", "data": "opaque"},
		{"type": "tool_use", "id": "tu_1", "name": "run_code", "input": map[string]any{"code": "1"}},
	}
	if g, w := fmt.Sprint(asst), fmt.Sprint(want); g != w {
		t.Errorf("replayed\n got %s\nwant %s", g, w)
	}
	th := bodies[1]["thinking"].(map[string]any)
	if th["type"] != "adaptive" || fmt.Sprint(th["block_binding"]) != "map[prefix_mismatch_behavior:drop_block]" {
		t.Errorf("thinking = %v", th)
	}
}

func TestModelLookup(t *testing.T) {
	for _, tc := range []struct {
		name      string
		models    string // "" answers 404
		maxTokens float64
		thinking  bool
	}{
		{"current model", `{"max_tokens":64000,"capabilities":{"thinking":{"types":{"adaptive":{"supported":true}}}}}`, 64000, true},
		{"no adaptive thinking", `{"max_tokens":64000,"capabilities":{"thinking":{"types":{"adaptive":{"supported":false}}}}}`, 64000, false},
		{"lookup fails", "", fallbackMaxTokens, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					if tc.models == "" {
						http.NotFound(w, r)
						return
					}
					w.Write([]byte(tc.models))
					return
				}
				json.NewDecoder(r.Body).Decode(&got)
				sse(w, `{"type":"message_start","message":{"usage":{"input_tokens":1}}}`, `{"type":"message_stop"}`)
			}))
			defer srv.Close()
			c, _ := New(Options{BaseURL: srv.URL, APIKey: "k", HTTP: srv.Client()})
			if _, err := c.Stream(context.Background(), llm.Request{Model: "m", Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}}}, func(string) error { return nil }); err != nil {
				t.Fatal(err)
			}
			if got["max_tokens"] != tc.maxTokens {
				t.Errorf("max_tokens = %v", got["max_tokens"])
			}
			if _, ok := got["thinking"]; ok != tc.thinking {
				t.Errorf("thinking = %v", got["thinking"])
			}
		})
	}
}

// A request with its own cap (a memory summary) keeps it, and thinks briefly.
// A cap limits the reply's length only: the model's default effort stays,
// so max-output-tokens doesn't make every turn think less.
func TestCapKeepsDefaultEffort(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(messages(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		sse(w, `{"type":"message_start","message":{"usage":{"input_tokens":1}}}`, `{"type":"message_stop"}`)
	}))
	defer srv.Close()
	c, _ := New(Options{BaseURL: srv.URL, APIKey: "k", HTTP: srv.Client()})
	if _, err := c.Stream(context.Background(), llm.Request{Model: "m", MaxTokens: 4096, Messages: []llm.Message{{Role: llm.RoleUser, Content: "sum"}}}, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if got["max_tokens"] != float64(4096) || got["output_config"] != nil {
		t.Errorf("max_tokens = %v, output_config = %v", got["max_tokens"], got["output_config"])
	}
}

func TestRefusalIsAnError(t *testing.T) {
	srv := httptest.NewServer(messages(func(w http.ResponseWriter, r *http.Request) {
		sse(w,
			`{"type":"message_start","message":{"usage":{"input_tokens":1}}}`,
			`{"type":"message_delta","delta":{"stop_reason":"refusal"},"usage":{"output_tokens":1}}`,
			`{"type":"message_stop"}`,
		)
	}))
	defer srv.Close()
	c, _ := New(Options{BaseURL: srv.URL, APIKey: "k", HTTP: srv.Client()})
	_, err := c.Stream(context.Background(), llm.Request{Model: "m", Messages: []llm.Message{{Role: llm.RoleUser, Content: "x"}}}, func(string) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "declined") {
		t.Fatalf("err = %v", err)
	}
}

// A level the model lacks becomes the highest one below it.
func TestEffortFallsToSupportedLevel(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Write([]byte(`{"max_tokens":128000,"capabilities":{"thinking":{"types":{"adaptive":{"supported":true}}},` +
				`"effort":{"supported":true,"low":{"supported":true},"medium":{"supported":true},"high":{"supported":true},"xhigh":{"supported":false},"max":{"supported":true}}}}`))
			return
		}
		json.NewDecoder(r.Body).Decode(&got)
		sse(w, `{"type":"message_start","message":{"usage":{"input_tokens":1}}}`, `{"type":"message_stop"}`)
	}))
	defer srv.Close()
	c, _ := New(Options{BaseURL: srv.URL, APIKey: "k", HTTP: srv.Client()})
	for want, sent := range map[string]string{"xhigh": "high", "max": "max", "": "<nil>"} {
		got = nil
		if _, err := c.Stream(context.Background(), llm.Request{Model: "m", Effort: want, Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}}}, func(string) error { return nil }); err != nil {
			t.Fatal(err)
		}
		effort := "<nil>"
		if oc, ok := got["output_config"].(map[string]any); ok {
			effort = fmt.Sprint(oc["effort"])
		}
		if effort != sent {
			t.Errorf("effort %q sent as %s, want %s", want, effort, sent)
		}
	}
}
