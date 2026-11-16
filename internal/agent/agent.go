// Package agent contains the core agent loop.
package agent

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spiiritual/steel/internal/llm"
	"github.com/spiiritual/steel/internal/tool"
)

// Config wires an Agent to a model, its tools, and terminal streams.
type Config struct {
	Provider llm.Provider
	Model    string
	System   string         // optional system prompt
	Tools    *tool.Registry // optional; nil means no tools
	In       io.Reader
	Out      io.Writer
}

// Agent drives the conversation loop between the user, the model, and tools.
type Agent struct {
	cfg     Config
	history []llm.Message
}

// New returns a ready-to-run Agent.
func New(cfg Config) *Agent {
	a := &Agent{cfg: cfg}
	if cfg.System != "" {
		a.history = append(a.history, llm.Message{Role: llm.RoleSystem, Content: cfg.System})
	}
	return a
}

// Run reads user turns until EOF or ctx is cancelled, streaming each reply.
func (a *Agent) Run(ctx context.Context) error {
	lines := make(chan string)
	readErr := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(a.cfg.In)
		for sc.Scan() {
			select {
			case lines <- sc.Text():
			case <-ctx.Done():
				return
			}
		}
		readErr <- sc.Err()
	}()

	out := a.cfg.Out
	for {
		fmt.Fprint(out, "> ")
		var line string
		select {
		case <-ctx.Done():
			fmt.Fprintln(out)
			return nil
		case err := <-readErr:
			fmt.Fprintln(out)
			return err
		case line = <-lines:
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		if err := a.Turn(ctx, line); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			fmt.Fprintln(out, "error:", err)
		}
	}
}

// Turn handles one user message: it calls the model, runs any requested
// tools, feeds results back, and repeats until the model replies without
// tool calls. On error, history is rolled back to before the user message.
func (a *Agent) Turn(ctx context.Context, input string) error {
	start := len(a.history)
	a.history = append(a.history, llm.Message{Role: llm.RoleUser, Content: input})

	var defs []llm.ToolDef
	if a.cfg.Tools != nil {
		defs = a.cfg.Tools.Defs()
	}

	out := a.cfg.Out
	for {
		reply, err := a.cfg.Provider.Stream(ctx, llm.Request{Model: a.cfg.Model, Messages: a.history, Tools: defs}, func(d string) error {
			_, err := io.WriteString(out, d)
			return err
		})
		if reply.Content != "" || err != nil {
			fmt.Fprintln(out)
		}
		if err != nil {
			a.history = a.history[:start]
			return err
		}
		a.history = append(a.history, reply)
		if len(reply.ToolCalls) == 0 {
			return nil
		}

		for _, call := range reply.ToolCalls {
			fmt.Fprintf(out, "→ %s %s\n", call.Name, call.Arguments)
			result, err := a.callTool(ctx, call)
			if err != nil {
				result = "error: " + err.Error()
			}
			fmt.Fprintf(out, "  %s\n", summarize(result))
			a.history = append(a.history, llm.Message{Role: llm.RoleTool, ToolCallID: call.ID, Content: result})
		}
	}
}

func (a *Agent) callTool(ctx context.Context, call llm.ToolCall) (string, error) {
	if a.cfg.Tools == nil {
		return "", fmt.Errorf("no tools available")
	}
	return a.cfg.Tools.Call(ctx, call.Name, call.Arguments)
}

// summarize renders a tool result as one short line for the terminal.
func summarize(s string) string {
	t := strings.TrimSpace(s)
	first, _, _ := strings.Cut(t, "\n")
	if len(first) > 100 {
		first = first[:100] + "…"
	}
	if first != t {
		return fmt.Sprintf("← %s (%d bytes)", first, len(s))
	}
	return "← " + first
}
