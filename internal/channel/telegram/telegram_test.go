package telegram

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spiiritual/steel/internal/agent"
	"github.com/spiiritual/steel/internal/llm"
)

func lastOffset(f *fakeAPI) int64 {
	calls := f.callsOf("getUpdates")
	if len(calls) == 0 {
		return -1
	}
	return num(calls[len(calls)-1].Params["offset"])
}

func TestPollingHandsUpdatesInOrderAndConfirmsThem(t *testing.T) {
	f := newFakeAPI(t)
	b, _, _ := testBot(t, f, 42)
	ss := &fakeSessions{}
	f.add(textUpdate(5, 42, 42, "one"), textUpdate(6, 42, 42, "two"))
	start(t, b, ss)

	eventually(t, "updates 5 and 6 confirmed", func() bool { return lastOffset(f) == 7 })
	f.add(textUpdate(7, 42, 42, "three"))
	eventually(t, "update 7 confirmed", func() bool { return lastOffset(f) == 8 })
	if want := []string{"telegram:42: one", "telegram:42: two", "telegram:42: three"}; !slices.Equal(ss.got(), want) {
		t.Errorf("handed %q, want %q", ss.got(), want)
	}
	if p := f.pending(); len(p) != 0 {
		t.Errorf("unconfirmed updates %v", p)
	}
	first := f.callsOf("getUpdates")[0].Params
	if _, ok := first["offset"]; ok {
		t.Errorf("first poll has offset %v; want none, to resume at the first unconfirmed update", first["offset"])
	}
	if num(first["timeout"]) != 30 {
		t.Errorf("poll timeout %v, want 30", first["timeout"])
	}
	if got := fmt.Sprint(first["allowed_updates"]); got != "[message stopped_message_generation]" {
		t.Errorf("allowed_updates %s", got)
	}
}

func TestUnreadableUpdateIsSkipped(t *testing.T) {
	f := newFakeAPI(t)
	b, log, _ := testBot(t, f, 42)
	ss := &fakeSessions{}
	bad := textUpdate(5, 42, 42, "x")
	bad["message"] = "not an object" // a shape steel can't decode
	f.add(bad, textUpdate(6, 42, 42, "after"))
	start(t, b, ss)
	eventually(t, "both confirmed", func() bool { return lastOffset(f) == 7 })
	if want := []string{"telegram:42: after"}; !slices.Equal(ss.got(), want) || !strings.Contains(log.String(), "skipping update 5") {
		t.Errorf("handed %q, log %q", ss.got(), log.String())
	}
}

func TestCancelLeavesUnhandedUpdatesUnconfirmed(t *testing.T) {
	f := newFakeAPI(t)
	b, _, _ := testBot(t, f, 42)
	f.add(textUpdate(10, 42, 42, "a"), textUpdate(11, 42, 42, "b"), textUpdate(12, 42, 42, "c"))
	ctx, cancel := context.WithCancel(context.Background())
	ss := &fakeSessions{hook: func(_, text string) {
		if text == "a" {
			cancel() // shutdown begins while the batch is being handed over
		}
	}}
	if err := b.run(ctx, ss); err != nil {
		t.Fatal(err)
	}
	if want := []string{"telegram:42: a"}; !slices.Equal(ss.got(), want) {
		t.Fatalf("handed %q, want %q", ss.got(), want)
	}
	if got := f.pending(); !slices.Equal(got, []int64{11, 12}) {
		t.Fatalf("unconfirmed updates %v, want [11 12]", got)
	}

	// After a restart they arrive.
	b2, _, _ := testBot(t, f, 42)
	ss2 := &fakeSessions{}
	start(t, b2, ss2)
	eventually(t, "redelivery", func() bool { return len(ss2.got()) == 2 })
	if want := []string{"telegram:42: b", "telegram:42: c"}; !slices.Equal(ss2.got(), want) {
		t.Errorf("redelivered %q, want %q", ss2.got(), want)
	}
}

