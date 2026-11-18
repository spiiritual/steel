package main

import (
	"fmt"
	"os"
	"slices"

	"github.com/spiiritual/steel/internal/llm"
	"github.com/spiiritual/steel/internal/memory"
	"github.com/spiiritual/steel/internal/memory/optmem"
	"github.com/spiiritual/steel/internal/tool"
)

// openedMemory is a memory system ready for sessions: the memory, the
// system prompt that explains it, the tools the model uses it with, and
// how to shut it down.
type openedMemory struct {
	memory memory.Memory
	system string
	tools  []tool.Tool
	close  func()
}

// memorySystems are the memory systems steel can run, by memory-system
// setting. Adding one means a package implementing memory.Memory and an
// entry here. summarizer is the provider for its background model calls,
// if it makes any.
var memorySystems = map[string]func(o *options, summarizer llm.Provider) (openedMemory, error){
	"optmem": func(o *options, summarizer llm.Provider) (openedMemory, error) {
		model := *o.compactModel
		if model == "" {
			model = *o.model
		}
		m, err := optmem.Open(*o.memoryDir, optmem.Options{
			Provider: summarizer,
			Model:    model,
			OnError:  func(err error) { fmt.Fprintln(os.Stderr, "steel:", err) },
		})
		if err != nil {
			return openedMemory{}, err
		}
		return openedMemory{m, optmem.SystemPrompt(systemPrompt(*o.system), m.EntryChars()), optmem.Tools(m), m.Close}, nil
	},
}

func memorySystemNames() []string {
	names := make([]string, 0, len(memorySystems))
	for name := range memorySystems {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// openMemory opens the memory system o selects.
func openMemory(o *options, summarizer llm.Provider) (openedMemory, error) {
	return memorySystems[o.memorySystem.value](o, summarizer)
}
