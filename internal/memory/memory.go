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

// Join is one memory made of several, e.g. notes and skills: a new
// conversation wakes with each one's messages in turn. Nil ones are
// skipped; with none left, Join is nil.
func Join(ms ...Memory) Memory {
	var j joined
	for _, m := range ms {
		if m != nil {
			j = append(j, m)
		}
	}
	switch len(j) {
	case 0:
		return nil
	case 1:
		return j[0]
	}
	return j
}

type joined []Memory

func (j joined) Wake(ctx context.Context, chat string) ([]llm.Message, error) {
	var out []llm.Message
	for _, m := range j {
		msgs, err := m.Wake(ctx, chat)
		if err != nil {
			return nil, err
		}
		out = append(out, msgs...)
	}
	return out, nil
}
