// Package agent contains the core agent loop. It knows nothing about
// terminals or chat platforms: it reports what happens as Events, and
// channels render them.
package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spiiritual/steel/internal/llm"
)

// Event is something that happened during a turn.
type Event interface{ event() }

// TextDelta is a fragment of model text as it streams in.
type TextDelta struct{ Text string }

// Reply is a complete assistant text message; it follows its TextDeltas.
type Reply struct{ Text string }

// ToolStart reports a tool call starting. Nested calls are made from inside
// another tool (e.g. a scratchpad cell) rather than by the model directly.
type ToolStart struct {
	ID     string
	Name   string
	Args   json.RawMessage
	Nested bool
}

// ToolEnd reports a tool call finishing.
type ToolEnd struct {
	ID     string
	Name   string
	Result string
	Err    error
	Nested bool
}

func (TextDelta) event() {}
func (Reply) event()     {}
func (ToolStart) event() {}
func (ToolEnd) event()   {}

// Toolbox is the set of tools the model can call directly. In steel that is
// the scratchpad, which offers only run_code.
type Toolbox interface {
	Defs() []llm.ToolDef
	Call(ctx context.Context, name string, args json.RawMessage) (string, error)
}

// Config wires an Agent to a model and its tools.
type Config struct {
	Provider llm.Provider
	Model    string
	System   string  // optional system prompt
	Tools    Toolbox // tools advertised to the model; nil means none
	// Emit receives events. It may be called from multiple goroutines.
	// Nil discards events.
	Emit func(Event)
}

// Agent holds one conversation and runs turns against the model.
type Agent struct {
	cfg     Config
	history []llm.Message
}

// New returns a ready-to-run Agent.
func New(cfg Config) *Agent {
	if cfg.Emit == nil {
		cfg.Emit = func(Event) {}
	}
	a := &Agent{cfg: cfg}
	if cfg.System != "" {
		a.history = append(a.history, llm.Message{Role: llm.RoleSystem, Content: cfg.System})
	}
	return a
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

	for {
		reply, err := a.cfg.Provider.Stream(ctx, llm.Request{Model: a.cfg.Model, Messages: a.history, Tools: defs}, func(d string) error {
			a.cfg.Emit(TextDelta{Text: d})
			return nil
		})
		if err != nil {
			a.history = a.history[:start]
			return err
		}
		if reply.Content != "" {
			a.cfg.Emit(Reply{Text: reply.Content})
		}
		a.history = append(a.history, reply)
		if len(reply.ToolCalls) == 0 {
			return nil
		}

		for _, call := range reply.ToolCalls {
			a.cfg.Emit(ToolStart{ID: call.ID, Name: call.Name, Args: call.Arguments})
			result, err := a.callTool(ctx, call)
			a.cfg.Emit(ToolEnd{ID: call.ID, Name: call.Name, Result: result, Err: err})
			if err != nil {
				result = "error: " + err.Error()
			}
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
