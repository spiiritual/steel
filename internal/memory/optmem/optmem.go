// Package optmem is a memory system: the model notes what's worth keeping,
// one line at a time, and every new conversation starts by reading its
// memory.
//
// Memories are kept word for word, forever, and summarized in the
// background (by a cheap model) into a binary tree of one-line summaries:
// pairs of memories, pairs of pairs, and so on. When a conversation
// starts, it is shown its memory: a bounded number of lines covering every
// memory, recent ones whole and old ones as coarser and coarser summaries.
// The model searches the raw memories by regex, or zooms a summary into
// its two halves.
//
// The design, prompt and files are OptMem's
// (github.com/VictorTaelin/OptMem), so a directory can be shared with its
// memo tool. Unlike memo, which hands summaries to the agent to write,
// steel writes them itself, so the model's only job is to take notes.
package optmem

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/spiiritual/steel/internal/llm"
	"github.com/spiiritual/steel/internal/memory"
)

// Options configures a Memory.
type Options struct {
	// Provider and Model write the summaries. A summary is about 1 per
	// note and small, so any model will do; a cheap one is enough. Model
	// returns the call each summary starts from (the model, and an effort
	// if the provider takes one); it's read at each call, so it can change
	// while the memory is open.
	Provider llm.Provider
	Model    func() llm.Request
	// Wait bounds how long Wake waits for summaries it needs. Default 1
	// minute; past it, unwritten ones show as placeholders to zoom.
	Wait time.Duration
	// OnError reports failures that don't stop the memory, e.g. a summary
	// that will be retried at the next note.
	OnError func(error)
	Now     func() time.Time
}

// Memory is an OptMem memory directory, open.
type Memory struct {
	s store
	o Options
	// wakeLines and entryChars are memo's WAKE_LINES and ENTRY_CHARS,
	// from the directory's config file if it sets them.
	wakeLines, entryChars int

	kick   chan struct{} // wakes the summarizer; buffered 1
	cancel context.CancelFunc
	done   chan struct{} // closed when the summarizer exits

	writeMu sync.Mutex // held while a summary is written: summarizer vs Rewrite

	mu       sync.Mutex
	progress chan struct{} // closed, and replaced, whenever the summarizer writes a summary or stops
	stuck    bool          // the summarizer's last round ended in an error
}

var _ memory.Memory = (*Memory)(nil)

