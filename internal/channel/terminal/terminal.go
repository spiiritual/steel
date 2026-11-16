// Package terminal is a line-based channel for talking to the agent locally.
package terminal

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/spiiritual/steel/internal/agent"
)

// Terminal reads user lines from in and renders agent events to out.
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
func (t *Terminal) Emit(e agent.Event) {
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
	}
}

// Run reads lines until EOF or ctx is cancelled, running one turn per line.
func (t *Terminal) Run(ctx context.Context, a *agent.Agent) error {
	lines := make(chan string)
	readErr := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(t.in)
		for sc.Scan() {
			select {
			case lines <- sc.Text():
			case <-ctx.Done():
				return
			}
		}
		readErr <- sc.Err()
	}()

	for {
		t.prompt()
		var line string
		select {
		case <-ctx.Done():
			t.endLine()
			return nil
		case err := <-readErr:
			t.endLine()
			return err
		case line = <-lines:
			// The user's Enter already moved the cursor to a new line.
			t.mu.Lock()
			t.midLine = false
			t.mu.Unlock()
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if err := a.Turn(ctx, line); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			t.mu.Lock()
			t.newline()
			t.write("error: " + err.Error() + "\n")
			t.mu.Unlock()
		}
	}
}

func (t *Terminal) prompt() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.newline()
	t.write("> ")
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

func (t *Terminal) endLine() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.newline()
}