func TestCancelEndsLongPollPromptly(t *testing.T) {
	f := newFakeAPI(t)
	b, _, _ := testBot(t, f, 42)
	r := start(t, b, &fakeSessions{})
	eventually(t, "a poll", func() bool { return len(f.callsOf("getUpdates")) == 1 })
	begin := time.Now()
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(begin); d > 2*time.Second {
		t.Errorf("Run took %s to return; the poll timeout is %s", d, b.pollTimeout)
	}
	if n := len(f.callsOf("getUpdates")); n != 1 {
		t.Errorf("%d getUpdates calls; nothing was received, so nothing needs confirming", n)
	}
}

func TestEmptyAllowlistIgnoresEveryone(t *testing.T) {
	f := newFakeAPI(t)
	b, log, _ := testBot(t, f)
	ss := &fakeSessions{}
	f.add(textUpdate(1, 7, 7, "hi"), textUpdate(2, 7, 7, "/start"), textUpdate(3, 8, -100, "hello"))
	start(t, b, ss)
	eventually(t, "updates confirmed", func() bool { return lastOffset(f) == 4 })
	b.Close() // delivers anything queued, so the checks below see it

	if len(ss.got()) != 0 {
		t.Errorf("handed %q", ss.got())
	}
	if n := len(f.callsOf("sendMessage")) + len(f.callsOf("sendMessageDraft")); n != 0 {
		t.Errorf("%d replies to strangers", n)
	}
	out := log.String()
	if !strings.Contains(out, "no allowed users") {
		t.Errorf("no warning about the empty allowlist in log:\n%s", out)
	}
	if strings.Count(out, "ignoring user 7 (@user7)") != 1 || strings.Count(out, "ignoring user 8 ") != 1 {
		t.Errorf("each unknown user should be logged once:\n%s", out)
	}
}

func TestAllowlistIsPerUser(t *testing.T) {
	f := newFakeAPI(t)
	b, log, _ := testBot(t, f, 42)
	ss := &fakeSessions{}
	f.add(textUpdate(1, 99, -100, "from a stranger"), textUpdate(2, 42, -100, "from the owner"))
	start(t, b, ss)
	eventually(t, "updates confirmed", func() bool { return lastOffset(f) == 3 })
	if want := []string{"telegram:-100: from the owner"}; !slices.Equal(ss.got(), want) {
		t.Errorf("handed %q, want %q", ss.got(), want)
	}
	if !strings.Contains(log.String(), "ignoring user 99") {
		t.Errorf("stranger not logged:\n%s", log)
	}
}

func TestCommand(t *testing.T) {
	for _, tc := range []struct {
		text, cmd, rest string
		mine            bool
	}{
		{"hello there", "", "hello there", true},
		{"  /stop  ", "/stop", "", true},
		{"/stop@steel_bot", "/stop", "", true},
		{"/Stop@Steel_Bot", "/stop", "", true},
		{"/new@other_bot", "", "", false},
		{"/new let's talk\nabout this", "/new", "let's talk\nabout this", true},
		{"/new\nhello", "/new", "hello", true},
		{"/help@steel_bot me", "/help", "me", true},
		// Paths aren't commands: kept whole, case intact.
		{"/Users/Andrew/notes.txt summarize", "", "/Users/Andrew/notes.txt summarize", true},
		{"/srv/repo@v2 check this", "", "/srv/repo@v2 check this", true},
	} {
		cmd, rest, mine := command(tc.text, "steel_bot")
		if cmd != tc.cmd || rest != tc.rest || mine != tc.mine {
			t.Errorf("command(%q) = %q, %q, %v; want %q, %q, %v", tc.text, cmd, rest, mine, tc.cmd, tc.rest, tc.mine)
		}
	}
}

