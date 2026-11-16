// Package tool holds the tools available inside the scratchpad and the
// registry that dispatches them.
package tool

import (
	"context"
	"encoding/json"
	"fmt"
)

// Tool is a capability scripts can call as tools.<Name>(args).
type Tool struct {
	// Name is dotted: "fs.read_file" is reachable as tools.fs.read_file.
	Name        string
	Description string
	// Signature is the TypeScript the model sees after the function name,
	// covering arguments and result, e.g.
	// "(args: { path: string }): Promise<string>". It must match what Run
	// decodes and returns; nothing checks this.
	Signature string
	// Run executes the tool. The result must be JSON-serializable; a string
	// reaches scripts as a JS string, anything else as the equivalent JS value.
	Run func(ctx context.Context, args json.RawMessage) (any, error)
}

// Registry is an ordered set of tools.
type Registry struct {
	tools []Tool
	index map[string]int
}

// NewRegistry returns a registry of tools. It panics on duplicate names.
func NewRegistry(tools ...Tool) *Registry {
	r := &Registry{tools: tools, index: make(map[string]int, len(tools))}
	for i, t := range tools {
		if _, dup := r.index[t.Name]; dup {
			panic("tool: duplicate tool " + t.Name)
		}
		r.index[t.Name] = i
	}
	return r
}

// All returns the registered tools in registration order.
func (r *Registry) All() []Tool {
	return append([]Tool(nil), r.tools...)
}

// Call runs the named tool.
func (r *Registry) Call(ctx context.Context, name string, args json.RawMessage) (any, error) {
	i, ok := r.index[name]
	if !ok {
		return nil, fmt.Errorf("unknown tool %q", name)
	}
	return r.tools[i].Run(ctx, args)
}
