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
	"github.com/spiiritual/steel/internal/llm/openai"
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return agent.New(agent.Config{
		Provider: openai.New(*baseURL, os.Getenv("OPENAI_API_KEY")),
		Model:    *model,
		System:   *system,
		In:       os.Stdin,
		Out:      os.Stdout,
	}).Run(ctx)
}