func TestCommandsReachTheSession(t *testing.T) {
	f := newFakeAPI(t)
	b, _, _ := testBot(t, f, 42)
	ss := &fakeSessions{}
	f.add(
		textUpdate(1, 42, 42, "/stop@steel_bot"),
		textUpdate(2, 42, -100, "/new@other_bot"),
		textUpdate(3, 42, 42, "/RESET"),
		textUpdate(4, 42, 42, "/new hello there"),
		textUpdate(5, 42, 42, "/start"),
		textUpdate(6, 42, 42, "/help@steel_bot me"),
	)
	start(t, b, ss)
	eventually(t, "all updates handled", func() bool { return lastOffset(f) == 7 })
	eventually(t, "greeting", func() bool { return len(f.callsOf("sendMessage")) == 1 })
	want := []string{
		"telegram:42: /stop",
		"telegram:42: /reset",
		"telegram:42: /new",
		"telegram:42: hello there",
		"telegram:42: /help me",
	}
	if !slices.Equal(ss.got(), want) {
		t.Errorf("handed %q, want %q", ss.got(), want)
	}
	c := f.callsOf("sendMessage")[0].Params
	if num(c["chat_id"]) != 42 || !strings.Contains(c["text"].(string), "/stop") {
		t.Errorf("greeting %v", c)
	}
	if _, ok := c["parse_mode"]; ok {
		t.Error("messages should be plain text")
	}
}

func TestStreamingDraftsThenMessages(t *testing.T) {
	f := newFakeAPI(t)
	b, _, _ := testBot(t, f, 42)
	start(t, b, &fakeSessions{})
	eventually(t, "polling", b.polling.Load)
	const id = "telegram:42"
	hasDraft := func(text string) func() bool {
		return func() bool { return slices.Contains(f.texts("sendMessageDraft"), text) }
	}

	b.Emit(id, agent.TextDelta{Text: "Let me "})
	eventually(t, "first draft", hasDraft("Let me "))
	b.Emit(id, agent.TextDelta{Text: "look."})
	eventually(t, "grown draft", hasDraft("Let me look."))
	b.Emit(id, agent.Reply{Text: "Let me look."})
	eventually(t, "reply", func() bool { return slices.Equal(f.texts("sendMessage"), []string{"Let me look."}) })
	b.Emit(id, agent.ToolStart{Name: "run_code"})
	eventually(t, "tool draft", hasDraft("⚙ run_code…"))
	b.Emit(id, agent.ToolStart{Name: "fs.read_file", Nested: true})
	eventually(t, "nested tool draft", hasDraft("⚙ fs.read_file…"))
	b.Emit(id, agent.ToolEnd{Name: "fs.read_file", Nested: true, Result: strings.Repeat("x", 10000)})
	b.Emit(id, agent.ToolEnd{Name: "run_code"})
	b.Emit(id, agent.TextDelta{Text: "Found it."})
	eventually(t, "answer draft", hasDraft("Found it."))
	b.Emit(id, agent.Reply{Text: "Found it."})
	b.Emit(id, agent.TurnDone{})
	b.Emit(id, agent.Notice{Text: "sync"})
	eventually(t, "messages", func() bool { return len(f.texts("sendMessage")) == 3 })
	if want := []string{"Let me look.", "Found it.", "sync"}; !slices.Equal(f.texts("sendMessage"), want) {
		t.Errorf("messages %q, want %q", f.texts("sendMessage"), want)
	}

	drafts := f.callsOf("sendMessageDraft")
	for _, d := range drafts {
		if strings.Contains(d.Params["text"].(string), "xxx") {
			t.Errorf("draft shows a tool result: %.40q", d.Params["text"])
		}
		if num(d.Params["chat_id"]) != 42 || num(d.Params["draft_id"]) == 0 || d.Params["can_stop"] != true || d.Params["keep_on_stop"] != true {
			t.Errorf("draft params %v", d.Params)
		}
	}
	// A sent message ends a draft; the next one gets a new ID.
	idOf := func(text string) int64 {
		for _, d := range drafts {
			if d.Params["text"] == text {
				return num(d.Params["draft_id"])
			}
		}
		return 0
	}
	if idOf("Let me ") != idOf("Let me look.") || idOf("Let me look.") == idOf("⚙ run_code…") || idOf("⚙ run_code…") != idOf("Found it.") {
		t.Errorf("draft IDs: %v", drafts)
	}
}

