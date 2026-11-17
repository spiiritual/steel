package main

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/spiiritual/steel/internal/llm"
)

// slowCall is how long a model call runs before the log says it's still
// waiting, and how often it says so again. Without it a stalled provider
// looks exactly like an idle steel.
const slowCall = 30 * time.Second

// logCalls returns p, writing a line to w when each call ends, and every
// slowCall while one runs, so `docker compose logs` shows where time goes.
func logCalls(p llm.Provider, purpose string, w io.Writer) llm.Provider {
	return &callLog{Provider: p, purpose: purpose, w: w, every: slowCall, now: time.Now}
}

type callLog struct {
	llm.Provider
	purpose string
	mu      sync.Mutex // one line at a time
	w       io.Writer
	every   time.Duration
	now     func() time.Time
}

func (c *callLog) Stream(ctx context.Context, req llm.Request, onDelta func(string) error) (llm.Message, error) {
	start := c.now()
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(c.every)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				c.printf("steel: %s call to %s still running after %s\n", c.purpose, req.Model, c.since(start))
			}
		}
	}()
	msg, err := c.Provider.Stream(ctx, req, onDelta)
	close(done)

	took := c.since(start)
	tokens := ""
	if u := msg.Usage; u != nil {
		tokens = fmt.Sprintf(", %d tokens in (%d cached), %d out", u.Input, u.Cached, u.Output)
	}
	if err != nil {
		c.printf("steel: %s call to %s failed after %s%s: %v\n", c.purpose, req.Model, took, tokens, err)
	} else {
		c.printf("steel: %s call to %s: %s%s\n", c.purpose, req.Model, took, tokens)
	}
	return msg, err
}

func (c *callLog) since(t time.Time) time.Duration {
	return c.now().Sub(t).Round(100 * time.Millisecond)
}

func (c *callLog) printf(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintf(c.w, format, args...)
}
