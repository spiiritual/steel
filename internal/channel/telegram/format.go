package telegram

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	east "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
	"golang.org/x/text/width"
)

// Models write Markdown, which Telegram shows as literal asterisks and
// backticks. Replies are converted to plain text plus message entities
// (https://core.telegram.org/bots/api#formatting-options) rather than to
// MarkdownV2 or HTML: nothing needs escaping, so no text can make a
// message fail to parse, and a long reply splits into messages by
// clipping entities to each part.

// formatted is text with formatting entities. Spans are byte ranges of
// text; they become UTF-16 entities only when a part is sent.
type formatted struct {
	text  string
	spans []span
}

// span is a formatted byte range [start, end) of a formatted's text.
type span struct {
	typ        string // a MessageEntity type: bold, italic, code, pre, ...
	start, end int
	url        string // text_link
	language   string // pre
}

// plain is text without formatting.
func plain(s string) formatted { return formatted{text: s} }

// markdownParser parses CommonMark with GitHub's strikethrough and tables.
var markdownParser = goldmark.New(goldmark.WithExtensions(extension.Strikethrough, extension.Table)).Parser()

// markdown converts a model's Markdown to Telegram formatting. Headings
// become bold lines, list markers bullets, tables monospace blocks; what
// Telegram can't show (images, raw HTML) stays as readable text.
func markdown(src string) formatted {
	source := []byte(src)
	doc := markdownParser.Parse(text.NewReader(source))
	w := &mdWriter{src: source}
	w.blocks(doc)
	f := formatted{text: strings.TrimRight(w.b.String(), " \n"), spans: w.spans}
	// Trimming may have cut the ends of spans.
	for i := range f.spans {
		f.spans[i].end = min(f.spans[i].end, len(f.text))
	}
	f.spans = slices.DeleteFunc(f.spans, func(s span) bool { return s.start >= s.end })
	// Outer entities first, as Telegram lists them.
	slices.SortStableFunc(f.spans, func(a, b span) int {
		return cmp.Or(cmp.Compare(a.start, b.start), cmp.Compare(b.end, a.end))
	})
	return f
}

// mdWriter renders a Markdown AST as formatted text.
type mdWriter struct {
	src    []byte
	b      strings.Builder
	spans  []span
	indent string // written after each line break inside list items
	quoted bool   // inside a blockquote, which can't nest
	linked bool   // inside a link, which can't contain code
}

// write appends s, indenting the lines after its line breaks.
func (w *mdWriter) write(s string) {
	if w.indent != "" {
		s = strings.ReplaceAll(s, "\n", "\n"+w.indent)
	}
	w.b.WriteString(s)
}

// wrap formats what f writes as typ, unless f writes nothing.
func (w *mdWriter) wrap(s span, f func()) {
	s.start = w.b.Len()
	f()
	if s.end = w.b.Len(); s.end > s.start {
		w.spans = append(w.spans, s)
	}
}

// blocks renders n's block children, a blank line between each, or a
// line break in a tight list.
func (w *mdWriter) blocks(n ast.Node) {
	tight := false
	if l, ok := n.Parent().(*ast.List); ok && n.Kind() == ast.KindListItem {
		tight = l.IsTight
	}
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		if c != n.FirstChild() {
			if tight {
				w.write("\n")
			} else {
				w.write("\n\n")
			}
		}
		w.block(c)
	}
}

func (w *mdWriter) block(n ast.Node) {
	switch n := n.(type) {
	case *ast.Paragraph, *ast.TextBlock:
		w.inlines(n)
	case *ast.Heading:
		w.wrap(span{typ: "bold"}, func() { w.inlines(n) })
	case *ast.ThematicBreak:
		w.write("———")
	case *ast.FencedCodeBlock:
		w.code(n, string(n.Language(w.src)))
	case *ast.CodeBlock:
		w.code(n, "")
	case *ast.HTMLBlock:
		w.write(strings.TrimRight(w.lines(n), "\n"))
	case *ast.Blockquote:
		if w.quoted {
			w.blocks(n)
			return
		}
		w.quoted = true
		w.wrap(span{typ: "blockquote"}, func() { w.blocks(n) })
		w.quoted = false
	case *ast.List:
		w.list(n)
	case *east.Table:
		w.table(n)
	default:
		w.blocks(n)
	}
}

// lines returns a block's raw source lines.
func (w *mdWriter) lines(n ast.Node) string {
	var s strings.Builder
	lines := n.Lines()
	for i := range lines.Len() {
		seg := lines.At(i)
		s.Write(seg.Value(w.src))
	}
	return s.String()
}

func (w *mdWriter) code(n ast.Node, language string) {
	body := strings.TrimRight(w.lines(n), "\n")
	if body == "" {
		return
	}
	w.wrap(span{typ: "pre", language: language}, func() { w.write(body) })
}

func (w *mdWriter) list(l *ast.List) {
	num := l.Start
	for item := l.FirstChild(); item != nil; item = item.NextSibling() {
		if item != l.FirstChild() {
			if l.IsTight {
				w.write("\n")
			} else {
				w.write("\n\n")
			}
		}
		marker := "• "
		if l.IsOrdered() {
			marker = strconv.Itoa(num) + ". "
			num++
		}
		w.write(marker)
		outer := w.indent
		w.indent += strings.Repeat(" ", utf8.RuneCountInString(marker))
		w.blocks(item)
		w.indent = outer
	}
}

