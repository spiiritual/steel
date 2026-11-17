// Package usage records what each model call cost, as the provider
// reports it, so prompt-cache hit rates can be measured on real use.
package usage

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/spiiritual/steel/internal/llm"
)

// Record is one model call. It's a line of the usage log.
type Record struct {
	Time    time.Time `json:"time"`
	Purpose string    `json:"purpose"` // what the call was for: "turn", "compaction"
	Model   string    `json:"model"`
	Input   int       `json:"input"`
	Cached  int       `json:"cached"` // of Input, read from the prompt cache
	Output  int       `json:"output"`
}

// Log appends records to a file of JSON lines. It's a log, not state: a
// line lost or torn in a crash only drops one call from the totals.
type Log struct {
	mu      sync.Mutex
	path    string
	onError func(error)
	now     func() time.Time
}

// NewLog returns a log at path; onError reports failed writes.
func NewLog(path string, onError func(error)) *Log {
	return &Log{path: path, onError: onError, now: time.Now}
}

// Track returns p, recording each call's usage under purpose.
func (l *Log) Track(p llm.Provider, purpose string) llm.Provider {
	return tracked{p, purpose, l}
}

type tracked struct {
	llm.Provider
	purpose string
	log     *Log
}

func (t tracked) Stream(ctx context.Context, req llm.Request, onDelta func(string) error) (llm.Message, error) {
	msg, err := t.Provider.Stream(ctx, req, onDelta)
	if u := msg.Usage; u != nil {
		t.log.add(Record{Time: t.log.now(), Purpose: t.purpose, Model: req.Model, Input: u.Input, Cached: u.Cached, Output: u.Output})
	}
	return msg, err
}

func (l *Log) add(r Record) {
	b, err := json.Marshal(r)
	if err != nil {
		l.onError(err)
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		l.onError(err)
		return
	}
	_, werr := f.Write(append(b, '\n'))
	if err := errors.Join(werr, f.Close()); err != nil {
		l.onError(err)
	}
}

// Read returns the records at path from since on. Lines that don't parse,
// like one torn by a crash, are skipped. A missing file has no records.
func Read(path string, since time.Time) ([]Record, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Record
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r Record
		if json.Unmarshal(sc.Bytes(), &r) != nil || r.Time.Before(since) {
			continue
		}
		out = append(out, r)
	}
	return out, sc.Err()
}

// Total sums calls with the same purpose and model; Purpose and Model are
// empty in the grand total.
type Total struct {
	Purpose, Model        string
	Calls                 int
	Input, Cached, Output int
}

// HitRate is the share of input tokens read from the cache.
func (t Total) HitRate() float64 {
	if t.Input == 0 {
		return 0
	}
	return float64(t.Cached) / float64(t.Input)
}

// Summarize totals records by purpose and model, in that order, and
// overall.
func Summarize(rs []Record) (groups []Total, all Total) {
	idx := map[[2]string]int{}
	for _, r := range rs {
		k := [2]string{r.Purpose, r.Model}
		i, ok := idx[k]
		if !ok {
			i = len(groups)
			idx[k] = i
			groups = append(groups, Total{Purpose: r.Purpose, Model: r.Model})
		}
		for _, t := range []*Total{&groups[i], &all} {
			t.Calls++
			t.Input += r.Input
			t.Cached += r.Cached
			t.Output += r.Output
		}
	}
	slices.SortFunc(groups, func(a, b Total) int {
		if a.Purpose != b.Purpose {
			if a.Purpose < b.Purpose {
				return 1 // "turn" before "compaction"
			}
			return -1
		}
		if a.Model < b.Model {
			return -1
		}
		if a.Model > b.Model {
			return 1
		}
		return 0
	})
	return groups, all
}