func TestDraftsAreThrottled(t *testing.T) {
	f := newFakeAPI(t)
	b, _, _ := testBot(t, f, 42)
	b.draftInterval = time.Hour
	b.Emit("telegram:42", agent.TextDelta{Text: "a"})
	eventually(t, "first draft", func() bool { return len(f.callsOf("sendMessageDraft")) == 1 })
	for _, s := range []string{"b", "c"} {
		b.Emit("telegram:42", agent.TextDelta{Text: s})
	}
	time.Sleep(50 * time.Millisecond)
	b.Emit("telegram:42", agent.Reply{Text: "abc"})
	eventually(t, "reply", func() bool { return len(f.callsOf("sendMessage")) == 1 })
	if n := len(f.callsOf("sendMessageDraft")); n != 1 {
		t.Errorf("%d drafts within the interval, want 1", n)
	}
}

func TestNoDraftAfterFinalReply(t *testing.T) {
	f := newFakeAPI(t)
	b, _, _ := testBot(t, f, 42)
	start(t, b, &fakeSessions{})
	eventually(t, "polling", b.polling.Load)
	b.Emit("telegram:42", agent.TextDelta{Text: "Hel"})
	b.Emit("telegram:42", agent.Reply{Text: "Hello"})
	eventually(t, "reply", func() bool { return len(f.callsOf("sendMessage")) == 1 })
	time.Sleep(100 * time.Millisecond) // TurnDone usually lags the reply
	b.Emit("telegram:42", agent.TurnDone{})
	time.Sleep(50 * time.Millisecond)
	if slices.Contains(f.texts("sendMessageDraft"), "") {
		t.Errorf("empty \"Thinking…\" draft after the reply: %q", f.texts("sendMessageDraft"))
	}
}

func TestBackgroundToolCallsDontKeepIdleChatBusy(t *testing.T) {
	f := newFakeAPI(t)
	b, _, _ := testBot(t, f, 42)
	b.draftRefresh = time.Millisecond
	start(t, b, &fakeSessions{})
	eventually(t, "polling", b.polling.Load)
	// A background job's nested call after its turn ended, then /reset drops
	// the job: nothing should keep showing activity.
	b.Emit("telegram:42", agent.ToolStart{Name: "fs.read_file", Nested: true})
	b.Emit("telegram:42", agent.Notice{Text: "Scratchpad cleared."})
	eventually(t, "notice", func() bool { return len(f.callsOf("sendMessage")) == 1 })
	time.Sleep(50 * time.Millisecond)
	if n := len(f.callsOf("sendMessageDraft")); n != 0 {
		t.Errorf("%d drafts for an idle chat: %q", n, f.texts("sendMessageDraft"))
	}
}

func TestBadTokenFailsWithoutLeakingIt(t *testing.T) {
	f := newFakeAPI(t)
	log := &syncBuffer{}
	b := New(Options{Token: "123:secret\r", AllowedUsers: []int64{42}, APIURL: f.srv.URL, Log: log})
	t.Cleanup(b.Close)
	err := start(t, b, &fakeSessions{}).wait()
	if err == nil || strings.Contains(err.Error()+log.String(), "secret") {
		t.Errorf("err = %v, log = %q", err, log.String())
	}
}

func TestWebhookConflictFails(t *testing.T) {
	f := newFakeAPI(t)
	b, _, _ := testBot(t, f, 42)
	f.respond("getUpdates", 409, `{"ok":false,"error_code":409,"description":"Conflict: can't use getUpdates method while webhook is active; use deleteWebhook to delete the webhook first"}`)
	if err := start(t, b, &fakeSessions{}).wait(); err == nil || !strings.Contains(err.Error(), "webhook") {
		t.Errorf("Run = %v, want the webhook conflict", err)
	}
}

