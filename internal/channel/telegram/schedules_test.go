package telegram

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
)

// fakeSchedules is a list of schedules /schedules can cancel.
type fakeSchedules struct {
	mu      sync.Mutex
	entries []ScheduleEntry
}

func (s *fakeSchedules) Overview() []ScheduleEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.entries)
}

func (s *fakeSchedules) CancelSchedule(_ context.Context, id string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.entries, func(e ScheduleEntry) bool { return e.ID == id })
	if i < 0 {
		return "", errors.New("no such schedule")
	}
	name := s.entries[i].Name
	s.entries = slices.Delete(s.entries, i, i+1)
	return name, nil
}

func TestSchedulesCommandListsAndCancels(t *testing.T) {
	f := newFakeAPI(t)
	b, _, _ := testBot(t, f, 42)
	b.SetSchedules(&fakeSchedules{entries: []ScheduleEntry{
		{ID: "a1", Name: "Digest", Text: "Digest — every 1h", From: "telegram:42/7"},
		{ID: "b2", Name: "Water", Text: "Water — every 2h", From: "telegram:42"},
	}})
	ss := &fakeSessions{}
	start(t, b, ss)
	f.add(textUpdate(1, 42, 42, "/schedules"))
	eventually(t, "list sent", func() bool { return len(f.callsOf("sendMessage")) == 1 })
	list := f.callsOf("sendMessage")[0].Params
	if text := list["text"].(string); !strings.Contains(text, "Digest — every 1h") || !strings.Contains(text, "Water — every 2h") {
		t.Errorf("list = %q", text)
	}
	if len(ss.got()) != 0 {
		t.Errorf("/schedules reached the model: %q", ss.got())
	}

	f.add(pressUpdate(2, 42, 42, 1, cancelSchedulePrefix+"a1"))
	eventually(t, "list updated", func() bool { return len(f.callsOf("editMessageText")) == 1 })
	edit := f.callsOf("editMessageText")[0].Params
	if text := edit["text"].(string); strings.Contains(text, "Digest") || !strings.Contains(text, "Water") {
		t.Errorf("updated list = %q", text)
	}
	if got := f.callsOf("answerCallbackQuery")[0].Params["text"]; got != "Cancelled Digest." {
		t.Errorf("answer = %v", got)
	}
}

func TestSchedulesShowsOnlyThisChats(t *testing.T) {
	f := newFakeAPI(t)
	b, _, _ := testBot(t, f, 42)
	b.SetSchedules(&fakeSchedules{entries: []ScheduleEntry{{ID: "a1", Name: "Digest", Text: "Digest — every 1h", From: "telegram:42"}}})
	start(t, b, &fakeSessions{})
	f.add(textUpdate(1, 42, -100, "/schedules")) // a group
	eventually(t, "list sent", func() bool { return len(f.callsOf("sendMessage")) == 1 })
	if text := f.callsOf("sendMessage")[0].Params["text"].(string); strings.Contains(text, "Digest") {
		t.Errorf("group sees a private schedule: %q", text)
	}
}

func TestCreatePlaceMakesATopic(t *testing.T) {
	f := newFakeAPI(t)
	b, _, _ := testBot(t, f, 42)
	f.respond("createForumTopic", 200, `{"ok":true,"result":{"message_thread_id":77,"name":"Digest"}}`)
	id, err := b.CreatePlace(context.Background(), "telegram:42/7", "Digest")
	if err != nil || id != "telegram:42/77" {
		t.Fatalf("CreatePlace = %q, %v", id, err)
	}
	if p := f.callsOf("createForumTopic")[0].Params; num(p["chat_id"]) != 42 || p["name"] != "Digest" {
		t.Errorf("params = %v", p)
	}
}

func TestCreatePlaceFailsWithoutTopics(t *testing.T) {
	f := newFakeAPI(t)
	b, _, _ := testBot(t, f, 42)
	f.respond("createForumTopic", 400, `{"ok":false,"error_code":400,"description":"Bad Request: the chat is not a forum"}`)
	if _, err := b.CreatePlace(context.Background(), "telegram:42", "Digest"); err == nil {
		t.Fatal("no error")
	}
}

func TestEnsurePlaceRemakesADeletedTopic(t *testing.T) {
	f := newFakeAPI(t)
	b, _, _ := testBot(t, f, 42)
	if id, err := b.EnsurePlace(context.Background(), "telegram:42/77", "Digest"); err != nil || id != "telegram:42/77" {
		t.Fatalf("existing topic: %q, %v", id, err)
	}
	if p := f.callsOf("editForumTopic")[0].Params; num(p["message_thread_id"]) != 77 || p["name"] != "Digest" {
		t.Errorf("probe = %v", p)
	}
	f.respond("editForumTopic", 400, `{"ok":false,"error_code":400,"description":"Bad Request: TOPIC_ID_INVALID"}`)
	f.respond("createForumTopic", 200, `{"ok":true,"result":{"message_thread_id":78,"name":"Digest"}}`)
	if id, err := b.EnsurePlace(context.Background(), "telegram:42/77", "Digest"); err != nil || id != "telegram:42/78" {
		t.Fatalf("deleted topic: %q, %v", id, err)
	}
}

func TestDeletePlaceDeletesTheTopic(t *testing.T) {
	f := newFakeAPI(t)
	b, _, _ := testBot(t, f, 42)
	if err := b.DeletePlace(context.Background(), "telegram:42/77"); err != nil {
		t.Fatal(err)
	}
	if p := f.callsOf("deleteForumTopic")[0].Params; num(p["chat_id"]) != 42 || num(p["message_thread_id"]) != 77 {
		t.Errorf("params = %v", p)
	}
	if err := b.DeletePlace(context.Background(), "telegram:42"); err == nil {
		t.Error("deleted a whole chat")
	}
}

func TestCommandMenuIsSetAtStart(t *testing.T) {
	f := newFakeAPI(t)
	b, _, _ := testBot(t, f, 42)
	b.SetSchedules(&fakeSchedules{})
	start(t, b, &fakeSessions{})
	eventually(t, "menu set", func() bool { return len(f.callsOf("setMyCommands")) == 1 })
	var names []string
	for _, c := range f.callsOf("setMyCommands")[0].Params["commands"].([]any) {
		names = append(names, c.(map[string]any)["command"].(string))
	}
	if want := []string{"stop", "reset", "new", "schedules"}; !slices.Equal(names, want) {
		t.Errorf("menu = %v; want %v", names, want)
	}
}