// table renders a table monospaced, with its columns aligned.
func (w *mdWriter) table(t *east.Table) {
	var rows [][]string
	for r := t.FirstChild(); r != nil; r = r.NextSibling() {
		var row []string
		for c := r.FirstChild(); c != nil; c = c.NextSibling() {
			cell := &mdWriter{src: w.src}
			cell.inlines(c)
			row = append(row, strings.ReplaceAll(cell.b.String(), "\n", " "))
		}
		rows = append(rows, row)
	}
	var widths []int
	for _, row := range rows {
		for i, cell := range row {
			if i == len(widths) {
				widths = append(widths, 0)
			}
			widths[i] = max(widths[i], displayWidth(cell))
		}
	}
	var s strings.Builder
	for ri, row := range rows {
		if ri > 0 {
			s.WriteString("\n")
		}
		for i, cell := range row {
			if i > 0 {
				s.WriteString(" | ")
			}
			pad := widths[i] - displayWidth(cell)
			left := 0
			if i < len(t.Alignments) {
				switch t.Alignments[i] {
				case east.AlignRight:
					left = pad
				case east.AlignCenter:
					left = pad / 2
				}
			}
			right := pad - left
			if i == len(row)-1 {
				right = 0 // no trailing spaces
			}
			s.WriteString(strings.Repeat(" ", left) + cell + strings.Repeat(" ", right))
		}
		if ri == 0 && len(rows) > 1 { // under the header
			s.WriteString("\n")
			for i, wd := range widths {
				if i > 0 {
					s.WriteString("-+-")
				}
				s.WriteString(strings.Repeat("-", wd))
			}
		}
	}
	w.wrap(span{typ: "pre"}, func() { w.write(s.String()) })
}

// displayWidth is how many columns s takes in a monospace font: CJK and
// emoji take two; marks, joiners and variation selectors none; an emoji
// after a zero-width joiner none (it draws as one with the emoji before
// it); and a symbol followed by U+FE0F ("⚠️") two, as it draws as emoji.
func displayWidth(s string) int {
	n, last := 0, 0
	joined := false
	for _, r := range s {
		switch {
		case r == '‍':
			joined = true
		case joined:
			joined = false
		case r == '️':
			if last == 1 {
				n, last = n+1, 2
			}
		case unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf) || r >= 0xfe00 && r <= 0xfe0e:
		case wide(r):
			n, last = n+2, 2
		default:
			n, last = n+1, 1
		}
	}
	return n
}

func wide(r rune) bool {
	switch width.LookupRune(r).Kind() {
	case width.EastAsianWide, width.EastAsianFullwidth:
		return true
	}
	// Emoji drawn as pictures, which East Asian Width calls neutral.
	return r >= 0x1f000 && r <= 0x1faff
}

// inlines renders n's inline children.
func (w *mdWriter) inlines(n ast.Node) {
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		w.inline(c)
	}
}

func (w *mdWriter) inline(n ast.Node) {
	switch n := n.(type) {
	case *ast.Text:
		v := n.Value(w.src)
		if !n.IsRaw() {
			v = util.ResolveEntityNames(util.ResolveNumericReferences(util.UnescapePunctuations(v)))
		}
		w.write(string(v))
		if n.SoftLineBreak() || n.HardLineBreak() {
			// Models break lines on purpose; Telegram keeps them.
			w.write("\n")
		}
	case *ast.String:
		w.write(string(n.Value))
	case *ast.CodeSpan:
		var s strings.Builder
		for c := n.FirstChild(); c != nil; c = c.NextSibling() {
			if t, ok := c.(*ast.Text); ok {
				s.Write(t.Value(w.src))
			}
		}
		if w.linked { // a link can't contain code
			w.write(s.String())
			return
		}
		w.wrap(span{typ: "code"}, func() { w.write(s.String()) })
	case *ast.Emphasis:
		typ := "italic"
		if n.Level >= 2 {
			typ = "bold"
		}
		w.wrap(span{typ: typ}, func() { w.inlines(n) })
	case *east.Strikethrough:
		w.wrap(span{typ: "strikethrough"}, func() { w.inlines(n) })
	case *ast.Link:
		w.link(n, string(n.Destination))
	case *ast.Image:
		w.link(n, string(n.Destination))
	case *ast.AutoLink:
		// Telegram finds URLs and addresses in text by itself.
		w.write(string(n.Label(w.src)))
	case *ast.RawHTML:
		for i := range n.Segments.Len() {
			seg := n.Segments.At(i)
			w.write(string(seg.Value(w.src)))
		}
	default:
		w.inlines(n)
	}
}

// link renders n's text linked to url. Links Telegram wouldn't open
// (relative paths, javascript:) are left as their text.
func (w *mdWriter) link(n ast.Node, url string) {
	if !linkable(url) || w.linked {
		w.inlines(n)
		return
	}
	w.linked = true
	w.wrap(span{typ: "text_link", url: url}, func() { w.inlines(n) })
	w.linked = false
}

func linkable(url string) bool {
	for _, scheme := range []string{"https://", "http://", "tg://", "mailto:"} {
		if len(url) > len(scheme) && strings.EqualFold(url[:len(scheme)], scheme) {
			return true
		}
	}
	return false
}

// entities returns f's spans within its text bytes [start, end), clipped
// to that range and measured in UTF-16 from start, as Telegram wants.
func (f formatted) entities(start, end int) []messageEntity {
	var out []messageEntity
	for _, s := range f.spans {
		a, b := max(s.start, start), min(s.end, end)
		// Whitespace-only entities are pointless and Telegram may reject them.
		if a >= b || strings.TrimSpace(f.text[a:b]) == "" {
			continue
		}
		out = append(out, messageEntity{
			Type:     s.typ,
			Offset:   utf16Len(f.text[start:a]),
			Length:   utf16Len(f.text[a:b]),
			URL:      s.url,
			Language: s.language,
		})
	}
	return out
}
