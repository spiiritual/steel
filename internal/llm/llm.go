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
}

// Provider generates completions from a model.
type Provider interface {
	// Stream sends req and calls onDelta with each text fragment as it arrives.
	// It returns the complete assistant message, including any tool calls,
	// once the stream ends. If the reply hit the token limit, Stream returns
	// the (non-zero) partial Message together with ErrTruncated.
	Stream(ctx context.Context, req Request, onDelta func(string) error) (Message, error)
}
