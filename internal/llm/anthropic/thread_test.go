package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/spiiritual/steel/internal/llm"
)

// threadServer behaves like the Messages API with message threads: it
// keeps each thread's conversation, takes a continue request's new
// messages onto it once, and answers every turn with a tool call.
type threadServer struct {
	t           *testing.T
	unsupported bool // refuse threads, as for a HIPAA organization

	mu       sync.Mutex
	threads  map[string]int // response ID -> messages the thread holds
	n        int
	requests []threadRequest
}

type threadRequest struct {
	kind     string // "create", "continue" or "none"
	messages int
	system   []wireBlock
	tools    int
}

func newThreadServer(t *testing.T) (*threadServer, *httptest.Server) {
	s := &threadServer{t: t, threads: map[string]int{}}
	srv := httptest.NewServer(messages(s.serve))
	t.Cleanup(srv.Close)
	return s, srv
}

func (s *threadServer) serve(w http.ResponseWriter, r *http.Request) {
	var req wireRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.t.Errorf("body: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	kind := "none"
	if req.Thread != nil {
		kind = req.Thread.Type
	}
	s.requests = append(s.requests, threadRequest{kind, len(req.Messages), req.System, len(req.Tools)})
	held := len(req.Messages)
	switch {
	case kind != "none" && s.unsupported:
		threadFail(w, http.StatusBadRequest, "thread_unsupported_request")
		return
	case kind == "continue":
		prev, ok := s.threads[req.Thread.PreviousMessageID]
		if !ok {
			threadFail(w, http.StatusNotFound, "thread_not_found")
			return
		}
		delete(s.threads, req.Thread.PreviousMessageID)
		held += prev
	}
	s.n++
	id := fmt.Sprintf("msg_%d", s.n)
	if kind != "none" {
		s.threads[id] = held + 1 // and the reply
	}
	sse(w,
		`{"type":"message_start","message":{"id":"`+id+`","usage":{"input_tokens":1}}}`,
		fmt.Sprintf(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tu_%d","name":"run_code"}}`, s.n),
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}`,
		`{"type":"message_stop"}`,
	)
}

func threadFail(w http.ResponseWriter, status int, code string) {
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"type":"error","error":{"type":"invalid_request_error","message":"%s","details":{"error_code":"%s"}}}`, code, code)
}

func (s *threadServer) forget() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.threads = map[string]int{}
}

func (s *threadServer) log() []threadRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]threadRequest(nil), s.requests...)
}

// toolLoop runs turns as a session does: each reply's tool call gets a
// result, and the next request carries the whole history.
// before, if set, runs before each turn.
func toolLoop(t *testing.T, c *Client, turns int, before func(turn int)) {
	t.Helper()
	history := []llm.Message{{Role: llm.RoleSystem, Content: "sys"}, {Role: llm.RoleUser, Content: "start"}}
	tools := []llm.ToolDef{{Name: "run_code", Parameters: json.RawMessage(`{"type":"object"}`)}}
	for turn := range turns {
		if before != nil {
			before(turn)
		}
		reply, err := c.Stream(context.Background(), llm.Request{Model: "m", Messages: history, Tools: tools}, func(string) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		history = append(history, reply, llm.Message{Role: llm.RoleTool, ToolCallID: reply.ToolCalls[0].ID, Content: "ok"})
	}
}

func kinds(rs []threadRequest) string {
	var parts []string
	for _, r := range rs {
		parts = append(parts, fmt.Sprintf("%s/%d", r.kind, r.messages))
	}
	return strings.Join(parts, " ")
}

func TestThreadSendsOnlyNewMessages(t *testing.T) {
	s, srv := newThreadServer(t)
	c, err := New(Options{BaseURL: srv.URL, AccessToken: func(context.Context) (string, error) { return "tok", nil }, HTTP: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	toolLoop(t, c, 3, nil)
	rs := s.log()
	if got := kinds(rs); got != "create/1 continue/1 continue/1" {
		t.Fatalf("requests = %s", got)
	}
	// A continuation carries no tools, and of system only the billing block.
	for _, r := range rs[1:] {
		if r.tools != 0 || len(r.system) != 1 || !strings.HasPrefix(r.system[0].Text, billingMarker) || r.system[0].CacheControl != nil {
			t.Errorf("continue request: tools %d, system %+v", r.tools, r.system)
		}
	}
	if rs[0].tools != 1 || len(rs[0].system) != 3 {
		t.Errorf("create request: tools %d, system %+v", rs[0].tools, rs[0].system)
	}
}

// A thread the API no longer has is replayed whole in a new one.
func TestLostThreadIsReplayed(t *testing.T) {
	s, srv := newThreadServer(t)
	c, _ := New(Options{BaseURL: srv.URL, APIKey: "k", HTTP: srv.Client()})
	toolLoop(t, c, 4, func(turn int) {
		if turn == 2 {
			s.forget()
		}
	})
	if got := kinds(s.log()); got != "create/1 continue/1 continue/1 create/5 continue/1" {
		t.Fatalf("requests = %s", got)
	}
}

// Where the API refuses threads, steel stops asking.
func TestThreadsUnsupported(t *testing.T) {
	s, srv := newThreadServer(t)
	s.unsupported = true
	c, _ := New(Options{BaseURL: srv.URL, APIKey: "k", HTTP: srv.Client()})
	toolLoop(t, c, 2, nil)
	if got := kinds(s.log()); got != "create/1 none/1 none/3" {
		t.Fatalf("requests = %s", got)
	}
}

// A changed system prompt can't continue a thread: it starts a new one.
func TestChangedSystemStartsNewThread(t *testing.T) {
	s, srv := newThreadServer(t)
	c, _ := New(Options{BaseURL: srv.URL, APIKey: "k", HTTP: srv.Client()})
	tools := []llm.ToolDef{{Name: "run_code"}}
	history := []llm.Message{{Role: llm.RoleSystem, Content: "one"}, {Role: llm.RoleUser, Content: "start"}}
	reply, err := c.Stream(context.Background(), llm.Request{Model: "m", Messages: history, Tools: tools}, func(string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	history = append(history, reply, llm.Message{Role: llm.RoleTool, ToolCallID: reply.ToolCalls[0].ID, Content: "ok"})
	history[0].Content = "two"
	if _, err := c.Stream(context.Background(), llm.Request{Model: "m", Messages: history, Tools: tools}, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if got := kinds(s.log()); got != "create/1 create/3" {
		t.Fatalf("requests = %s", got)
	}
}
