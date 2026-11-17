package usage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spiiritual/steel/internal/llm"
)

type provider struct{ usage *llm.Usage }

func (p provider) Stream(context.Context, llm.Request, func(string) error) (llm.Message, error) {
	return llm.Message{Role: llm.RoleAssistant, Usage: p.usage}, nil
}

func TestTrackedCallsAreTotaledByPurposeAndModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	l := NewLog(path, func(err error) { t.Error(err) })
	turn := l.Track(provider{&llm.Usage{Input: 1000, Cached: 900, Output: 10}}, "turn")
	compact := l.Track(provider{&llm.Usage{Input: 500, Cached: 0, Output: 5}}, "compaction")
	silent := l.Track(provider{}, "turn") // a provider that reports no usage
	ctx := context.Background()
	for _, c := range []struct {
		p     llm.Provider
		model string
	}{{turn, "big"}, {turn, "big"}, {compact, "cheap"}, {silent, "big"}} {
		if _, err := c.p.Stream(ctx, llm.Request{Model: c.model}, nil); err != nil {
			t.Fatal(err)
		}
	}
	// A line torn by a crash is skipped, not fatal.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"time":"2026-`)
	f.Close()

	rs, err := Read(path, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	groups, all := Summarize(rs)
	want := []Total{
		{Purpose: "turn", Model: "big", Calls: 2, Input: 2000, Cached: 1800, Output: 20},
		{Purpose: "compaction", Model: "cheap", Calls: 1, Input: 500, Output: 5},
	}
	if len(groups) != 2 || groups[0] != want[0] || groups[1] != want[1] {
		t.Errorf("groups = %+v", groups)
	}
	if all.Calls != 3 || all.Input != 2500 || all.Cached != 1800 || all.HitRate() != 0.72 {
		t.Errorf("all = %+v, hit rate %v", all, all.HitRate())
	}
	if rs, _ := Read(path, time.Now().Add(time.Hour)); len(rs) != 0 {
		t.Errorf("since the future: %d records", len(rs))
	}
}
