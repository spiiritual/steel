// Package agent runs conversations between users, the model, and tools. It
// knows nothing about terminals or chat platforms: channels send text into
// Sessions and render the Events that come back.
package agent

import (
	"context"
	"encoding/json"
	"time"

	"github.com/spiiritual/steel/internal/llm"
)

// Event is something that happened in a session.
type Event interface{ event() }

// TextDelta is a fragment of model text as it streams in.
type TextDelta struct{ Text string }

// Reply is a complete assistant text message; it follows its TextDeltas.
type Reply struct{ Text string }

// ToolStart reports a tool call starting. Nested calls are made from inside
// another tool (e.g. a scratchpad script) rather than by the model directly.
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

// TurnDone reports that the session finished processing its inbox. Err is
// nil on success and when the user stopped the turn.
type TurnDone struct{ Err error }

// Notice is a message from steel itself, e.g. a command acknowledgement.
type Notice struct{ Text string }

func (TextDelta) event() {}
func (Reply) event()     {}
func (ToolStart) event() {}
func (ToolEnd) event()   {}
func (TurnDone) event()  {}
func (Notice) event()    {}

// Toolbox is what a session gives the model; in steel, a scratchpad.
type Toolbox interface {
	Defs() []llm.ToolDef
	Call(ctx context.Context, name string, args json.RawMessage) (string, error)
	// Stop halts running work and drops background jobs; once it returns no
	// job result is delivered. It reports whether state was lost doing so.
	Stop() bool
	Reset()     // discard all state
	Busy() bool // background work still pending or being delivered
	Close()
}

// Hooks route a toolbox's asynchronous output back into its session.
type Hooks struct {
	// Emit reports events such as nested tool calls. Safe for concurrent use.
	Emit func(Event)
	// Deliver queues text for the model, e.g. a finished background job.
	// Safe for concurrent use.
	Deliver func(text string)
}

// Config describes how sessions are built.
type Config struct {
	Provider llm.Provider
	Model    string
	System   string // optional system prompt
	// NewToolbox creates a session's toolbox; nil means no tools.
	NewToolbox func(Hooks) Toolbox
	// Dir stores one history file per session; "" keeps history in memory.
	Dir string
	// IdleTimeout closes the toolboxes of sessions idle this long, freeing
	// their memory; 0 never closes them.
	IdleTimeout time.Duration
}
