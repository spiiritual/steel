package telegram

import (
	"slices"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/spiiritual/steel/internal/agent"
)

// shown describes entities as "type:text" (plus "=url" or "/language"),
// slicing text by UTF-16 offsets the way Telegram does.
func shown(text string, ents []messageEntity) []string {
	units := utf16.Encode([]rune(text))
	var out []string
	for _, e := range ents {
		s := e.Type + ":" + string(utf16.Decode(units[e.Offset:e.Offset+e.Length]))
		if e.URL != "" {
			s += "=" + e.URL
		}
		if e.Language != "" {
			s += "/" + e.Language
		}
		out = append(out, s)
	}
	return out
}

func TestMarkdown(t *testing.T) {
	for _, tc := range []struct {
		name, md, text string
		ents           []string
	}{
		{
			name: "inline styles",
			md:   "**Bold**, *italic*, `code`, ~~gone~~ and [a link](https://example.com).",
			text: "Bold, italic, code, gone and a link.",
			ents: []string{"bold:Bold", "italic:italic", "code:code", "strikethrough:gone", "text_link:a link=https://example.com"},
		},
		{
			// Offsets count UTF-16 units: the emoji takes two.
			name: "offsets after an emoji",
			md:   "🚀 **go**",
			text: "🚀 go",
			ents: []string{"bold:go"},
		},
		{
			name: "nested styles",
			md:   "**bold with *italic* and `code`**",
			text: "bold with italic and code",
			ents: []string{"bold:bold with italic and code", "italic:italic", "code:code"},
		},
		{
			// Where hand-rolled converters go wrong.
			name: "underscores and asterisks that aren't emphasis",
			md:   "set max_retry_count to 2 * 3 * 4, see file_name.go",
			text: "set max_retry_count to 2 * 3 * 4, see file_name.go",
		},
		{
			name: "escapes",
			md:   `a \*literal\* star and 1\. not a list`,
			text: "a *literal* star and 1. not a list",
		},
		{
			name: "code block keeps its text and language",
			md:   "Run:\n\n```go\nx := a**b // *not* bold\n```\n\nDone.",
			text: "Run:\n\nx := a**b // *not* bold\n\nDone.",
			ents: []string{"pre:x := a**b // *not* bold/go"},
		},
		{
			name: "headings and lists",
			md:   "# Plan\n\n- one\n- two\n  1. nested\n  2. more\n\n3. three",
			text: "Plan\n\n• one\n• two\n  1. nested\n  2. more\n\n3. three",
			ents: []string{"bold:Plan"},
		},
		{
			name: "blockquotes don't nest",
			md:   "> quoted **text**\n>\n> > deeper",
			text: "quoted text\n\ndeeper",
			ents: []string{"blockquote:quoted text\n\ndeeper", "bold:text"},
		},
		{
			name: "table becomes an aligned monospace block",
			md:   "| Name | Size |\n|---|---|\n| a.txt | 12 KB |\n| b | 3 |",
			text: "Name  | Size\n------+------\na.txt | 12 KB\nb     | 3",
			ents: []string{"pre:Name  | Size\n------+------\na.txt | 12 KB\nb     | 3"},
		},
		{
			name: "table column alignment",
			md:   "| left | right | mid |\n|:--|--:|:-:|\n| a | 1 | x |\n| bbbb | 1000 | yyyyy |",
			text: "left | right |  mid\n-----+-------+------\na    |     1 |   x\nbbbb |  1000 | yyyyy",
			ents: []string{"pre:left | right |  mid\n-----+-------+------\na    |     1 |   x\nbbbb |  1000 | yyyyy"},
		},
		{
			// Emoji and CJK take two columns in a monospace font.
			name: "table with wide characters",
			md:   "| Status | Who |\n|---|---|\n| ✅ ok | 太郎 |\n| ⚠️ warn | ann |",
			text: "Status  | Who\n--------+-----\n✅ ok   | 太郎\n⚠️ warn | ann",
			ents: []string{"pre:Status  | Who\n--------+-----\n✅ ok   | 太郎\n⚠️ warn | ann"},
		},
		{
			// A link can't contain code; a relative link can't be opened.
			name: "links Telegram can't show",
			md:   "[`run`](https://x.dev) and [notes](./notes.md) and ![chart](https://x.dev/c.png)",
			text: "run and notes and chart",
			ents: []string{"text_link:run=https://x.dev", "text_link:chart=https://x.dev/c.png"},
		},
		{
			name: "line breaks are kept",
			md:   "first line\nsecond line",
			text: "first line\nsecond line",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := markdown(tc.md)
			if f.text != tc.text {
				t.Errorf("text = %q, want %q", f.text, tc.text)
			}
			if got := shown(f.text, f.entities(0, len(f.text))); !slices.Equal(got, tc.ents) {
				t.Errorf("entities = %q, want %q", got, tc.ents)
			}
		})
	}
}

