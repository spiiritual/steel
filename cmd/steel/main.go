// Command steel is a personal agent harness.
package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spiiritual/steel/internal/agent"
	"github.com/spiiritual/steel/internal/channel/terminal"
	"github.com/spiiritual/steel/internal/llm/openai"
	"github.com/spiiritual/steel/internal/scratchpad"
	"github.com/spiiritual/steel/internal/tool"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "steel:", err)
		os.Exit(1)
	}
}

func run() error {
	home, _ := os.UserHomeDir()
	baseURL := flag.String("base-url", cmp.Or(os.Getenv("OPENAI_BASE_URL"), openai.DefaultBaseURL), "OpenAI-compatible API base URL (env OPENAI_BASE_URL)")
	model := flag.String("model", os.Getenv("OPENAI_MODEL"), "model name (env OPENAI_MODEL)")
	system := flag.String("system", "", "system prompt")
	dataDir := flag.String("data", filepath.Join(home, ".steel", "sessions"), "directory for saved conversation history")
	idle := flag.Duration("idle", 30*time.Minute, "close a session's scratchpad after this much inactivity")
	flag.Parse()

	if *model == "" {
		return errors.New("no model: pass -model or set OPENAI_MODEL")
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	tools := tool.NewRegistry(tool.ReadFile(cwd), tool.Bash(cwd))
	term := terminal.New(os.Stdin, os.Stdout)
	sessions, err := agent.NewSessions(agent.Config{
		Provider:    openai.New(*baseURL, os.Getenv("OPENAI_API_KEY")),
		Model:       *model,
		System:      *system,
		NewToolbox:  func(h agent.Hooks) agent.Toolbox { return newPad(tools, h) },
		Dir:         *dataDir,
		IdleTimeout: *idle,
	}, term.Emit)
	if err != nil {
		return err
	}
	defer sessions.Close()

	return term.Run(ctx, sessions)
}

// newPad builds a session's scratchpad, routing its nested tool calls and
// finished background jobs back into the session.
func newPad(tools *tool.Registry, h agent.Hooks) *scratchpad.Pad {
	return scratchpad.New(tools, scratchpad.Options{
		OnCall: func(e scratchpad.CallEvent) {
			if !e.Done {
				h.Emit(agent.ToolStart{Name: e.Name, Args: e.Args, Nested: true})
				return
			}
			h.Emit(agent.ToolEnd{Name: e.Name, Result: e.Result, Err: e.Err, Nested: true})
		},
		OnJobDone: func(id string, r scratchpad.Result) {
			h.Deliver(fmt.Sprintf("[job %s finished]\n%s", id, r))
		},
	})
}
