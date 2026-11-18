// Package agent runs conversations between users, the model, and tools. It
// knows nothing about terminals or chat platforms: channels send text into
// Sessions and render the Events that come back.
package agent

import (
	"context"
	"encoding/json"
	"time"

	"github.com/spiiritual/steel/internal/llm"
	"github.com/spiiritual/steel/internal/memory"
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

// ApprovalRequest asks the user whether a tool may do something. Channels
// show it and pass the answer to Sessions.Answer; an ApprovalDone with the
// same ID follows however it ends.
type ApprovalRequest struct {
	ID      string // random, so answers to an old process's requests match nothing
	Tool    string
	Action  string   // what would run, to show verbatim
	Reasons []string // why it needs approval
}

// ApprovalDone reports how an ApprovalRequest ended, so channels can update
// or withdraw it.
type ApprovalDone struct {
	ID      string
	Outcome ApprovalOutcome
}

// ApprovalOutcome is how an ApprovalRequest ended.
type ApprovalOutcome string

const (
	Allowed   ApprovalOutcome = "allowed"
	Refused   ApprovalOutcome = "refused"
	Expired   ApprovalOutcome = "expired"   // nobody answered in time
	Cancelled ApprovalOutcome = "cancelled" // the work asking was stopped
)

// SecretRequest asks the user to enter a secret's value where the model
// can't see it. Channels pass the value to Sessions.ProvideSecret, or
// Sessions.CancelSecret if the user declines, without it ever becoming a
// message; a SecretDone with the same ID follows however it ends. A
// session has at most one waiting at a time.
type SecretRequest struct {
	ID      string // random, like ApprovalRequest.ID
	Name    string
	Purpose string // what the model says it's for
}

// SecretDone reports how a SecretRequest ended.
type SecretDone struct {
	ID      string
	Outcome SecretOutcome
	Problem string // why it wasn't saved, for SecretNotSaved
}

// SecretOutcome is how a SecretRequest ended.
type SecretOutcome string

const (
	SecretReceived  SecretOutcome = "received"  // and saved
	SecretNotSaved  SecretOutcome = "not saved" // received, but saving failed
	SecretDeclined  SecretOutcome = "declined"  // the user cancelled
	SecretExpired   SecretOutcome = "expired"   // nobody answered in time
	SecretCancelled SecretOutcome = "cancelled" // the work asking was stopped
)

func (TextDelta) event() {}
func (Reply) event()     {}
func (ToolStart) event() {}
func (ToolEnd) event()   {}
func (TurnDone) event()  {}
func (Notice) event()    {}

func (ApprovalRequest) event() {}
func (ApprovalDone) event()    {}
func (SecretRequest) event()   {}
func (SecretDone) event()      {}

// Toolbox is what a session gives the model; in steel, a scratchpad.
type Toolbox interface {
	Defs() []llm.ToolDef
	Call(ctx context.Context, name string, args json.RawMessage) (string, error)
	// Stop halts running work and drops background jobs; once it returns no
	// job result is delivered. It reports whether state was lost doing so.
	Stop() bool
	Reset()     // discard script state: variables and running work
	Busy() bool // background work still pending or being delivered
	Close()
}

// Hooks route a toolbox's asynchronous output back into its session.
type Hooks struct {
	// Session is the session's ID, e.g. for tools whose effects belong to
	// a conversation.
	Session string
	// Emit reports events such as nested tool calls. Safe for concurrent use.
	Emit func(Event)
	// Deliver queues text for the model, e.g. a finished background job.
	// Safe for concurrent use.
	Deliver func(text string)
	// Approve asks the user whether a tool may do something and waits for
	// the answer, until ctx ends. Safe for concurrent use.
	Approve func(ctx context.Context, tool, action string, reasons []string) (bool, error)
	// AskSecret asks the user to enter secret name's value out of the
	// model's sight, waits for it until ctx ends, and passes it to save
	// before telling the user how it went. Safe for concurrent use.
	AskSecret func(ctx context.Context, name, purpose string, save func(value string) error) error
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
	// Memory, if set, opens every new conversation with what it recalls.
	// System should then include how to use it, and NewToolbox its tools.
	Memory memory.Memory
	// IdleTimeout closes the toolboxes of sessions idle this long, freeing
	// their memory; 0 never closes them.
	IdleTimeout time.Duration
}
