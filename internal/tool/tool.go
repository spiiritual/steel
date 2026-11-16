// Package tool holds the tools the agent can run and the registry that dispatches them.
package tool

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spiiritual/steel/internal/llm"
)

// maxOutput caps tool output returned to the model, in bytes.
const maxOutput = 64 << 10

// Tool is a function the model can call.
type Tool struct {
	Name        string
	Description string
	Parameters  json.RawMessage // JSON Schema for the arguments object
	// Run executes the tool. A returned error is reported to the model as the
	// tool result; it does not stop the agent.
	Run func(ctx context.Context, args json.RawMessage) (string, error)
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

// Defs returns the tool definitions to advertise to the model.
func (r *Registry) Defs() []llm.ToolDef {
	defs := make([]llm.ToolDef, len(r.tools))
	for i, t := range r.tools {
		defs[i] = llm.ToolDef{Name: t.Name, Description: t.Description, Parameters: t.Parameters}
	}
	return defs
}

// Call runs the named tool and truncates its output to maxOutput.
func (r *Registry) Call(ctx context.Context, name string, args json.RawMessage) (string, error) {
	i, ok := r.index[name]
	if !ok {
		return "", fmt.Errorf("unknown tool %q", name)
	}
	out, err := r.tools[i].Run(ctx, args)
	if len(out) > maxOutput {
		out = fmt.Sprintf("%s\n[truncated: %d of %d bytes shown]", out[:maxOutput], maxOutput, len(out))
	}
	return out, err
}

// decodeArgs unmarshals a tool's JSON arguments into v.
func decodeArgs(args json.RawMessage, v any) error {
	if err := json.Unmarshal(args, v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}
