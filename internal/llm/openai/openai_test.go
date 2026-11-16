package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spiiritual/steel/internal/llm"
)

func TestStreamAccumulatesDeltas(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("Authorization = %q", got)
		}
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		if req.Model != "m" || !req.Stream || len(req.Messages) != 1 || req.Messages[0].Role != llm.RoleUser {
			t.Errorf("unexpected request: %+v", req)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, ": keep-alive\n\n")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"role":"assistant"}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"Hel"}}]}`+"\n\n")
		fmt.Fprint(w, `data:{"choices":[{"delta":{"content":"lo"}}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	var deltas []string
	msg, err := New(srv.URL+"/v1/", "sk-test").Stream(context.Background(), llm.Request{
		Model:    "m",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}, func(d string) error { deltas = append(deltas, d); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if msg.Role != llm.RoleAssistant || msg.Content != "Hello" {
		t.Errorf("msg = %+v", msg)
	}
	if strings.Join(deltas, "|") != "Hel|lo" {
		t.Errorf("deltas = %q", deltas)
	}
}

func TestStreamErrors(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		want    string
	}{
		{"http status", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":{"message":"bad key"}}`)
		}, "/chat/completions: 401 Unauthorized: bad key"},
		{"in-stream error", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `data: {"error":{"message":"overloaded"}}`+"\n\n")
		}, "api error: overloaded"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()
			_, err := New(srv.URL, "").Stream(context.Background(), llm.Request{Model: "m"}, func(string) error { return nil })
			if err == nil || !strings.HasSuffix(err.Error(), tt.want) {
				t.Errorf("err = %v, want suffix %q", err, tt.want)
			}
		})
	}
}

func TestStreamToolCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		msgs := req["messages"].([]any)
		asst := msgs[1].(map[string]any)
		if c, ok := asst["content"]; !ok || c != nil {
			t.Errorf("tool-call-only assistant content = %#v, want null", c)
		}
		call := asst["tool_calls"].([]any)[0].(map[string]any)
		fn := call["function"].(map[string]any)
		if call["id"] != "c0" || call["type"] != "function" || fn["name"] != "bash" || fn["arguments"] != `{"cmd":"ls"}` {
			t.Errorf("assistant tool_call = %#v", call)
		}
		toolMsg := msgs[2].(map[string]any)
		if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "c0" || toolMsg["content"] != "a.go" {
			t.Errorf("tool message = %#v", toolMsg)
		}
		tools := req["tools"].([]any)
		if tf := tools[0].(map[string]any)["function"].(map[string]any); tf["name"] != "bash" || tf["parameters"] == nil {
			t.Errorf("tools = %#v", tools)
		}

		// Two parallel calls; names and ids arrive once, arguments are split.
		for _, c := range []string{
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"x1","type":"function","function":{"name":"read_file","arguments":""}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"x2","type":"function","function":{"name":"bash","arguments":"{\"cmd\":\"pwd\"}"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"go.mod\"}"}}]}}]}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", c)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	params := json.RawMessage(`{"type":"object"}`)
	msg, err := New(srv.URL, "").Stream(context.Background(), llm.Request{
		Model: "m",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "list"},
			{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "c0", Name: "bash", Arguments: json.RawMessage(`{"cmd":"ls"}`)}}},
			{Role: llm.RoleTool, ToolCallID: "c0", Content: "a.go"},
		},
		Tools: []llm.ToolDef{{Name: "bash", Parameters: params}},
	}, func(string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	want := []llm.ToolCall{
		{ID: "x1", Name: "read_file", Arguments: json.RawMessage(`{"path":"go.mod"}`)},
		{ID: "x2", Name: "bash", Arguments: json.RawMessage(`{"cmd":"pwd"}`)},
	}
	if len(msg.ToolCalls) != len(want) {
		t.Fatalf("tool calls = %+v", msg.ToolCalls)
	}
	for i, w := range want {
		got := msg.ToolCalls[i]
		if got.ID != w.ID || got.Name != w.Name || string(got.Arguments) != string(w.Arguments) {
			t.Errorf("call %d = %+v, want %+v (args %s)", i, got, w, got.Arguments)
		}
	}
}
