package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const testToken = "123:secret-token"

// fakeAPI is a Bot API server. getUpdates long-polls a queue of updates and
// confirms them like Telegram: calling it with an offset forgets every
// earlier update.
type fakeAPI struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	calls    []apiCall
	updates  []map[string]any
	changed  chan struct{} // closed and replaced when updates change
	canned   map[string][]cannedResponse
	nextMsg  int
	username string
}

type apiCall struct {
	Method string
	Params map[string]any
}

type cannedResponse struct {
	status int
	body   string
}

func newFakeAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{t: t, changed: make(chan struct{}), canned: map[string][]cannedResponse{}, username: "steel_bot"}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	method, ok := strings.CutPrefix(r.URL.Path, "/bot"+testToken+"/")
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"ok":false,"error_code":401,"description":"Unauthorized"}`)
		return
	}
	var params map[string]any
	dec := json.NewDecoder(r.Body)
	dec.UseNumber()
	if err := dec.Decode(&params); err != nil {
		f.t.Errorf("%s: bad body: %v", method, err)
	}
	f.mu.Lock()
	f.calls = append(f.calls, apiCall{method, params})
	if q := f.canned[method]; len(q) > 0 {
		f.canned[method] = q[1:]
		f.mu.Unlock()
		w.WriteHeader(q[0].status)
		fmt.Fprint(w, q[0].body)
		return
	}
	f.mu.Unlock()

	var result any = true
	switch method {
	case "getMe":
		result = map[string]any{"id": 1, "is_bot": true, "username": f.username}
	case "sendMessage":
		f.mu.Lock()
		f.nextMsg++
		result = map[string]any{"message_id": f.nextMsg}
		f.mu.Unlock()
	case "getUpdates":
		result = f.getUpdates(r.Context(), params)
	}
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}

func (f *fakeAPI) getUpdates(ctx context.Context, params map[string]any) []map[string]any {
	offset := num(params["offset"])
	timeout := time.Duration(num(params["timeout"])) * time.Second
	limit := int(num(params["limit"]))
	deadline := time.After(timeout)
	for {
		f.mu.Lock()
		if offset > 0 {
			f.updates = filter(f.updates, func(u map[string]any) bool { return num(u["update_id"]) >= offset })
		}
		ups := append([]map[string]any(nil), f.updates...)
		if limit > 0 && len(ups) > limit {
			ups = ups[:limit]
		}
		changed := f.changed
		f.mu.Unlock()
		if len(ups) > 0 || timeout == 0 {
			return ups
		}
		select {
		case <-changed:
		case <-deadline:
			return nil
		case <-ctx.Done():
			return nil
		}
	}
}

func filter[T any](s []T, keep func(T) bool) []T {
	var out []T
	for _, v := range s {
		if keep(v) {
			out = append(out, v)
		}
	}
	return out
}

func num(v any) int64 {
	switch v := v.(type) {
	case json.Number:
		n, _ := v.Int64()
		return n
	case float64:
		return int64(v)
	case int:
		return int64(v)
	case int64:
		return v
	}
	return 0
}

// add queues updates for getUpdates.
func (f *fakeAPI) add(ups ...map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, ups...)
	close(f.changed)
	f.changed = make(chan struct{})
}

// respond makes the next call of method get status and body.
func (f *fakeAPI) respond(method string, status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.canned[method] = append(f.canned[method], cannedResponse{status, body})
}

// callsOf returns the calls of method so far.
func (f *fakeAPI) callsOf(method string) []apiCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return filter(f.calls, func(c apiCall) bool { return c.Method == method })
}

// texts returns the text params of method's calls.
func (f *fakeAPI) texts(method string) []string {
	var out []string
	for _, c := range f.callsOf(method) {
		s, _ := c.Params["text"].(string)
		out = append(out, s)
	}
	return out
}

// pending returns the IDs of updates not yet confirmed.
func (f *fakeAPI) pending() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []int64
	for _, u := range f.updates {
		ids = append(ids, num(u["update_id"]))
	}
	return ids
}

func textUpdate(id, from, chat int64, text string) map[string]any {
	typ := "private"
	if chat < 0 {
		typ = "group"
	}
	return map[string]any{"update_id": id, "message": map[string]any{
		"message_id": id,
		"from":       map[string]any{"id": from, "is_bot": false, "first_name": "U", "username": fmt.Sprintf("user%d", from)},
		"chat":       map[string]any{"id": chat, "type": typ},
		"text":       text,
	}}
}

// stopUpdate is a stop-button press. draft_id is a string, as real Telegram
// sends it, though the docs say Integer.
func stopUpdate(id, chat, draft int64) map[string]any {
	return map[string]any{"update_id": id, "stopped_message_generation": map[string]any{
		"chat":     map[string]any{"id": chat, "type": "private"},
		"draft_id": fmt.Sprint(draft),
	}}
}

// eventually fails the test unless cond becomes true within a few seconds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// syncBuffer is a log safe for concurrent writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// fakeSessions records what the bot hands over.
type fakeSessions struct {
	mu   sync.Mutex
	sent []string // "id: text"
	hook func(id, text string)
}

func (s *fakeSessions) Send(id, text string) {
	s.mu.Lock()
	s.sent = append(s.sent, id+": "+text)
	hook := s.hook
	s.mu.Unlock()
	if hook != nil {
		hook(id, text)
	}
}

// Answer records the answer and reports whether the request was waiting:
// only IDs starting with "live" are.
func (s *fakeSessions) Answer(id, approvalID string, allow bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, fmt.Sprintf("%s: answer %s %v", id, approvalID, allow))
	return strings.HasPrefix(approvalID, "live")
}

// ProvideSecret and CancelSecret record the answer like Answer.
func (s *fakeSessions) ProvideSecret(id, requestID, value string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, fmt.Sprintf("%s: secret %s %q", id, requestID, value))
	return strings.HasPrefix(requestID, "live")
}

func (s *fakeSessions) CancelSecret(id, requestID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, fmt.Sprintf("%s: cancel secret %s", id, requestID))
	return strings.HasPrefix(requestID, "live")
}

func (s *fakeSessions) got() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.sent...)
}

// testBot returns a Bot on f with short timings. Sleeps between retries
// are recorded in *sleeps and take no time.
func testBot(t *testing.T, f *fakeAPI, allowed ...int64) (*Bot, *syncBuffer, *sleepLog) {
	log := &syncBuffer{}
	b := New(Options{Token: testToken, AllowedUsers: allowed, APIURL: f.srv.URL, Log: log})
	b.pollTimeout = 30 * time.Second // cancellation, not the timeout, must end polls
	b.draftInterval = 0
	b.closeTimeout = 5 * time.Second
	sl := &sleepLog{}
	b.api.sleep = sl.sleep
	t.Cleanup(b.Close)
	return b, log, sl
}

type sleepLog struct {
	mu sync.Mutex
	d  []time.Duration
}

func (s *sleepLog) sleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	s.d = append(s.d, d)
	s.mu.Unlock()
	return ctx.Err()
}

func (s *sleepLog) got() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.d...)
}

// runner is a Bot running in the background until the test ends.
type runner struct {
	t      *testing.T
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

func start(t *testing.T, b *Bot, ss sender) *runner {
	ctx, cancel := context.WithCancel(context.Background())
	r := &runner{t: t, cancel: cancel, done: make(chan struct{})}
	go func() {
		r.err = b.run(ctx, ss)
		close(r.done)
	}()
	t.Cleanup(func() { r.stop() })
	return r
}

// stop cancels Run and returns its result.
func (r *runner) stop() error {
	r.cancel()
	return r.wait()
}

// wait returns Run's result once it returns.
func (r *runner) wait() error {
	r.t.Helper()
	select {
	case <-r.done:
		return r.err
	case <-time.After(5 * time.Second):
		r.t.Fatal("Run didn't return")
		return nil
	}
}
