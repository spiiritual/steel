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
	"syscall"

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
	baseURL := flag.String("base-url", cmp.Or(os.Getenv("OPENAI_BASE_URL"), openai.DefaultBaseURL), "OpenAI-compatible API base URL (env OPENAI_BASE_URL)")
	model := flag.String("model", os.Getenv("OPENAI_MODEL"), "model name (env OPENAI_MODEL)")
	system := flag.String("system", "", "system prompt")
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

	term := terminal.New(os.Stdin, os.Stdout)
	pad := scratchpad.New(tool.NewRegistry(tool.ReadFile(cwd), tool.Bash(cwd)), scratchpad.Options{
		OnCall: func(e scratchpad.CallEvent) {
			if !e.Done {
				term.Emit(agent.ToolStart{Name: e.Name, Args: e.Args, Nested: true})
				return
			}
			term.Emit(agent.ToolEnd{Name: e.Name, Result: e.Result, Err: e.Err, Nested: true})
		},
	})
	defer pad.Close()

	return term.Run(ctx, agent.New(agent.Config{
		Provider: openai.New(*baseURL, os.Getenv("OPENAI_API_KEY")),
		Model:    *model,
		System:   *system,
		Tools:    pad,
		Emit:     term.Emit,
	}))
}
