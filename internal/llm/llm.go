// Package llm defines the provider-agnostic types the agent talks to models with.
package llm

import (
	"context"
	"encoding/json"
	"errors"
)

// ErrTruncated reports that the model's reply was cut off by the token limit.
// The accompanying Message holds what was generated; any tool calls in it may
// have incomplete (invalid JSON) arguments.
var ErrTruncated = errors.New("reply truncated by token limit")

// Role identifies the author of a message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is one turn in a conversation. Its JSON form is used to persist
// history.
type Message struct {
	Role    Role   `json:"role"`
	Content string `json:"content,omitempty"`
	// ToolCalls are the tools an assistant message asks to run.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID links a RoleTool message to the call it answers.
	ToolCallID string `json:"tool_call_id,omitempty"`
	// Blocks is an assistant reply exactly as its provider returned it, for
	// providers that need it sent back unchanged: Anthropic's signed thinking
	// blocks. It's opaque to everything else, which uses Content and
	// ToolCalls, and saved with history.
	Blocks json.RawMessage `json:"blocks,omitempty"`
	// Usage is what a reply cost, when the provider reports it. It isn't
	// saved with history.
	Usage *Usage `json:"-"`
}

// Usage counts a request's tokens. Cached is the part of Input the
// provider read from its prompt cache.
type Usage struct {
	Input, Cached, Output int
}

// ToolCall is a model's request to run one tool.
type ToolCall struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Arguments is the raw argument text exactly as the model produced it:
	// normally a JSON object, but possibly invalid JSON (truncated or sloppy
	// output). It is a string so history always serializes.
	Arguments string `json:"arguments"`
}

// ToolDef describes a tool the model may call.
type ToolDef struct {
	Name        string
	Description string
	Parameters  json.RawMessage // JSON Schema for the arguments object
}

// Request is a single completion request.
type Request struct {
	Model    string
	Messages []Message
	Tools    []ToolDef
	// NoToolCalls sends Tools but forbids calling them, so a request can
	// share a cached prefix with ones that may call tools.
	NoToolCalls bool
	// MaxTokens caps the reply's tokens, reasoning included; 0 leaves it
	// to the provider. A reply cut by the cap comes with ErrTruncated.
	MaxTokens int
	// Effort is how hard the model should think: "low", "medium", "high",
	// "xhigh" or "max". "" leaves it to the model's default.
	Effort string
}

// Provider generates completions from a model.
type Provider interface {
	// Stream sends req and calls onDelta with each text fragment as it arrives.
	// It returns the complete assistant message, including any tool calls,
	// once the stream ends. If the reply hit the token limit, Stream returns
	// the (non-zero) partial Message together with ErrTruncated.
	Stream(ctx context.Context, req Request, onDelta func(string) error) (Message, error)
}
