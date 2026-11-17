package optmem

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spiiritual/steel/internal/llm"
)

// summarizer answers summary requests like a model would: "sum a-b" for
// block a-b. reply can override that per call.
type summarizer struct {
	mu    sync.Mutex
	reqs  []llm.Request
	reply func(i int, task string) (string, error)
}

var taskBlock = regexp.MustCompile(`Compress memories #(\d+-\d+) `)

func (s *summarizer) Stream(_ context.Context, req llm.Request, _ func(string) error) (llm.Message, error) {
	s.mu.Lock()
	i := len(s.reqs)
	s.reqs = append(s.reqs, req)
	reply := s.reply
	s.mu.Unlock()
	task := req.Messages[0].Content
	if reply != nil {
		text, err := reply(i, task)
		return llm.Message{Role: llm.RoleAssistant, Content: text}, err
	}
	return llm.Message{Role: llm.RoleAssistant, Content: "sum " + taskBlock.FindStringSubmatch(task)[1]}, nil
}

func (s *summarizer) requests() []llm.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]llm.Request(nil), s.reqs...)
}

var today = time.Date(2026, 11, 17, 9, 30, 0, 0, time.UTC)

func open(t *testing.T, dir string, p llm.Provider, o Options) *Memory {
	t.Helper()
	o.Provider = p
	o.Now = func() time.Time { return today }
	m, err := Open(dir, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return m
}

func note(t *testing.T, m *Memory, texts ...string) {
	t.Helper()
	for _, s := range texts {
		if _, err := m.Note(s); err != nil {
			t.Fatal(err)
		}
	}
}

func noteN(t *testing.T, m *Memory, n int) {
	t.Helper()
	for i := range n {
		note(t, m, fmt.Sprintf("m%d", i))
	}
}

func wake(t *testing.T, m *Memory) string {
	t.Helper()
	msgs, err := m.Wake(context.Background(), "c")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Role != llm.RoleUser {
		t.Fatalf("wake = %v", msgs)
	}
	return msgs[0].Content
}

// memoryLines is the lines inside <memory>.
func memoryLines(woken string) []string {
	_, in, _ := strings.Cut(woken, "<memory>\n")
	in, _, _ = strings.Cut(in, "</memory>")
	return strings.Split(strings.TrimSuffix(in, "\n"), "\n")
}

func TestNotesAreStoredInOptMemsFormat(t *testing.T) {
	dir := t.TempDir()
	m := open(t, dir, &summarizer{}, Options{})
	id, err := m.Note("  the user's name is Ana  ")
	if err != nil || id != 0 {
		t.Fatalf("Note = %d, %v", id, err)
	}
	note(t, m, "café: 2 bytes for é")
	b, err := os.ReadFile(filepath.Join(dir, "LOG.txt"))
	if err != nil {
		t.Fatal(err)
	}
	want := "#0 2026-11-17 the user's name is Ana"
	want += strings.Repeat(" ", logRec-1-len(want)) + "\n"
	rec1 := "#1 2026-11-17 café: 2 bytes for é"
	want += rec1 + strings.Repeat(" ", logRec-1-len(rec1)) + "\n"
	if string(b) != want {
		t.Errorf("LOG.txt:\n%q\nwant\n%q", b, want)
	}
}

func TestBadNotesAreRefusedWithTheReason(t *testing.T) {
	m := open(t, t.TempDir(), &summarizer{}, Options{})
	for text, reason := range map[string]string{
		"   ":                     "empty",
		"one\ntwo":                "2 lines",
		strings.Repeat("x", 281):  "too long: 281 bytes, limit 280",
		strings.Repeat("é", 141):  "too long: 282 bytes",
		string([]byte{0xff, 'a'}): "UTF-8",
	} {
		if _, err := m.Note(text); err == nil || !strings.Contains(err.Error(), reason) {
			t.Errorf("Note(%q) = %v, want %q", cut(text, 20), err, reason)
		}
	}
	if n, _ := m.s.logLen(); n != 0 {
		t.Errorf("%d memories stored", n)
	}
}

func TestWakeShowsAFewMemoriesWholeWithoutAModel(t *testing.T) {
	s := &summarizer{}
	m := open(t, t.TempDir(), s, Options{})
	if got := wake(t, m); !strings.Contains(got, "memory is empty") || !strings.Contains(got, "Tue 2026-11-17") {
		t.Errorf("empty wake: %s", got)
	}
	note(t, m, "a", "b", "c")
	got := wake(t, m)
	if want := "[steel] Your memory, oldest first (3 memories). This conversation started on Tue 2026-11-17.\n<memory>\n#0 2026-11-17 a\n#1 2026-11-17 b\n#2 2026-11-17 c\n</memory>"; got != want {
		t.Errorf("wake:\n%s\nwant\n%s", got, want)
	}
	// Summaries are still written, in the background, for later wakes and
	// zooms; wake doesn't wait for ones it doesn't show.
	waitFor(t, func() bool { _, ok, _ := m.s.treeGet(0, 2); return ok })
}

func TestWakeCoversEveryMemoryWithSummariesOfOldOnes(t *testing.T) {
	s := &summarizer{}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config"), []byte("# a comment\n WAKE_LINES = 8  # lines\n# ENTRY_CHARS = 100\n"), 0o600)
	m := open(t, dir, s, Options{})
	noteN(t, m, 200)

	got := memoryLines(wake(t, m))
	want := []string{"#0-127 sum 0-127", "#128-159 sum 128-159", "#160-175 sum 160-175", "#176-191 sum 176-191",
		"#192-195 sum 192-195", "#196-197 sum 196-197", "#198 2026-11-17 m198", "#199 2026-11-17 m199"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("wake:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestSummariesAreMadeFromMemoriesThenFromHalves(t *testing.T) {
	s := &summarizer{}
	m := open(t, t.TempDir(), s, Options{})
	noteN(t, m, 32)
	waitFor(t, func() bool { _, ok, _ := m.s.treeGet(0, 32); return ok })
	tasks := map[string]string{}
	for _, r := range s.requests() {
		task := r.Messages[0].Content
		tasks[taskBlock.FindStringSubmatch(task)[1]] = task
		if r.MaxTokens == 0 {
			t.Error("a summary call without MaxTokens")
		}
	}
	if len(tasks) != 31 {
		t.Errorf("%d summaries for 32 memories, want 31", len(tasks))
	}
	if got := tasks["0-15"]; !strings.Contains(got, "at most 280 bytes") || !strings.Contains(got, "\n  #0 2026-11-17 m0\n") || !strings.Contains(got, "  #15 2026-11-17 m15") {
		t.Errorf("16-memory block isn't made from its memories:\n%s", got)
	}
	if got := tasks["0-31"]; !strings.HasSuffix(got, "\n\n  #0-15 sum 0-15\n  #16-31 sum 16-31") {
		t.Errorf("32-memory block isn't made from its halves:\n%s", got)
	}
}

func TestABadSummaryIsRetriedWithTheReasonThenForced(t *testing.T) {
	s := &summarizer{reply: func(i int, task string) (string, error) {
		if strings.HasPrefix(task, "Compress") && i == 0 {
			return "line one\nline two", nil
		}
		return `"` + strings.Repeat("y", 300) + `"`, nil
	}}
	m := open(t, t.TempDir(), s, Options{})
	note(t, m, "a", "b")
	waitFor(t, func() bool { _, ok, _ := m.s.treeGet(0, 2); return ok })
	reqs := s.requests()
	if len(reqs) != 3 {
		t.Fatalf("%d calls, want 3 tries", len(reqs))
	}
	if fb := reqs[1].Messages[2].Content; !strings.Contains(fb, "2 lines") {
		t.Errorf("first feedback: %s", fb)
	}
	if fb := reqs[2].Messages[4].Content; !strings.Contains(fb, "too long: 300 bytes") {
		t.Errorf("second feedback: %s", fb)
	}
	if got, _, _ := m.s.treeGet(0, 2); got != strings.Repeat("y", 280) {
		t.Errorf("forced summary = %q", got)
	}
}

func TestWakeShowsPlaceholdersWhenSummariesFail(t *testing.T) {
	s := &summarizer{reply: func(int, string) (string, error) { return "", errors.New("provider down") }}
	var mu sync.Mutex
	var errs []error
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config"), []byte("WAKE_LINES=2\n"), 0o600)
	m := open(t, dir, s, Options{OnError: func(err error) { mu.Lock(); errs = append(errs, err); mu.Unlock() }})
	noteN(t, m, 4)
	got := memoryLines(wake(t, m))
	if want := "#0-1 (not summarized yet: zoom it)\n#2-3 (not summarized yet: zoom it)"; strings.Join(got, "\n") != want {
		t.Errorf("wake:\n%s", strings.Join(got, "\n"))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(errs) == 0 || !strings.Contains(errs[0].Error(), "provider down") {
		t.Errorf("errors reported: %v", errs)
	}
}

func TestWakeWaitsOnlyAsLongAsOptionsWait(t *testing.T) {
	release := make(chan struct{})
	s := &summarizer{reply: func(int, string) (string, error) { <-release; return "late", nil }}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config"), []byte("WAKE_LINES=1\n"), 0o600)
	m := open(t, dir, s, Options{Wait: 20 * time.Millisecond})
	defer close(release)
	noteN(t, m, 2)
	if got := memoryLines(wake(t, m)); len(got) != 1 || got[0] != "#0-1 (not summarized yet: zoom it)" {
		t.Errorf("wake: %v", got)
	}
}

func TestZoomOpensANodeIntoItsHalves(t *testing.T) {
	m := open(t, t.TempDir(), &summarizer{}, Options{})
	noteN(t, m, 6)
	waitFor(t, func() bool { _, ok, _ := m.s.treeGet(4, 6); return ok })
	waitFor(t, func() bool { _, ok, _ := m.s.treeGet(0, 4); return ok })
	for id, want := range map[string]string{
		"0-3":  "#0-1 sum 0-1\n#2-3 sum 2-3",
		"#0-1": "#0 2026-11-17 m0\n#1 2026-11-17 m1",
		"4-7":  "#4-5 sum 4-5", // 6-7: no memories yet
	} {
		if got, err := m.Zoom(id); err != nil || got != want {
			t.Errorf("Zoom(%s) = %q, %v; want %q", id, got, err, want)
		}
	}
	for id, reason := range map[string]string{"x": "not a block id", "1-2": "not a block", "2-4": "not a block", "8-15": "beyond the memory"} {
		if _, err := m.Zoom(id); err == nil || !strings.Contains(err.Error(), reason) {
			t.Errorf("Zoom(%s) = %v, want %q", id, err, reason)
		}
	}
}

func TestRecallSearchesEveryMemoryAndKeepsTheNewest(t *testing.T) {
	m := open(t, t.TempDir(), &summarizer{}, Options{})
	note(t, m, "Ana likes TEA", "Bob likes coffee", "ana moved to Lisbon")
	if got, _ := m.Recall("ana"); got != "#0 2026-11-17 Ana likes TEA\n#2 2026-11-17 ana moved to Lisbon\n2 matches." {
		t.Errorf("recall ana: %q", got)
	}
	if got, _ := m.Recall("zebra"); got != "No match." {
		t.Errorf("recall zebra: %q", got)
	}
	if _, err := m.Recall("("); err == nil {
		t.Error("bad regex accepted")
	}
	for i := range 100 {
		note(t, m, fmt.Sprintf("filler %03d %s", i, strings.Repeat("z", 250)))
	}
	got, _ := m.Recall("filler")
	if !strings.HasSuffix(got, "of 100 matches. Narrow the regex.") || !strings.Contains(got, "filler 099") || strings.Contains(got, "filler 000") || len(got) > recallMax+100 {
		t.Errorf("capped recall: %d bytes, tail %q", len(got), got[len(got)-60:])
	}
}

func TestAReopenedMemoryRepairsACrashedWriteAndCarriesOn(t *testing.T) {
	dir := t.TempDir()
	s := &summarizer{}
	m := open(t, dir, s, Options{})
	note(t, m, "a", "b", "c")
	m.Close()
	f, _ := os.OpenFile(filepath.Join(dir, "LOG.txt"), os.O_WRONLY|os.O_APPEND, 0)
	f.WriteString("#3 2026-11-17 half writt")
	f.Close()

	m = open(t, dir, s, Options{})
	if id, err := m.Note("d"); err != nil || id != 3 {
		t.Fatalf("Note after crash = %d, %v", id, err)
	}
	if got := memoryLines(wake(t, m)); strings.Join(got, "|") != "#0 2026-11-17 a|#1 2026-11-17 b|#2 2026-11-17 c|#3 2026-11-17 d" {
		t.Errorf("wake: %v", got)
	}
}

func TestBadConfigIsRefused(t *testing.T) {
	for _, cfg := range []string{"WAKE_LINES=0\n", "WAKE_LINES=x\n", "COLOR=3\n"} {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "config"), []byte(cfg), 0o600)
		if m, err := Open(dir, Options{Provider: &summarizer{}}); err == nil {
			m.Close()
			t.Errorf("config %q accepted", cfg)
		}
	}
}

func TestCoverMatchesMemo(t *testing.T) {
	// From memo's own cover(), so both print the same memory.
	for _, c := range []struct {
		T, budget int
		want      string
	}{
		{5, 96, "[[0 1] [1 2] [2 3] [3 4] [4 5]]"},
		{200, 8, "[[0 128] [128 160] [160 176] [176 192] [192 196] [196 198] [198 199] [199 200]]"},
		{1000, 10, "[[0 512] [512 768] [768 896] [896 960] [960 976] [976 992] [992 996] [996 998] [998 999] [999 1000]]"},
	} {
		if got := fmt.Sprint(cover(c.T, c.budget)); got != c.want {
			t.Errorf("cover(%d, %d) = %s\nwant %s", c.T, c.budget, got, c.want)
		}
	}
	for _, c := range []struct{ T, budget, n int }{{97, 96, 96}, {1_000_000, 96, 96}} {
		got := cover(c.T, c.budget)
		if len(got) != c.n || got[0] != [2]int{0, 2} && c.T == 97 || got[len(got)-1] != [2]int{c.T - 1, c.T} {
			t.Errorf("cover(%d, %d): %d blocks, first %v, last %v", c.T, c.budget, len(got), got[0], got[len(got)-1])
		}
	}
	if got := cover(1_000_000, 96); got[0] != [2]int{0, 131072} || got[3] != [2]int{327680, 393216} {
		t.Errorf("cover(1e6, 96) starts %v", got[:4])
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition never held")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRewriteRedoesASummaryAndTheOnesMadeFromIt(t *testing.T) {
	var redo sync.Mutex
	redoing := false
	s := &summarizer{reply: func(_ int, task string) (string, error) {
		redo.Lock()
		defer redo.Unlock()
		b := taskBlock.FindStringSubmatch(task)[1]
		if redoing {
			return "new " + b, nil
		}
		return "sum " + b, nil
	}}
	m := open(t, t.TempDir(), s, Options{})
	noteN(t, m, 64)
	waitFor(t, func() bool { _, ok, _ := m.s.treeGet(0, 64); return ok })
	redo.Lock()
	redoing = true
	redo.Unlock()

	// A small block feeds no other summary: its ancestors up to rawMax are
	// made from the raw memories.
	before := len(s.requests())
	if got, err := m.Rewrite(context.Background(), "2-3"); err != nil || got != "Rewrote:\n#2-3 new 2-3" {
		t.Errorf("Rewrite(2-3) = %q, %v", got, err)
	}
	if n := len(s.requests()) - before; n != 1 {
		t.Errorf("Rewrite(2-3): %d model calls, want 1", n)
	}

	before = len(s.requests())
	got, err := m.Rewrite(context.Background(), "16-31")
	if err != nil {
		t.Fatal(err)
	}
	if want := "Rewrote:\n#16-31 new 16-31\n#0-31 new 0-31\n#0-63 new 0-63"; got != want {
		t.Errorf("Rewrite(16-31):\n%s\nwant\n%s", got, want)
	}
	if n := len(s.requests()) - before; n != 3 {
		t.Errorf("Rewrite(16-31): %d model calls, want 3", n)
	}
	last := s.requests()[len(s.requests())-1].Messages[0].Content
	if !strings.HasSuffix(last, "#0-31 new 0-31\n  #32-63 sum 32-63") {
		t.Errorf("#0-63 not made from the rewritten #0-31:\n%s", last)
	}
	// Everything else keeps its summary.
	for _, b := range [][2]int{{0, 2}, {0, 4}, {0, 16}, {16, 24}, {32, 64}} {
		if text, _, _ := m.s.treeGet(b[0], b[1]); !strings.HasPrefix(text, "sum ") {
			t.Errorf("#%s = %q, want untouched", blockName(b[0], b[1]), text)
		}
	}
}

func TestRewriteFixesABlankSummaryAndRefusesAnUnbuiltOne(t *testing.T) {
	dir := t.TempDir()
	m := open(t, dir, &summarizer{}, Options{})
	noteN(t, m, 3)
	waitFor(t, func() bool { _, ok, _ := m.s.treeGet(0, 2); return ok })
	// A corrupt write: the record is there, blank.
	os.WriteFile(filepath.Join(dir, "TREE", "2"), []byte(strings.Repeat(" ", treeRec-1)+"\n"), 0o600)
	if z, _ := m.Zoom("0-3"); !strings.Contains(z, "#0-1 (not summarized yet") {
		t.Fatalf("blank record shows as %q", z)
	}
	if got, err := m.Rewrite(context.Background(), "0-1"); err != nil || got != "Rewrote:\n#0-1 sum 0-1" {
		t.Errorf("Rewrite(0-1) = %q, %v", got, err)
	}
	if _, err := m.Rewrite(context.Background(), "2-3"); err == nil || !strings.Contains(err.Error(), "no summary yet") {
		t.Errorf("Rewrite(2-3) = %v, want no summary yet", err)
	}
}
