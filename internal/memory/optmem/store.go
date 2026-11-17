package optmem

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unicode"
)

// The store is OptMem's, byte for byte (github.com/VictorTaelin/OptMem), so
// steel and OptMem's memo tool can share a memory directory:
//
//	LOG.txt      every memory, append-only: memory i is the record at i*logRec
//	TREE/<size>  the summaries of aligned blocks of size memories: block
//	             [lo,lo+size) is the record at (lo/size)*treeRec
//	.lock        flock'd by every writer, for one write
//
// Records are fixed width, space padded, newline terminated, so position
// is identity and every lookup is one seek.
const (
	logRec  = 320
	treeRec = 288
)

// rawMax is the largest block summarized from the raw log; bigger ones are
// summarized from their two halves' summaries.
const rawMax = 16

// entry is one memory.
type entry struct {
	id         int
	date, text string
}

func (e entry) String() string { return fmt.Sprintf("#%d %s %s", e.id, e.date, e.text) }

type store struct{ dir string }

func (s store) logPath() string             { return filepath.Join(s.dir, "LOG.txt") }
func (s store) treePath(size int) string    { return filepath.Join(s.dir, "TREE", strconv.Itoa(size)) }
func blockName(lo, hi int) string           { return fmt.Sprintf("%d-%d", lo, hi-1) }
func (s store) logLen() (int, error)        { return count(s.logPath(), logRec) }
func (s store) built(size int) (int, error) { return count(s.treePath(size), treeRec) }

// count is how many whole records the file at path holds; a missing file
// holds none.
func count(path string, rec int) (int, error) {
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return int(fi.Size()) / rec, nil
}

func parse(rec []byte) (entry, error) {
	line := strings.TrimRightFunc(string(rec), unicode.IsSpace)
	head, rest, _ := strings.Cut(line, " ")
	date, text, _ := strings.Cut(rest, " ")
	id, err := strconv.Atoi(strings.TrimPrefix(head, "#"))
	if err != nil || !strings.HasPrefix(head, "#") {
		return entry{}, fmt.Errorf("corrupt memory record %q", cut(line, 40))
	}
	return entry{id, date, text}, nil
}

// logSlice reads memories [lo,hi) in one read.
func (s store) logSlice(lo, hi int) ([]entry, error) {
	f, err := os.Open(s.logPath())
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, (hi-lo)*logRec)
	if _, err := f.ReadAt(buf, int64(lo*logRec)); err != nil {
		return nil, err
	}
	out := make([]entry, 0, hi-lo)
	for i := range hi - lo {
		e, err := parse(buf[i*logRec : (i+1)*logRec])
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// scan calls each with every memory, oldest first, without holding the log.
func (s store) scan(each func(entry)) error {
	f, err := os.Open(s.logPath())
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, logRec*1024)
	rec := make([]byte, logRec)
	for {
		if _, err := io.ReadFull(r, rec); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil // a partial record is a crashed write, never acknowledged
			}
			return err
		}
		e, err := parse(rec)
		if err != nil {
			return err
		}
		each(e)
	}
}

// treeGet is the summary of block [lo,hi), and whether it's built. A blank
// record (a corrupt write) counts as not built.
func (s store) treeGet(lo, hi int) (string, bool, error) {
	size := hi - lo
	f, err := os.Open(s.treePath(size))
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	rec := make([]byte, treeRec)
	if _, err := f.ReadAt(rec, int64(lo/size*treeRec)); err != nil {
		if errors.Is(err, io.EOF) {
			return "", false, nil
		}
		return "", false, err
	}
	text := strings.TrimRightFunc(string(rec), unicode.IsSpace)
	return text, text != "", nil
}

func pad(text string, rec int) ([]byte, error) {
	if len(text) > rec-1 {
		return nil, fmt.Errorf("too long: %d bytes; the record holds %d", len(text), rec-1)
	}
	return []byte(text + strings.Repeat(" ", rec-1-len(text)) + "\n"), nil
}

