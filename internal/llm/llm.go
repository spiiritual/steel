// Package llm defines the provider-agnostic types the agent talks to models with.
package llm

import "context"

// Role identifies the author of a message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is one turn in a conversation.
type Message struct {
	Role    Role
	Content string
}

// Request is a single completion request.
type Request struct {
	Model    string
	Messages []Message
}

// Provider generates completions from a model.
type Provider interface {
	// Stream sends req and calls onDelta with each text fragment as it arrives.
	// It returns the complete assistant message once the stream ends.
	Stream(ctx context.Context, req Request, onDelta func(string) error) (Message, error)
}