func TestDisplayWidth(t *testing.T) {
	for s, want := range map[string]int{
		"abc":   3,
		"太郎":    4,
		"✅":     2, // wide by itself
		"⚠️":    2, // a text symbol made emoji by U+FE0F
		"⚠":     1,
		"📦":     2,
		"👨‍👩‍👧": 2, // one drawn family
		"é":     1, // e and a combining accent
	} {
		if got := displayWidth(s); got != want {
			t.Errorf("displayWidth(%q) = %d, want %d", s, got, want)
		}
	}
}

// Replies don't grow a preview card of their first link.
func TestNoLinkPreviews(t *testing.T) {
	f := newFakeAPI(t)
	b, _, _ := testBot(t, f, 42)
	b.Emit("telegram:42", agent.Reply{Text: "See [the docs](https://core.telegram.org/bots/api)."})
	eventually(t, "sent", func() bool { return len(f.callsOf("sendMessage")) == 1 })
	opts, _ := f.callsOf("sendMessage")[0].Params["link_preview_options"].(map[string]any)
	if opts["is_disabled"] != true {
		t.Errorf("link_preview_options = %v", f.callsOf("sendMessage")[0].Params["link_preview_options"])
	}
}

// A long reply splits into messages, each with the formatting that falls
// in it, offsets counted from its own start.
func TestFormattedReplySplits(t *testing.T) {
	f := newFakeAPI(t)
	b, _, _ := testBot(t, f, 42)
	code := strings.Repeat("line of code\n", 400) // ~5200 characters
	b.Emit("telegram:42", agent.Reply{Text: "**Here** it is:\n\n```\n" + code + "```\n\n*done*"})
	eventually(t, "messages", func() bool { return len(f.callsOf("sendMessage")) == 2 })
	var all []string
	for _, c := range f.callsOf("sendMessage") {
		text := c.Params["text"].(string)
		if utf16Len(text) > maxText {
			t.Errorf("a part has %d characters", utf16Len(text))
		}
		var ents []messageEntity
		for _, e := range c.Params["entities"].([]any) {
			e := e.(map[string]any)
			ents = append(ents, messageEntity{Type: e["type"].(string), Offset: int(num(e["offset"])), Length: int(num(e["length"]))})
		}
		for _, s := range shown(text, ents) {
			typ, body, _ := strings.Cut(s, ":")
			all = append(all, typ)
			if typ == "pre" && strings.Contains(body, "Here") {
				t.Errorf("the code block's entity reaches outside it: %q", body)
			}
		}
	}
	// The code block is in both parts; bold and italic in one each.
	if want := []string{"bold", "pre", "pre", "italic"}; !slices.Equal(all, want) {
		t.Errorf("entities %q, want %q", all, want)
	}
}

// If Telegram refuses the formatting, the reply still arrives, plain.
func TestRefusedFormattingFallsBackToPlain(t *testing.T) {
	f := newFakeAPI(t)
	b, log, _ := testBot(t, f, 42)
	f.respond("sendMessage", 400, `{"ok":false,"error_code":400,"description":"Bad Request: can't parse entities: unsupported nesting"}`)
	b.Emit("telegram:42", agent.Reply{Text: "**hi**"})
	eventually(t, "resent", func() bool { return len(f.callsOf("sendMessage")) == 2 })
	calls := f.callsOf("sendMessage")
	if calls[0].Params["entities"] == nil || calls[1].Params["entities"] != nil || calls[1].Params["text"] != "hi" {
		t.Errorf("calls: %v", calls)
	}
	if !strings.Contains(log.String(), "formatting refused") {
		t.Errorf("log: %s", log.String())
	}
}

// steel's own messages aren't Markdown: an error mentioning *args stays as is.
func TestNoticesStayPlain(t *testing.T) {
	f := newFakeAPI(t)
	b, _, _ := testBot(t, f, 42)
	b.Emit("telegram:42", agent.Notice{Text: "bad **kwargs in `f`"})
	eventually(t, "sent", func() bool { return len(f.callsOf("sendMessage")) == 1 })
	c := f.callsOf("sendMessage")[0].Params
	if c["text"] != "bad **kwargs in `f`" || c["entities"] != nil {
		t.Errorf("notice sent as %v", c)
	}
}

func TestDraftsAreFormatted(t *testing.T) {
	f := newFakeAPI(t)
	b, _, _ := testBot(t, f, 42)
	start(t, b, &fakeSessions{})
	eventually(t, "polling", b.polling.Load)
	b.Emit("telegram:42", agent.TextDelta{Text: "Use `go test`"})
	eventually(t, "draft", func() bool { return slices.Contains(f.texts("sendMessageDraft"), "Use go test") })
	for _, c := range f.callsOf("sendMessageDraft") {
		if c.Params["text"] == "Use go test" {
			e := c.Params["entities"].([]any)[0].(map[string]any)
			if e["type"] != "code" || num(e["offset"]) != 4 || num(e["length"]) != 7 {
				t.Errorf("draft entity %v", e)
			}
		}
	}
}
