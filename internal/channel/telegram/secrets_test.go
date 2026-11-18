package telegram

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spiiritual/steel/internal/agent"
	"github.com/spiiritual/steel/internal/secret"
)

// The message after a model's secret request is the value: it goes to the
// session's request, is deleted from the chat, and never becomes a
// message. Commands still work meanwhile, and once the request is
// answered, messages are messages again.
func TestSecretRequestCapturesNextMessage(t *testing.T) {
	f := newFakeAPI(t)
	b, _, _ := testBot(t, f, 42)
	ss := &fakeSessions{}
	start(t, b, ss)
	eventually(t, "polling", b.polling.Load)

	b.Emit("telegram:42", agent.SecretRequest{ID: "live1", Name: "GH_TOKEN", Purpose: "GitHub API calls"})
	eventually(t, "prompt", func() bool { return len(f.callsOf("sendMessage")) == 1 })
	msg := f.callsOf("sendMessage")[0].Params
	if text := msg["text"].(string); !strings.HasPrefix(text, "🔑 steel needs GH_TOKEN\nFor: GitHub API calls") {
		t.Errorf("prompt %q", text)
	}
	if got := fmt.Sprint(msg["reply_markup"]); !strings.Contains(got, "unsecret:live1") {
		t.Errorf("buttons %s", got)
	}

	f.add(
		textUpdate(1, 42, 42, "/reset"),
		textUpdate(2, 42, 42, " ghp_value123\n"),
		textUpdate(3, 42, 42, "hello"),
	)
	eventually(t, "messages handled", func() bool { return lastOffset(f) == 4 })
	want := []string{"telegram:42: /reset", `telegram:42: secret live1 "ghp_value123"`, "telegram:42: hello"}
	if !slices.Equal(ss.got(), want) {
		t.Errorf("sessions got %q, want %q", ss.got(), want)
	}
	eventually(t, "value deleted", func() bool { return len(f.callsOf("deleteMessage")) == 1 })
	if del := f.callsOf("deleteMessage"); num(del[0].Params["message_id"]) != 2 {
		t.Errorf("deleted %v", del)
	}

	b.Emit("telegram:42", agent.SecretDone{ID: "live1", Outcome: agent.SecretReceived})
	eventually(t, "outcome", func() bool { return len(f.callsOf("editMessageText")) == 1 })
	if text := f.callsOf("editMessageText")[0].Params["text"].(string); !strings.HasSuffix(text, "\n\n✅ Saved") {
		t.Errorf("outcome %q", text)
	}
}

func TestSecretRequestCancel(t *testing.T) {
	f := newFakeAPI(t)
	b, _, _ := testBot(t, f, 42)
	ss := &fakeSessions{}
	start(t, b, ss)
	eventually(t, "polling", b.polling.Load)

	b.Emit("telegram:42", agent.SecretRequest{ID: "live1", Name: "GH_TOKEN", Purpose: "x"})
	eventually(t, "prompt", func() bool { return len(f.callsOf("sendMessage")) == 1 })
	f.add(pressUpdate(1, 42, 42, 1, "unsecret:live1"))
	eventually(t, "cancelled", func() bool { return len(ss.got()) == 1 })
	if got := ss.got()[0]; got != "telegram:42: cancel secret live1" {
		t.Errorf("sessions got %q", got)
	}
	b.Emit("telegram:42", agent.SecretDone{ID: "live1", Outcome: agent.SecretDeclined})
	f.add(textUpdate(2, 42, 42, "hello"))
	eventually(t, "message", func() bool { return len(ss.got()) == 2 })
	if got := ss.got()[1]; got != "telegram:42: hello" {
		t.Errorf("after cancel, sessions got %q", got)
	}
}

// /secret NAME saves the next message into the store, without involving a
// session.
func TestSecretCommand(t *testing.T) {
	f := newFakeAPI(t)
	b, _, _ := testBot(t, f, 42)
	path := filepath.Join(t.TempDir(), ".env")
	store, err := secret.Open(path, secret.Options{})
	if err != nil {
		t.Fatal(err)
	}
	b.SetSecrets(store)
	ss := &fakeSessions{}
	start(t, b, ss)
	eventually(t, "polling", b.polling.Load)

	f.add(textUpdate(1, 42, 42, "/secret lower"))
	eventually(t, "bad name refused", func() bool { return len(f.callsOf("sendMessage")) == 1 })
	f.add(textUpdate(2, 42, 42, "/secret API_KEY"), textUpdate(3, 42, 42, "key-12345"))
	eventually(t, "saved", func() bool { return len(f.callsOf("editMessageText")) == 1 })
	if len(ss.got()) != 0 {
		t.Errorf("sessions got %q", ss.got())
	}
	if b, _ := os.ReadFile(path); string(b) != "API_KEY=key-12345\n" {
		t.Errorf(".env = %q", b)
	}
	eventually(t, "value deleted", func() bool { return len(f.callsOf("deleteMessage")) == 1 })
	if del := f.callsOf("deleteMessage"); num(del[0].Params["message_id"]) != 3 {
		t.Errorf("deleted %v", del)
	}
	if text := f.callsOf("editMessageText")[0].Params["text"].(string); !strings.HasSuffix(text, "✅ Saved") {
		t.Errorf("outcome %q", text)
	}
}

