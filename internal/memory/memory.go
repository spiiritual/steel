// Package memory is the contract between steel's sessions and a memory
// system: what a conversation is told about the past when it starts. How a
// memory keeps the past, and the tools the model uses it with, are the
// implementation's business. Implementations live in subpackages (optmem).
package memory

import (
	"context"

	"github.com/spiiritual/steel/internal/llm"
)

// Memory is a memory system as sessions use it.
type Memory interface {
	// Wake returns what a new conversation in chat is told about the past.
	// The messages go right after the system prompt and stay there,
	// unchanged, for the conversation's life, so they're read from the
	// prompt cache. It may wait, e.g. for summaries to be written; ctx
	// bounds that.
	Wake(ctx context.Context, chat string) ([]llm.Message, error)
}
