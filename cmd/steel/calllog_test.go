package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spiiritual/steel/internal/llm"
)

// syncBuffer is a bytes.Buffer safe to write from the ticker goroutine
// while the test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// gatedProvider answers once release is closed.
type gatedProvider struct {
	release chan struct{}
	msg     llm.Message
	err     error
}

func (g gatedProvider) Stream(ctx context.Context, _ llm.Request, _ func(string) error) (llm.Message, error) {
	<-g.release
	return g.msg, g.err
}

// A stalled call shows up in the log while it runs, and every call ends
// with one line saying how long it took and what it cost or why it failed.
func TestLogCallsReportsSlowAndFinishedCalls(t *testing.T) {
	var out syncBuffer
	release := make(chan struct{})
	p := logCalls(gatedProvider{
		release: release,
		msg:     llm.Message{Usage: &llm.Usage{Input: 2966, Cached: 2816, Output: 79}},
	}, "turn", &out).(*callLog)
	p.every = 5 * time.Millisecond

	done := make(chan struct{})
	go func() {
		p.Stream(context.Background(), llm.Request{Model: "m"}, nil)
		close(done)
	}()
	for !strings.Contains(out.String(), "turn call to m still running after") {
		time.Sleep(time.Millisecond)
	}
	close(release)
	<-done
	if got := out.String(); !strings.Contains(got, "2966 tokens in (2816 cached), 79 out\n") {
		t.Errorf("finished call: log %q lacks its token counts", got)
	}

	var failed syncBuffer
	release = make(chan struct{})
	close(release)
	logCalls(gatedProvider{release: release, err: errors.New("502 Bad Gateway")}, "compaction", &failed).
		Stream(context.Background(), llm.Request{Model: "m"}, nil)
	if got := failed.String(); !strings.Contains(got, "compaction call to m failed after") || !strings.Contains(got, "502 Bad Gateway") {
		t.Errorf("failed call: got log %q", got)
	}
}