// A reply to a secret prompt whose request is gone, e.g. from before a
// restart, is deleted rather than sent to the model.
func TestReplyToStalePromptIsDeleted(t *testing.T) {
	f := newFakeAPI(t)
	b, _, _ := testBot(t, f, 42)
	ss := &fakeSessions{}
	start(t, b, ss)
	eventually(t, "polling", b.polling.Load)

	u := textUpdate(1, 42, 42, "ghp_value123")
	u["message"].(map[string]any)["reply_to_message"] = map[string]any{
		"message_id": 9, "from": map[string]any{"id": 1, "is_bot": true}, "chat": map[string]any{"id": 42, "type": "private"},
		"text": "🔑 steel needs GH_TOKEN\n\nCancelled",
	}
	f.add(u)
	eventually(t, "deleted", func() bool { return len(f.callsOf("deleteMessage")) == 1 })
	eventually(t, "notice", func() bool { return len(f.callsOf("sendMessage")) == 1 })
	if len(ss.got()) != 0 {
		t.Errorf("sessions got %q", ss.got())
	}
}

// A value that can't be saved is reported on the prompt, not as saved.
func TestSecretCommandNotSaved(t *testing.T) {
	f := newFakeAPI(t)
	b, _, _ := testBot(t, f, 42)
	dir := t.TempDir()
	store, err := secret.Open(filepath.Join(dir, "secrets.env"), secret.Options{})
	if err != nil {
		t.Fatal(err)
	}
	// A read-only directory: saving fails.
	os.Chmod(dir, 0o500)
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	b.SetSecrets(store)
	start(t, b, &fakeSessions{})
	eventually(t, "polling", b.polling.Load)

	f.add(textUpdate(1, 42, 42, "/secret API_KEY"), textUpdate(2, 42, 42, "key-12345"))
	eventually(t, "outcome", func() bool { return len(f.callsOf("editMessageText")) == 1 })
	if text := f.callsOf("editMessageText")[0].Params["text"].(string); !strings.Contains(text, "❌ Not saved: ") {
		t.Errorf("outcome %q", text)
	}
}

// A reply to the prompt for another secret isn't taken as the value of the
// one waiting now.
func TestReplyToOtherPromptIsntTheValue(t *testing.T) {
	f := newFakeAPI(t)
	b, _, _ := testBot(t, f, 42)
	ss := &fakeSessions{}
	start(t, b, ss)
	eventually(t, "polling", b.polling.Load)

	b.Emit("telegram:42", agent.SecretRequest{ID: "live1", Name: "OPENAI_KEY", Purpose: "x"})
	eventually(t, "prompt", func() bool { return len(f.callsOf("sendMessage")) == 1 })
	reply := func(n int64, prompt string) map[string]any {
		u := textUpdate(n, 42, 42, "ghp_value123")
		u["message"].(map[string]any)["reply_to_message"] = map[string]any{
			"message_id": 9, "from": map[string]any{"id": 1, "is_bot": true}, "chat": map[string]any{"id": 42, "type": "private"},
			"text": prompt,
		}
		return u
	}
	f.add(reply(1, "🔑 steel needs GITHUB_TOKEN\n\nSend it…\n\n⌛ Nothing was sent in time; nothing was saved"))
	eventually(t, "deleted", func() bool { return len(f.callsOf("deleteMessage")) == 1 })
	if len(ss.got()) != 0 {
		t.Errorf("sessions got %q", ss.got())
	}
	f.add(reply(2, "🔑 steel needs OPENAI_KEY\nFor: x"))
	eventually(t, "provided", func() bool { return len(ss.got()) == 1 })
	if got := ss.got()[0]; got != `telegram:42: secret live1 "ghp_value123"` {
		t.Errorf("sessions got %q", got)
	}
}
