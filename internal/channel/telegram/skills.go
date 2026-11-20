package telegram

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Skills lets the user list and delete skills with /skills, without going
// through the model.
type Skills interface {
	SkillList() []SkillEntry
	DeleteSkill(name string) error
}

// SkillEntry is one skill in /skills.
type SkillEntry struct {
	Name        string
	Description string
}

// SetSkills makes /skills list and delete s's skills. Call it before Run.
func (b *Bot) SetSkills(s Skills) { b.skills = s }

// deleteSkillPrefix starts the callback data of /skills' buttons. A
// skill's name can be longer than callback data allows, so the button
// carries a hash of it.
const deleteSkillPrefix = "unskill:"

func skillKey(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:8])
}

// skillList is /skills' message.
func (b *Bot) skillList() buttons {
	if b.skills == nil {
		return buttons{text: "Skills aren't available."}
	}
	entries := b.skills.SkillList()
	if len(entries) == 0 {
		return buttons{text: "No skills yet. I save one when I work out how to do something, or send me one to install (a link or the files)."}
	}
	var text strings.Builder
	text.WriteString("Skills:\n")
	kb := &inlineKeyboard{}
	for _, e := range entries {
		desc := e.Description
		if utf8.RuneCountInString(desc) > 150 {
			desc = string([]rune(desc)[:149]) + "…"
		}
		fmt.Fprintf(&text, "\n• %s: %s", e.Name, desc)
		kb.InlineKeyboard = append(kb.InlineKeyboard, []inlineButton{{Text: "Delete " + e.Name, CallbackData: deleteSkillPrefix + skillKey(e.Name)}})
	}
	return buttons{text: text.String(), keyboard: kb}
}

// deleteSkill handles a press of a /skills button: it deletes the skill
// and updates the list.
func (b *Bot) deleteSkill(q *callbackQuery, key string, reply func(string)) {
	if b.skills == nil {
		reply("")
		return
	}
	var name string
	var err error
	for _, e := range b.skills.SkillList() {
		if skillKey(e.Name) == key {
			name, err = e.Name, b.skills.DeleteSkill(e.Name)
			break
		}
	}
	switch {
	case name == "":
		reply("That skill is already gone.")
	case err != nil:
		reply("Couldn't delete " + name + ": " + firstLine(err.Error(), 150))
	default:
		reply("Deleted " + name + ".")
	}
	ctx, cancel := context.WithTimeout(b.ctx, requestTimeout)
	defer cancel()
	l := b.skillList()
	b.api.callRetry(ctx, "editMessageText", editMessageTextParams{ChatID: q.Message.Chat.ID, MessageID: q.Message.MessageID, Text: l.text, ReplyMarkup: l.keyboard}, nil, requestTimeout, b.sendAttempts)
}
