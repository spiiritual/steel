package telegram

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spiiritual/steel/internal/agent"
)

// inTopic puts an update's message in topic. topic marks a forum topic;
// without it, message_thread_id names a reply thread, which isn't one.
func inTopic(u map[string]any, thread int64, topic bool) map[string]any {
	m, ok := u["message"].(map[string]any)
	if !ok {
		m = u["callback_query"].(map[string]any)["message"].(map[string]any)
	}
	m["message_thread_id"] = thread
	m["is_topic_message"] = topic
	return u
}

func TestEachTopicIsItsOwnConversation(t *testing.T) {
	f := newFakeAPI(t)
	b, _, _ := testBot(t, f, 42)
	ss := &fakeSessions{}
	start(t, b, ss)
	f.add(
		inTopic(textUpdate(1, 42, 42, "in topic 7"), 7, true),
		textUpdate(2, 42, 42, "outside topics"),
		inTopic(textUpdate(3, 42, 42, "a reply thread"), 5, false),
		inTopic(textUpdate(4, 42, 42, "/stop"), 7, true),
	)
	eventually(t, "updates handled", func() bool { return lastOffset(f) == 5 })
	want := []string{"telegram:42/7: in topic 7", "telegram:42: outside topics", "telegram:42: a reply thread", "telegram:42/7: /stop"}
	if !slices.Equal(ss.got(), want) {
		t.Errorf("handed %q, want %q", ss.got(), want)
	}
}

// Everything a topic's session says goes to that topic, and nothing said
// outside topics does.
func TestRepliesGoToTheirTopic(t *testing.T) {
	f := newFakeAPI(t)
	b, _, _ := testBot(t, f, 42, -100)
	b.Emit("telegram:42/7", agent.TextDelta{Text: "drafting"})
	b.Emit("telegram:42/7", agent.Reply{Text: "in the topic"})
	b.Emit("telegram:42", agent.Reply{Text: "outside"})
	b.Emit("telegram:-100/9", agent.TextDelta{Text: "group topic"}) // groups show typing
	b.Close()

	threads := map[string]any{} // absent for no thread
	for _, c := range f.callsOf("sendMessage") {
		threads[c.Params["text"].(string)] = c.Params["message_thread_id"]
	}
	if num(threads["in the topic"]) != 7 || threads["outside"] != nil {
		t.Errorf("message threads %v", threads)
	}
	for _, c := range f.callsOf("sendMessageDraft") {
		if num(c.Params["message_thread_id"]) != 7 {
			t.Errorf("draft outside its topic: %v", c.Params)
		}
	}
	typing := f.callsOf("sendChatAction")
	if len(typing) == 0 || num(typing[0].Params["message_thread_id"]) != 9 {
		t.Errorf("typing %v", typing)
	}
}

// A stop button press names only the chat and draft; it stops the topic
// whose draft it was.
func TestStopButtonStopsItsTopic(t *testing.T) {
	f := newFakeAPI(t)
	b, _, _ := testBot(t, f, 42)
	ss := &fakeSessions{}
	start(t, b, ss)
	eventually(t, "polling", b.polling.Load)
	b.Emit("telegram:42", agent.TextDelta{Text: "main"})
	b.Emit("telegram:42/7", agent.TextDelta{Text: "topic"})
	eventually(t, "drafts", func() bool { return len(f.callsOf("sendMessageDraft")) >= 2 })
	var draft int64
	for _, c := range f.callsOf("sendMessageDraft") {
		if num(c.Params["message_thread_id"]) == 7 {
			draft = num(c.Params["draft_id"])
		}
	}

	f.add(
		stopUpdate(1, 42, draft),
		stopUpdate(2, 42, draftID(9, 5)), // a draft from before a restart: no renderer knows it
		stopUpdate(3, 42, 999),
	)
	eventually(t, "stops handled", func() bool { return lastOffset(f) == 4 })
	if want := []string{"telegram:42/7: /stop", "telegram:42/9: /stop", "telegram:42: /stop"}; !slices.Equal(ss.got(), want) {
		t.Errorf("handed %q, want %q", ss.got(), want)
	}
}

