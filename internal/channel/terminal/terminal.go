// Package terminal is a line-based channel for talking to steel locally.
package terminal

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/spiiritual/steel/internal/agent"
)

// ChatID is the session ID the terminal uses.
const ChatID = "terminal"

// Terminal reads user lines from in and renders session events to out.
type Terminal struct {
	in  io.Reader
	mu  sync.Mutex
	out io.Writer
	// midLine is true when the last write didn't end with a newline.
	midLine bool
}

// New returns a Terminal on the given streams.
func New(in io.Reader, out io.Writer) *Terminal {
	return &Terminal{in: in, out: out}
}

// Emit renders one event. It is safe for concurrent use.
func (t *Terminal) Emit(_ string, e agent.Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch e := e.(type) {
	case agent.TextDelta:
		t.write(e.Text)
	case agent.Reply:
		t.newline()
	case agent.ToolStart:
		t.newline()
		if e.Nested {
			t.write(fmt.Sprintf("  → %s %s\n", e.Name, e.Args))
			return
		}
		var args struct {
			Code string `json:"code"`
		}
		if json.Unmarshal(e.Args, &args) == nil && args.Code != "" {
			t.write("┌ " + e.Name + "\n")
			for _, l := range strings.Split(strings.TrimRight(args.Code, "\n"), "\n") {
				t.write("│ " + l + "\n")
			}
			t.write("└\n")
			return
		}
		t.write(fmt.Sprintf("→ %s %s\n", e.Name, e.Args))
	case agent.ToolEnd:
		t.newline()
		indent := ""
		if e.Nested {
			indent = "    "
		}
		if e.Err != nil {
			t.write(indent + "← error: " + summarize(e.Err.Error()) + "\n")
			return
		}
		t.write(indent + "← " + summarize(e.Result) + "\n")
	case agent.Notice:
		t.newline()
		t.write("· " + e.Text + "\n")
		t.write("> ")
	case agent.TurnDone:
		t.newline()
		if e.Err != nil {
			t.write("error: " + e.Err.Error() + "\n")
		}
		t.write("> ")
	}
}

// Run sends each input line to the terminal's session until ctx is
// cancelled or input ends. At end of input it waits for the session to go
// idle, including background jobs, so piped input sees every result.
func (t *Terminal) Run(ctx context.Context, ss *agent.Sessions) error {
	lines := make(chan string)
	readErr := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(t.in)
		sc.Buffer(make([]byte, 0, 64<<10), 4<<20) // pasted text can exceed the 64KB default
		for sc.Scan() {
			select {
			case lines <- sc.Text():
			case <-ctx.Done():
				return
			}
		}
		readErr <- sc.Err()
	}()

	t.mu.Lock()
	t.write("> ")
	t.mu.Unlock()
	for {
		select {
		case <-ctx.Done():
			t.endLine()
			return nil
		case err := <-readErr:
			if err != nil {
				return err
			}
			for !ss.Idle(ChatID) {
				select {
				case <-ctx.Done():
					t.endLine()
					return nil
				case <-time.After(100 * time.Millisecond):
				}
			}
			t.endLine()
			return nil
		case line := <-lines:
			t.mu.Lock()
			t.midLine = false // the user's Enter already moved to a new line
			t.mu.Unlock()
			if line = strings.TrimSpace(line); line != "" {
				ss.Send(ChatID, line)
			}
		}
	}
}

// write and newline require t.mu.
func (t *Terminal) write(s string) {
	if s == "" {
		return
	}
	io.WriteString(t.out, s)
	t.midLine = !strings.HasSuffix(s, "\n")
}

func (t *Terminal) newline() {
	if t.midLine {
		t.write("\n")
	}
}

func (t *Terminal) endLine() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.newline()
}

// summarize renders a result as one short line: section labels like
// "[result]" are dropped and whitespace is collapsed.
func summarize(s string) string {
	s = strings.TrimSpace(s)
	var kept []string
	for _, l := range strings.Split(s, "\n") {
		if !(strings.HasPrefix(l, "[") && strings.HasSuffix(l, "]")) {
			kept = append(kept, l)
		}
	}
	line := strings.Join(strings.Fields(strings.Join(kept, " ")), " ")
	if len(line) > 100 {
		line = line[:100] + "…"
	}
	if line != s {
		return fmt.Sprintf("%s (%d bytes)", line, len(s))
	}
	return line
}