func TestDraftIsRefreshedBeforeItExpires(t *testing.T) {
	f := newFakeAPI(t)
	b, _, _ := testBot(t, f, 42)
	b.draftRefresh = time.Millisecond
	b.Emit("telegram:42", agent.ToolStart{Name: "run_code"})
	eventually(t, "refreshes", func() bool { return len(f.callsOf("sendMessageDraft")) >= 3 })
	b.Emit("telegram:42", agent.TurnDone{})
	b.Emit("telegram:42", agent.Notice{Text: "sync"})
	eventually(t, "turn end", func() bool { return len(f.callsOf("sendMessage")) == 1 })
	n := len(f.callsOf("sendMessageDraft"))
	time.Sleep(20 * time.Millisecond)
	if m := len(f.callsOf("sendMessageDraft")); m != n {
		t.Errorf("drafts kept coming after the turn ended: %d, then %d", n, m)
	}
}

func TestGroupsShowTyping(t *testing.T) {
	f := newFakeAPI(t)
	b, _, _ := testBot(t, f, 42)
	b.Emit("telegram:-100", agent.ToolStart{Name: "run_code"})
	eventually(t, "typing", func() bool { return len(f.callsOf("sendChatAction")) > 0 })
	c := f.callsOf("sendChatAction")[0].Params
	if num(c["chat_id"]) != -100 || c["action"] != "typing" {
		t.Errorf("chat action %v", c)
	}
	b.Emit("telegram:-100", agent.Reply{Text: "done"})
	eventually(t, "reply", func() bool { return len(f.callsOf("sendMessage")) == 1 })
	if n := len(f.callsOf("sendMessageDraft")); n != 0 {
		t.Errorf("%d drafts in a group", n)
	}
}

func TestRejectedDraftsFallBackToTyping(t *testing.T) {
	f := newFakeAPI(t)
	b, log, _ := testBot(t, f, 42)
	f.respond("sendMessageDraft", 400, `{"ok":false,"error_code":400,"description":"Bad Request: drafts are unavailable"}`)
	b.Emit("telegram:42", agent.TextDelta{Text: "hi"})
	eventually(t, "typing", func() bool { return len(f.callsOf("sendChatAction")) > 0 })
	if !strings.Contains(log.String(), "drafts unavailable") {
		t.Errorf("log:\n%s", log)
	}
}

func TestStopButtonStopsTheSession(t *testing.T) {
	f := newFakeAPI(t)
	b, _, _ := testBot(t, f, 42)
	const id = "telegram:42"
	ss := &fakeSessions{hook: func(sid, text string) {
		if text == "/stop" {
			b.Emit(sid, agent.Notice{Text: "Stopped."}) // as Session.Send does
		}
	}}
	start(t, b, ss)
	eventually(t, "polling", b.polling.Load)
	b.Emit(id, agent.TextDelta{Text: "partial answer"})
	eventually(t, "draft", func() bool { return len(f.callsOf("sendMessageDraft")) > 0 })
	draftID := num(f.callsOf("sendMessageDraft")[0].Params["draft_id"])

	f.add(stopUpdate(1, 7, 1), stopUpdate(2, 42, draftID)) // chat 7 isn't allowed
	eventually(t, "stop handled", func() bool { return lastOffset(f) == 3 })
	if want := []string{id + ": /stop"}; !slices.Equal(ss.got(), want) {
		t.Errorf("handed %q, want %q", ss.got(), want)
	}
	// The rest of the stopped reply is dropped.
	b.Emit(id, agent.TextDelta{Text: " and more"})
	b.Emit(id, agent.TurnDone{})
	b.Emit(id, agent.Notice{Text: "sync"})
	eventually(t, "messages", func() bool { return len(f.texts("sendMessage")) == 3 })
	if want := []string{"partial answer", "Stopped.", "sync"}; !slices.Equal(f.texts("sendMessage"), want) {
		t.Errorf("messages %q, want %q", f.texts("sendMessage"), want)
	}
}

// streamer streams "partial" and then waits to be stopped.
type streamer struct{}