func TestApprovalInTopicAnswersItsSession(t *testing.T) {
	f := newFakeAPI(t)
	b, _, _ := testBot(t, f, 42)
	ss := &fakeSessions{}
	start(t, b, ss)
	f.add(inTopic(pressUpdate(1, 42, 42, 1, "allow:live1"), 7, true))
	eventually(t, "press handled", func() bool { return lastOffset(f) == 2 })
	if want := []string{"telegram:42/7: answer live1 true"}; !slices.Equal(ss.got(), want) {
		t.Errorf("answers %q, want %q", ss.got(), want)
	}
}

func TestParsePlace(t *testing.T) {
	for _, p := range []place{{42, 0}, {42, 7}, {-100123, 9}} {
		if got, ok := parsePlace(p.sessionID()); !ok || got != p {
			t.Errorf("%s: got %v, %v", p.sessionID(), got, ok)
		}
	}
	for _, id := range []string{"telegram:", "telegram:x", "telegram:42/", "telegram:42/0", "telegram:42/x", "other:42"} {
		if p, ok := parsePlace(id); ok {
			t.Errorf("%q parsed as %v", id, p)
		}
	}
}

// Creating a topic sends a service message without text; it isn't the
// user writing, so it gets no "text only" notice.
func TestTopicServiceMessagesAreIgnored(t *testing.T) {
	f := newFakeAPI(t)
	b, _, _ := testBot(t, f, 42)
	ss := &fakeSessions{}
	start(t, b, ss)
	created := inTopic(textUpdate(1, 42, 42, ""), 7, true)
	created["message"].(map[string]any)["forum_topic_created"] = map[string]any{"name": "ideas", "icon_color": 1}
	f.add(created, textUpdate(2, 42, 42, "hi"))
	eventually(t, "updates handled", func() bool { return lastOffset(f) == 3 })
	b.Close()
	if got := f.texts("sendMessage"); len(got) != 0 {
		t.Errorf("sent %q", got)
	}
	if want := []string{"telegram:42: hi"}; !slices.Equal(ss.got(), want) {
		t.Errorf("handed %q", ss.got())
	}
}

// A topic's renderer goes away once idle and comes back for the next event.
func TestIdleRendererIsRemoved(t *testing.T) {
	f := newFakeAPI(t)
	b, _, _ := testBot(t, f, 42)
	b.chatIdle = 10 * time.Millisecond
	renderers := func() int { b.mu.Lock(); defer b.mu.Unlock(); return len(b.chats) }
	b.Emit("telegram:42/7", agent.Notice{Text: "one"})
	eventually(t, "renderer removed", func() bool { return renderers() == 0 && len(f.texts("sendMessage")) == 1 })
	b.Emit("telegram:42/7", agent.Notice{Text: "two"})
	eventually(t, "second message", func() bool { return len(f.texts("sendMessage")) == 2 })
	if c := f.callsOf("sendMessage")[1]; num(c.Params["message_thread_id"]) != 7 {
		t.Errorf("second message %v", c.Params)
	}
}

func TestOwnTopicMessagesAreIgnoredQuietly(t *testing.T) {
	f := newFakeAPI(t)
	b, log, _ := testBot(t, f, 42)
	start(t, b, &fakeSessions{})
	u := inTopic(textUpdate(1, 9, 42, ""), 77, true)
	m := u["message"].(map[string]any)
	m["from"].(map[string]any)["is_bot"] = true
	m["forum_topic_created"] = map[string]any{"name": "Digest"}
	f.add(u)
	eventually(t, "update handled", func() bool { return lastOffset(f) == 2 })
	if strings.Contains(log.String(), "ignoring user") {
		t.Errorf("log = %q", log.String())
	}
}