// Open opens the memory in dir, creating it if needed, and starts writing
// any summaries that are due.
func Open(dir string, o Options) (*Memory, error) {
	if o.Wait == 0 {
		o.Wait = time.Minute
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Model == nil {
		o.Model = func() llm.Request { return llm.Request{} } // the provider's defaults
	}
	if o.OnError == nil {
		o.OnError = func(error) {}
	}
	if err := os.MkdirAll(filepath.Join(dir, "TREE"), 0o700); err != nil {
		return nil, err
	}
	m := &Memory{
		s: store{dir}, o: o, wakeLines: 96, entryChars: 280,
		kick: make(chan struct{}, 1), done: make(chan struct{}), progress: make(chan struct{}),
	}
	if err := m.readConfig(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	go m.summarizer(ctx)
	m.wake() // summaries an earlier run or memo left due
	return m, nil
}

// Close stops the summarizer, abandoning a summary in flight; the next
// Open writes it.
func (m *Memory) Close() {
	m.cancel()
	<-m.done
}

// readConfig applies memo's per-memory sizes. Only the ones that change
// what steel does matter; memo's output paging doesn't apply.
func (m *Memory) readConfig() error {
	path := filepath.Join(m.s.dir, "config")
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line, _, _ := strings.Cut(sc.Text(), "#")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.ToUpper(strings.TrimSpace(k)), strings.TrimSpace(v)
		i, err := strconv.Atoi(v)
		if err != nil || i < 1 {
			return fmt.Errorf("%s line %d: %s must be a positive whole number, not %q", path, n, k, v)
		}
		switch k {
		case "WAKE_LINES":
			m.wakeLines = i
		case "ENTRY_CHARS":
			m.entryChars = min(i, treeRec-8, logRec-40)
		case "PART_CHARS", "PART_LINES":
		default:
			return fmt.Errorf("%s line %d: %s is not a size", path, n, k)
		}
	}
	return sc.Err()
}

// EntryChars is the longest a memory may be, in bytes.
func (m *Memory) EntryChars() int { return m.entryChars }

// check validates one line of memory, a note or a summary.
func (m *Memory) check(text string) (string, error) {
	text = strings.TrimSpace(text)
	switch {
	case text == "":
		return "", errors.New("empty: a memory is one line of text")
	case !utf8.ValidString(text):
		return "", errors.New("not valid UTF-8")
	case strings.ContainsAny(text, "\r\n"):
		return "", fmt.Errorf("%d lines: a memory is one line; merge them, or note them separately", strings.Count(text, "\n")+1)
	case len(text) > m.entryChars:
		return "", fmt.Errorf("too long: %d bytes, limit %d (accented characters cost 2 bytes); compress it further", len(text), m.entryChars)
	}
	return text, nil
}

// Note records one memory and returns its id.
func (m *Memory) Note(text string) (int, error) {
	text, err := m.check(text)
	if err != nil {
		return 0, err
	}
	id, err := m.s.logAppend(m.o.Now().Format(time.DateOnly), text)
	if err != nil {
		return 0, err
	}
	m.wake()
	return id, nil
}

// recallMax caps what Recall returns, as memo caps its output.
const recallMax = 20_000

// Recall returns every memory matching the regular expression pattern,
// case-insensitively: the newest that fit, if there are too many.
func (m *Memory) Recall(pattern string) (string, error) {
	re, err := regexp.Compile("(?i)" + pattern)
	if err != nil {
		return "", fmt.Errorf("bad regex: %w", err)
	}
	var out []string
	hits, size := 0, 0
	err = m.s.scan(func(e entry) {
		line := e.String()
		if !re.MatchString(line) {
			return
		}
		hits++
		out = append(out, line)
		size += len(line) + 1
		for size > recallMax {
			size -= len(out[0]) + 1
			out = out[1:]
		}
	})
	if err != nil {
		return "", err
	}
	switch {
	case hits == 0:
		return "No match.", nil
	case len(out) < hits:
		return fmt.Sprintf("%s\nNewest %d of %d matches. Narrow the regex.", strings.Join(out, "\n"), len(out), hits), nil
	}
	return fmt.Sprintf("%s\n%s.", strings.Join(out, "\n"), plural(hits, "match", "matches")), nil
}

var blockID = regexp.MustCompile(`^#?(\d+)-(\d+)$`)

// Zoom opens block id ("lo-hi", inclusive, as the memory shows it) into its
// two halves: summaries, or memories once a half is one.
func (m *Memory) Zoom(id string) (string, error) {
	lo, hi, err := parseBlock(id)
	if err != nil {
		return "", err
	}
	T, err := m.s.logLen()
	if err != nil {
		return "", err
	}
	if lo >= T {
		return "", fmt.Errorf("#%s is beyond the memory: it holds %s", id, plural(T, "memory", "memories"))
	}
	var lines []string
	mid := (lo + hi) / 2
	for _, b := range [][2]int{{lo, mid}, {mid, hi}} {
		if b[0] >= T {
			continue // no memories there yet
		}
		line, err := m.line(b)
		if err != nil {
			return "", err
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n"), nil
}

// parseBlock parses a block id as the memory shows it: "lo-hi", inclusive,
// an aligned power-of-two range of at least 2. It returns [lo,hi).
func parseBlock(id string) (lo, hi int, err error) {
	g := blockID.FindStringSubmatch(strings.TrimSpace(id))
	if g == nil {
		return 0, 0, fmt.Errorf("%q is not a block id: copy one from your memory, like 16-31", id)
	}
	lo, _ = strconv.Atoi(g[1])
	last, _ := strconv.Atoi(g[2])
	hi = last + 1
	if n := hi - lo; n < 2 || n&(n-1) != 0 || lo%n != 0 {
		return 0, 0, fmt.Errorf("%s is not a block: copy an id from your memory, like 16-31", id)
	}
	return lo, hi, nil
}

// Rewrite writes block id's summary again, from what's below it, then each
// summary built on it, in place. Blocks up to rawMax are made from the raw
// memories, not from summaries, so only ancestors bigger than that change:
// at most log2(T/rawMax) more model calls, none for a small block. memo's forget
// truncates instead, which also drops every later summary at those levels
// to be rebuilt; at thousands of memories that's thousands of calls. It
// returns the new lines.
func (m *Memory) Rewrite(ctx context.Context, id string) (string, error) {
	lo, hi, err := parseBlock(id)
	if err != nil {
		return "", err
	}
	// The summarizer holds writeMu per summary, so it never builds a parent
	// from a half that's about to change.
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	if n, err := m.s.built(hi - lo); err != nil {
		return "", err
	} else if lo/(hi-lo) >= n {
		return "", fmt.Errorf("#%s has no summary yet", id)
	}
	T, err := m.s.logLen()
	if err != nil {
		return "", err
	}
	var lines []string
	for {
		body, err := m.napBody(lo, hi)
		if err != nil {
			return "", err
		}
		line, err := m.summarize(ctx, napTask(lo, hi, m.entryChars, body))
		if err != nil {
			return "", fmt.Errorf("#%s: %w", blockName(lo, hi), err)
		}
		if err := m.s.treeRewrite(lo, hi, line); err != nil {
			return "", err
		}
		lines = append(lines, fmt.Sprintf("#%s %s", blockName(lo, hi), line))
		// Then its parent, if that's made from summaries and built: a level
		// is built only after the one below it, so above an unbuilt parent
		// there's nothing. A block under rawMax feeds no summary; its
		// ancestors up to rawMax are made from the raw memories.
		size := 2 * (hi - lo)
		if size <= rawMax || size > T {
			break
		}
		n, err := m.s.built(size)
		if err != nil {
			return "", err
		}
		if lo/size >= n {
			break
		}
		lo = lo / size * size
		hi = lo + size
	}
	return "Rewrote:\n" + strings.Join(lines, "\n"), nil
}

// line renders block b as the memory shows it.
func (m *Memory) line(b [2]int) (string, error) {
	if b[1]-b[0] == 1 {
		es, err := m.s.logSlice(b[0], b[1])
		if err != nil {
			return "", err
		}
		return es[0].String(), nil
	}
	text, ok, err := m.s.treeGet(b[0], b[1])
	if err != nil {
		return "", err
	}
	if !ok {
		text = "(not summarized yet: zoom it)"
	}
	return fmt.Sprintf("#%s %s", blockName(b[0], b[1]), text), nil
}

// Wake renders the memory for a new conversation: at most wakeLines lines
// covering every memory, oldest first. It waits, up to Options.Wait, for
// the summaries those lines need.
func (m *Memory) Wake(ctx context.Context, chat string) ([]llm.Message, error) {
	T, err := m.s.logLen()
	if err != nil {
		return nil, err
	}
	today := m.o.Now().Format("Mon 2006-01-02")
	if T == 0 {
		return []llm.Message{{Role: llm.RoleUser, Content: fmt.Sprintf(
			"[steel] Your memory is empty: no memories yet. Record the first with tools.memory.note. This conversation started on %s.", today)}}, nil
	}
	blocks := cover(T, m.wakeLines)
	if err := m.await(ctx, blocks); err != nil {
		return nil, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[steel] Your memory, oldest first (%s). This conversation started on %s.\n<memory>\n", plural(T, "memory", "memories"), today)
	for _, bl := range blocks {
		line, err := m.line(bl)
		if err != nil {
			return nil, err
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("</memory>")
	return []llm.Message{{Role: llm.RoleUser, Content: b.String()}}, nil
}

// await waits until every block is summarized, the summarizer fails, ctx
// ends or Options.Wait passes, whichever comes first.
func (m *Memory) await(ctx context.Context, blocks [][2]int) error {
	timer := time.NewTimer(m.o.Wait)
	defer timer.Stop()
	m.wake()
	for {
		m.mu.Lock()
		ch, stuck := m.progress, m.stuck
		m.mu.Unlock()
		missing := false
		for _, b := range blocks {
			if b[1]-b[0] == 1 {
				continue
			}
			_, ok, err := m.s.treeGet(b[0], b[1])
			if err != nil {
				return err
			}
			if !ok {
				missing = true
				break
			}
		}
		if !missing || stuck {
			return nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return nil // placeholders, not a failed turn
		case <-timer.C:
			return nil
		}
	}
}

// wake asks the summarizer to write whatever summaries are due.
func (m *Memory) wake() {
	m.mu.Lock()
	m.stuck = false
	m.mu.Unlock()
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

// advance announces the summarizer's progress to anyone in await.
func (m *Memory) advance(stuck bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stuck = stuck
	close(m.progress)
	m.progress = make(chan struct{})
}

// summarizer writes due summaries, one at a time and in order (each level
// is built left to right, and a big block from its halves), whenever it's
// woken. A failure ends the round; the next note or wake retries.
func (m *Memory) summarizer(ctx context.Context) {
	defer close(m.done)
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.kick:
		}
		for {
			err := m.summarizeNext(ctx)
			if errors.Is(err, errNothingDue) {
				m.advance(false)
				break
			}
			if err != nil {
				if ctx.Err() == nil {
					m.o.OnError(fmt.Errorf("memory: summarizing: %w", err))
				}
				m.advance(true)
				break
			}
			m.advance(false)
		}
	}
}

var errNothingDue = errors.New("nothing due")

// summarizeNext writes the first due summary.
func (m *Memory) summarizeNext(ctx context.Context) error {
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	T, err := m.s.logLen()
	if err != nil {
		return err
	}
	todo, err := m.s.pending(T, 1)
	if err != nil {
		return err
	}
	if len(todo) == 0 {
		return errNothingDue
	}
	lo, hi := todo[0][0], todo[0][1]
	body, err := m.napBody(lo, hi)
	if err != nil {
		return err
	}
	line, err := m.summarize(ctx, napTask(lo, hi, m.entryChars, body))
	if err != nil {
		return fmt.Errorf("#%s: %w", blockName(lo, hi), err)
	}
	_, err = m.s.treePut(lo, hi, line) // false: memo wrote it meanwhile
	return err
}

// napBody is what block [lo,hi)'s summary is made from: its memories, or
// its halves' summaries once it's big.
func (m *Memory) napBody(lo, hi int) (string, error) {
	var lines []string
	if hi-lo <= rawMax {
		es, err := m.s.logSlice(lo, hi)
		if err != nil {
			return "", err
		}
		for _, e := range es {
			lines = append(lines, "  "+e.String())
		}
		return strings.Join(lines, "\n"), nil
	}
	mid := (lo + hi) / 2
	for _, b := range [][2]int{{lo, mid}, {mid, hi}} {
		text, ok, err := m.s.treeGet(b[0], b[1])
		if err != nil {
			return "", err
		}
		if !ok {
			// pending lists a block only after its halves, so this is a
			// blank record: a corrupt write.
			return "", fmt.Errorf("the summary of #%s is blank", blockName(b[0], b[1]))
		}
		lines = append(lines, fmt.Sprintf("  #%s %s", blockName(b[0], b[1]), text))
	}
	return strings.Join(lines, "\n"), nil
}

// summaryTokens caps a summary call's reply. A summary is one line; a
// model that copies its input instead writes until the provider stops it.
const summaryTokens = 4096

// summarize asks the model for one valid line, telling it what was wrong
// with a bad one. The last try's line is forced into shape, so one stubborn
// block can't stop the tree from growing.
func (m *Memory) summarize(ctx context.Context, task string) (string, error) {
	msgs := []llm.Message{{Role: llm.RoleUser, Content: task}}
	const tries = 3
	for try := 1; ; try++ {
		req := m.o.Model()
		req.Messages, req.MaxTokens = msgs, summaryTokens
		reply, err := m.o.Provider.Stream(ctx, req, func(string) error { return nil })
		if err != nil && !errors.Is(err, llm.ErrTruncated) {
			return "", err
		}
		text := unquote(strings.TrimSpace(reply.Content))
		line, bad := m.check(text)
		if bad == nil {
			return line, nil
		}
		if try == tries {
			line := strings.Join(strings.Fields(text), " ")
			if line == "" {
				return "", fmt.Errorf("no usable line after %d tries: %w", tries, bad)
			}
			return cut(line, m.entryChars), nil
		}
		msgs = append(msgs,
			llm.Message{Role: llm.RoleAssistant, Content: cut(reply.Content, 2*m.entryChars)},
			llm.Message{Role: llm.RoleUser, Content: fmt.Sprintf("That doesn't work: %v. Reply with the line alone: one line of at most %d bytes.", bad, m.entryChars)},
		)
	}
}

// unquote strips one pair of quotes or backticks a model may wrap its line in.
func unquote(s string) string {
	for _, q := range []string{`"`, "`", "'"} {
		if len(s) >= 2 && strings.HasPrefix(s, q) && strings.HasSuffix(s, q) {
			return strings.TrimSpace(s[1 : len(s)-1])
		}
	}
	return s
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