// lock takes the store's write lock, as memo does: blocking, one write at
// a time across every process that shares the directory.
func (s store) lock() (unlock func(), err error) {
	f, err := os.OpenFile(filepath.Join(s.dir, ".lock"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}

// repair drops a partial trailing record left by a crash. It was never
// acknowledged, and the next append would land misaligned. Callers hold
// the lock.
func repair(path string, rec int) error {
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if n := fi.Size(); n%int64(rec) != 0 {
		return os.Truncate(path, n-n%int64(rec))
	}
	return nil
}

// appendRecord writes one record to the end of path, durably.
func appendRecord(path string, rec []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(rec); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// logAppend records one memory and returns its id, assigned under the lock
// so two writers never share one.
func (s store) logAppend(date, text string) (int, error) {
	unlock, err := s.lock()
	if err != nil {
		return 0, err
	}
	defer unlock()
	if err := repair(s.logPath(), logRec); err != nil {
		return 0, err
	}
	id, err := s.logLen()
	if err != nil {
		return 0, err
	}
	rec, err := pad(entry{id, date, text}.String(), logRec)
	if err != nil {
		return 0, err
	}
	return id, appendRecord(s.logPath(), rec)
}

// treePut writes block [lo,hi)'s summary. Each level is built in order, so
// this only appends; it reports false if the block was built (or its level
// truncated) meanwhile, e.g. by memo.
func (s store) treePut(lo, hi int, text string) (bool, error) {
	size := hi - lo
	unlock, err := s.lock()
	if err != nil {
		return false, err
	}
	defer unlock()
	p := s.treePath(size)
	if err := repair(p, treeRec); err != nil {
		return false, err
	}
	if n, err := count(p, treeRec); err != nil || n != lo/size {
		return false, err
	}
	rec, err := pad(text, treeRec)
	if err != nil {
		return false, err
	}
	return true, appendRecord(p, rec)
}

// treeRewrite replaces block [lo,hi)'s summary, which must be built (or
// blank), in place: later blocks at its level stay as they are.
func (s store) treeRewrite(lo, hi int, text string) error {
	size := hi - lo
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	p := s.treePath(size)
	if n, err := count(p, treeRec); err != nil {
		return err
	} else if lo/size >= n {
		return fmt.Errorf("#%s has no summary to rewrite", blockName(lo, hi))
	}
	rec, err := pad(text, treeRec)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if _, err := f.WriteAt(rec, int64(lo/size*treeRec)); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// pending is the blocks of the first T memories not summarized yet,
// smallest first, at most limit of them (0: all). A level file is a dense
// prefix, so its length says how far that level got.
func (s store) pending(T, limit int) ([][2]int, error) {
	var todo [][2]int
	for size := 2; size <= T; size *= 2 {
		have, err := s.built(size)
		if err != nil {
			return nil, err
		}
		for k := have; k < T/size; k++ {
			todo = append(todo, [2]int{k * size, (k + 1) * size})
			if limit > 0 && len(todo) >= limit {
				return todo, nil
			}
		}
	}
	return todo, nil
}

// cover tiles the first T memories with at most budget aligned blocks,
// finest near T: recent memories stay whole and old ones collapse. It is
// memo's, so both print the same memory.
func cover(T, budget int) [][2]int {
	if T <= 0 {
		return nil
	}
	if T <= budget {
		out := make([][2]int, T)
		for i := range out {
			out[i] = [2]int{i, i + 1}
		}
		return out
	}
	lo, hi := 0.0, 1.0
	for range 60 {
		mid := (lo + hi) / 2
		if len(coverAt(T, mid)) > budget {
			lo = mid
		} else {
			hi = mid
		}
	}
	out := coverAt(T, hi)
	// Block sizes jump in powers of two, so alpha alone can undershoot the
	// budget: spend the rest on the present, where detail is worth most.
	for len(out) < budget {
		i := -1
		for j, b := range out {
			if b[1]-b[0] > 1 {
				i = j
			}
		}
		if i < 0 {
			break
		}
		b := out[i]
		mid := (b[0] + b[1]) / 2
		out = append(out[:i], append([][2]int{{b[0], mid}, {mid, b[1]}}, out[i+1:]...)...)
	}
	return out
}

// coverAt keeps a block whole iff its size is at most alpha times its age.
func coverAt(T int, alpha float64) [][2]int {
	root := 1
	for root < T {
		root *= 2
	}
	var out [][2]int
	var walk func(lo, hi int)
	walk = func(lo, hi int) {
		if lo >= T {
			return
		}
		size := hi - lo
		if size > 1 && (hi > T || float64(size) > alpha*float64(T-lo)) {
			mid := (lo + hi) / 2
			walk(lo, mid)
			walk(mid, hi)
			return
		}
		out = append(out, [2]int{lo, hi})
	}
	walk(0, root)
	return out
}

// cut returns s's first n bytes or fewer, ending on a rune boundary.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
