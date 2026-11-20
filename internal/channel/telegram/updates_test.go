package telegram

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// fakeUpdates reports a fixed status and records what was started.
type fakeUpdates struct {
	status UpdateStatus
	mu     sync.Mutex
	starts []string // "action from"
}

func (u *fakeUpdates) Status(context.Context) (UpdateStatus, error) { return u.status, nil }

func (u *fakeUpdates) Update(_ context.Context, from string) (string, error) {
	return u.record("update", from), nil
}

func (u *fakeUpdates) Rollback(_ context.Context, from string) (string, error) {
	return u.record("rollback", from), nil
}

func (u *fakeUpdates) record(action, from string) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.starts = append(u.starts, action+" "+from)
	return "Started " + action + "."
}

func (u *fakeUpdates) got() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.starts...)
}

// buttonsOf returns the callback data and URLs of a sent message's buttons.
func buttonsOf(params map[string]any) []string {
	var out []string
	kb, _ := params["reply_markup"].(map[string]any)
	rows, _ := kb["inline_keyboard"].([]any)
	for _, row := range rows {
		for _, b := range row.([]any) {
			b := b.(map[string]any)
			if d, ok := b["callback_data"].(string); ok {
				out = append(out, d)
			}
			if u, ok := b["url"].(string); ok {
				out = append(out, u)
			}
		}
	}
	return out
}

// /update asks before updating, since it restarts steel; the button
// starts it, and the offer becomes what's happening.
func TestUpdateAsksThenStarts(t *testing.T) {
	f := newFakeAPI(t)
	b, _, _ := testBot(t, f, 42)
	u := &fakeUpdates{status: UpdateStatus{Current: "v1.0.0", Latest: "v1.1.0", Available: true}}
	b.SetUpdates(u)
	ss := &fakeSessions{}
	start(t, b, ss)

	f.add(textUpdate(1, 42, 42, "/update"))
	eventually(t, "offer sent", func() bool { return len(f.callsOf("sendMessage")) == 1 })
	offer := f.callsOf("sendMessage")[0].Params
	if text := offer["text"].(string); !strings.Contains(text, "v1.1.0 is out") {
		t.Errorf("offer = %q", text)
	}
	if got := buttonsOf(offer); len(got) != 2 || got[0] != updatePrefix+"v1.1.0" || got[1] != releaseURL+"v1.1.0" {
		t.Errorf("buttons = %q", got)
	}
	if len(u.got()) != 0 || len(ss.got()) != 0 {
		t.Fatalf("/update acted before the button: %q, model got %q", u.got(), ss.got())
	}

	f.add(pressUpdate(2, 42, 42, 1, updatePrefix+"v1.1.0"))
	eventually(t, "offer replaced", func() bool { return len(f.callsOf("editMessageText")) == 1 })
	if got := u.got(); len(got) != 1 || got[0] != "update telegram:42" {
		t.Errorf("started %q", got)
	}
	edit := f.callsOf("editMessageText")[0].Params
	if edit["text"] != "Started update." || edit["reply_markup"] != nil {
		t.Errorf("edit = %v, want the start's reply without buttons", edit)
	}
}

func TestRollbackOffer(t *testing.T) {
	for _, tc := range []struct {
		status  UpdateStatus
		want    string
		buttons int
	}{
		{UpdateStatus{Current: "v1.1.0", Previous: "v1.0.0"}, "Rolling back to v1.0.0", 1},
		{UpdateStatus{Current: "v1.1.0"}, "no previous version", 0},
		{UpdateStatus{Current: "v1.1.0", Previous: "v1.0.0", Busy: true}, "under way", 0},
	} {
		got := rollbackOffer(tc.status)
		n := 0
		if got.keyboard != nil {
			n = len(got.keyboard.InlineKeyboard[0])
		}
		if !strings.Contains(got.text, tc.want) || n != tc.buttons {
			t.Errorf("rollbackOffer(%+v) = %q with %d buttons", tc.status, got.text, n)
		}
	}
}

func TestUpdateOfferWhenNothingToInstall(t *testing.T) {
	for _, tc := range []struct {
		status UpdateStatus
		want   string
	}{
		{UpdateStatus{Current: "v1.1.0", Latest: "v1.1.0", Previous: "v1.0.0"}, "up to date. /rollback goes back to v1.0.0"},
		{UpdateStatus{Current: "v1.1.0", LatestError: "GitHub is down"}, "couldn't check for a newer release: GitHub is down"},
		{UpdateStatus{Current: "v1.1.0", Latest: "v1.2.0", Available: true, Busy: true}, "under way"},
	} {
		got := updateOffer(tc.status)
		if !strings.Contains(got.text, tc.want) || got.keyboard != nil {
			t.Errorf("updateOffer(%+v) = %q, keyboard %v", tc.status, got.text, got.keyboard)
		}
	}
}

func TestUpdateWithoutUpdaterSaysHow(t *testing.T) {
	f := newFakeAPI(t)
	b, _, _ := testBot(t, f, 42)
	start(t, b, &fakeSessions{})
	f.add(textUpdate(1, 42, 42, "/update"))
	eventually(t, "reply sent", func() bool { return len(f.callsOf("sendMessage")) == 1 })
	if text := f.callsOf("sendMessage")[0].Params["text"].(string); text != noUpdates {
		t.Errorf("reply = %q", text)
	}
}

func TestAnnounceUpdateGoesToEachUser(t *testing.T) {
	f := newFakeAPI(t)
	b, _, _ := testBot(t, f, 42, 43)
	b.AnnounceUpdate(UpdateStatus{Current: "v1.0.0", Latest: "v1.1.0", Available: true})
	eventually(t, "announcements sent", func() bool { return len(f.callsOf("sendMessage")) == 2 })
	chats := map[int64]bool{}
	for _, c := range f.callsOf("sendMessage") {
		chats[num(c.Params["chat_id"])] = true
		if got := buttonsOf(c.Params); len(got) == 0 || got[0] != updatePrefix+"v1.1.0" {
			t.Errorf("announcement buttons = %q", got)
		}
	}
	if !chats[42] || !chats[43] {
		t.Errorf("announced in %v", chats)
	}
}
