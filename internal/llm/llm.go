// Package llm defines the provider-agnostic types the agent talks to models with.
package llm

import (
	"context"
	"encoding/json"
)

// Role identifies the author of a message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is one turn in a conversation.
type Message struct {
	Role    Role
	Content string
	// ToolCalls are the tools an assistant message asks to run.
	ToolCalls []ToolCall
	// ToolCallID links a RoleTool message to the call it answers.
	ToolCallID string
}

// ToolCall is a model's request to run one tool.
type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage // JSON object, as produced by the model
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
	// once the stream ends.
	Stream(ctx context.Context, req Request, onDelta func(string) error) (Message, error)
}