func (streamer) Stream(ctx context.Context, _ llm.Request, onDelta func(string) error) (llm.Message, error) {
	onDelta("partial")
	<-ctx.Done()
	return llm.Message{}, ctx.Err()
}

func TestStopEndToEnd(t *testing.T) {
	f := newFakeAPI(t)
	b, _, _ := testBot(t, f, 42)
	ss, err := agent.NewSessions(agent.Config{Provider: streamer{}, Model: "m"}, b.Emit)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ss.Close)
	start(t, b, ss)
	f.add(textUpdate(1, 42, 42, "hello"))
	eventually(t, "draft", func() bool { return slices.Contains(f.texts("sendMessageDraft"), "partial") })
	f.add(stopUpdate(2, 42, 1))
	eventually(t, "stop", func() bool { return len(f.texts("sendMessage")) == 2 })
	if want := []string{"partial", "Stopped."}; !slices.Equal(f.texts("sendMessage"), want) {
		t.Errorf("messages %q, want %q", f.texts("sendMessage"), want)
	}
	eventually(t, "idle session", func() bool { return ss.Idle("telegram:42") })
}

func TestTurnErrorsAndNoticesBecomeMessages(t *testing.T) {
	f := newFakeAPI(t)
	b, _, _ := testBot(t, f, 42)
	b.Emit("terminal", agent.Notice{Text: "not ours"})
	b.Emit("telegram:42", agent.Notice{Text: "Scratchpad cleared."})
	b.Emit("telegram:42", agent.TextDelta{Text: "cut off"})
	b.Emit("telegram:42", agent.TurnDone{Err: errors.New("model unavailable\nlong details")})
	eventually(t, "messages", func() bool { return len(f.callsOf("sendMessage")) == 3 })
	want := []string{"Scratchpad cleared.", "cut off", "Error: model unavailable …"}
	if !slices.Equal(f.texts("sendMessage"), want) {
		t.Errorf("messages %q, want %q", f.texts("sendMessage"), want)
	}
}

func TestLongRepliesAreSplit(t *testing.T) {
	f := newFakeAPI(t)
	b, _, _ := testBot(t, f, 42)
	reply := strings.Repeat("word ", 1000) // 5000 characters
	b.Emit("telegram:42", agent.Reply{Text: reply})
	eventually(t, "messages", func() bool { return len(f.callsOf("sendMessage")) == 2 })
	parts := f.texts("sendMessage")
	if utf16Len(parts[0]) > maxText || strings.Join(parts, " ") != reply {
		t.Errorf("parts of %d and %d characters", utf16Len(parts[0]), utf16Len(parts[1]))
	}
}

func TestFloodControlWaits(t *testing.T) {
	f := newFakeAPI(t)
	b, _, sleeps := testBot(t, f, 42)
	f.respond("sendMessage", 429, `{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 3","parameters":{"retry_after":3}}`)
	b.Emit("telegram:42", agent.Notice{Text: "hi"})
	eventually(t, "retry", func() bool { return len(f.callsOf("sendMessage")) == 2 })
	if got := sleeps.got(); !slices.Equal(got, []time.Duration{3 * time.Second}) {
		t.Errorf("slept %v, want [3s]", got)
	}
}

func TestDraftFloodControlPostponesDrafts(t *testing.T) {
	f := newFakeAPI(t)
	b, _, _ := testBot(t, f, 42)
	f.respond("sendMessageDraft", 429, `{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 60","parameters":{"retry_after":60}}`)
	b.Emit("telegram:42", agent.TextDelta{Text: "a"})
	eventually(t, "draft", func() bool { return len(f.callsOf("sendMessageDraft")) == 1 })
	b.Emit("telegram:42", agent.TextDelta{Text: "b"})
	b.Emit("telegram:42", agent.Reply{Text: "ab"})
	eventually(t, "reply", func() bool { return len(f.callsOf("sendMessage")) == 1 })
	if n := len(f.callsOf("sendMessageDraft")); n != 1 {
		t.Errorf("%d drafts during flood control, want 1", n)
	}
}

