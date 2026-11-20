package telegram

import (
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
)

// fakeSkills is a list of skills /skills can delete.
type fakeSkills struct {
	mu      sync.Mutex
	entries []SkillEntry
}

func (s *fakeSkills) SkillList() []SkillEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.entries)
}

func (s *fakeSkills) DeleteSkill(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.entries, func(e SkillEntry) bool { return e.Name == name })
	if i < 0 {
		return errors.New("no such skill")
	}
	s.entries = slices.Delete(s.entries, i, i+1)
	return nil
}

func TestSkillsCommandListsAndDeletes(t *testing.T) {
	f := newFakeAPI(t)
	b, _, _ := testBot(t, f, 42)
	long := strings.Repeat("x", 64)
	b.SetSkills(&fakeSkills{entries: []SkillEntry{
		{Name: "deploy-blog", Description: "Deploy the blog to Netlify."},
		{Name: long, Description: "A skill with the longest name allowed."},
	}})
	ss := &fakeSessions{}
	start(t, b, ss)
	f.add(textUpdate(1, 42, 42, "/skills"))
	eventually(t, "list sent", func() bool { return len(f.callsOf("sendMessage")) == 1 })
	list := f.callsOf("sendMessage")[0].Params
	if text := list["text"].(string); !strings.Contains(text, "deploy-blog: Deploy the blog to Netlify.") || !strings.Contains(text, long) {
		t.Errorf("list = %q", text)
	}
	if len(ss.got()) != 0 {
		t.Errorf("/skills reached the model: %q", ss.got())
	}
	// Telegram refuses callback data over 64 bytes.
	data := deleteSkillPrefix + skillKey(long)
	if len(data) > 64 {
		t.Fatalf("callback data is %d bytes", len(data))
	}

	f.add(pressUpdate(2, 42, 42, 1, data))
	eventually(t, "list updated", func() bool { return len(f.callsOf("editMessageText")) == 1 })
	if text := f.callsOf("editMessageText")[0].Params["text"].(string); strings.Contains(text, long) || !strings.Contains(text, "deploy-blog") {
		t.Errorf("updated list = %q", text)
	}
	if got := f.callsOf("answerCallbackQuery")[0].Params["text"]; got != "Deleted "+long+"." {
		t.Errorf("answer = %v", got)
	}

	f.add(pressUpdate(3, 42, 42, 1, data))
	eventually(t, "second answer", func() bool { return len(f.callsOf("answerCallbackQuery")) == 2 })
	if got := f.callsOf("answerCallbackQuery")[1].Params["text"]; got != "That skill is already gone." {
		t.Errorf("answer = %v", got)
	}
}
