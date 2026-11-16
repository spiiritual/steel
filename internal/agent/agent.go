// Package agent contains the core agent loop.
package agent

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spiiritual/steel/internal/llm"
)

// Config wires an Agent to a model and terminal streams.
type Config struct {
	Provider llm.Provider
	Model    string
	System   string // optional system prompt
	In       io.Reader
	Out      io.Writer
}

// Agent drives the conversation loop between the user and the model.
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

		a.history = append(a.history, llm.Message{Role: llm.RoleUser, Content: line})
		reply, err := a.cfg.Provider.Stream(ctx, llm.Request{Model: a.cfg.Model, Messages: a.history}, func(d string) error {
			_, err := io.WriteString(out, d)
			return err
		})
		fmt.Fprintln(out)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// Drop the unanswered turn so history stays alternating.
			a.history = a.history[:len(a.history)-1]
			fmt.Fprintln(out, "error:", err)
			continue
		}
		a.history = append(a.history, reply)
	}
}