func TestPollingBacksOff(t *testing.T) {
	f := newFakeAPI(t)
	b, log, sleeps := testBot(t, f, 42)
	for range 3 {
		f.respond("getUpdates", 502, "<html>Bad Gateway</html>")
	}
	f.add(textUpdate(1, 42, 42, "hi"))
	ss := &fakeSessions{}
	start(t, b, ss)
	eventually(t, "message", func() bool { return len(ss.got()) == 1 })
	if got, want := sleeps.got(), []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}; !slices.Equal(got, want) {
		t.Errorf("slept %v, want %v", got, want)
	}
	if strings.Contains(log.String(), testToken) {
		t.Errorf("log leaks the token:\n%s", log)
	}
}

func TestInvalidTokenFailsRun(t *testing.T) {
	f := newFakeAPI(t)
	log := &syncBuffer{}
	b := New(Options{Token: "wrong", AllowedUsers: []int64{42}, APIURL: f.srv.URL, Log: log})
	defer b.Close()
	err := b.run(context.Background(), &fakeSessions{})
	if err == nil || !strings.Contains(err.Error(), "Unauthorized") {
		t.Fatalf("err = %v, want Unauthorized", err)
	}
}

func TestNetworkErrorsHideToken(t *testing.T) {
	f := newFakeAPI(t)
	url := f.srv.URL
	f.srv.Close()
	c := newClient(url, testToken, nil, func(string, ...any) {})
	err := c.call(context.Background(), "getMe", struct{}{}, nil, time.Second)
	if err == nil || strings.Contains(err.Error(), testToken) {
		t.Errorf("err = %v", err)
	}
}

func TestCloseDeliversQueuedMessages(t *testing.T) {
	f := newFakeAPI(t)
	b, _, _ := testBot(t, f, 42)
	b.Emit("telegram:42", agent.Reply{Text: "bye"})
	b.Close()
	b.Emit("telegram:42", agent.Notice{Text: "too late"})
	if got := f.texts("sendMessage"); !slices.Equal(got, []string{"bye"}) {
		t.Errorf("messages %q", got)
	}
}

func TestSplit(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		limit      int
		want       []string
	}{
		{"fits", "hello", 5, []string{"hello"}},
		{"blank", " \n ", 5, nil},
		{"paragraph", "aaaa bb\n\ncccc", 10, []string{"aaaa bb", "cccc"}},
		{"line", "aaaa bb\ncccc dd", 10, []string{"aaaa bb", "cccc dd"}},
		{"space", "aaaa bbbb cccc", 10, []string{"aaaa bbbb", "cccc"}},
		{"hard", "abcdefghijkl", 5, []string{"abcde", "fghij", "kl"}},
		{"early break ignored", "a\nbcdefghijkl", 6, []string{"a\nbcde", "fghijk", "l"}},
		// "😀" is two UTF-16 code units; "é" is one.
		{"utf16", "😀😀😀é", 4, []string{"😀😀", "😀é"}},
		{"no split surrogate", "a😀😀", 2, []string{"a", "😀", "😀"}},
	} {
		got := split(tc.text, tc.limit)
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: split(%q, %d) = %q, want %q", tc.name, tc.text, tc.limit, got, tc.want)
		}
		for _, p := range got {
			if utf16Len(p) > tc.limit {
				t.Errorf("%s: part %q over the limit", tc.name, p)
			}
		}
	}

	long := strings.Repeat("😀", 3000) // 6000 code units
	parts := split(long, maxText)
	if len(parts) != 2 || utf16Len(parts[0]) != maxText || parts[0]+parts[1] != long {
		t.Errorf("emoji split into %d parts", len(parts))
	}
}

func TestTail(t *testing.T) {
	if got := tail("abcdef", 4); got != "…def" {
		t.Errorf("tail = %q", got)
	}
	if got := tail("ab😀c", 3); got != "…c" {
		t.Errorf("tail with a surrogate pair at the cut = %q", got)
	}
	if got := tail("abc", 3); got != "abc" {
		t.Errorf("tail = %q", got)
	}
}
