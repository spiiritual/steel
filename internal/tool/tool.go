// Package tool holds the tools available inside the scratchpad and the
// registry that dispatches them.
package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
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
	// AcceptsSecrets marks tools that take secrets out of steel, like a
	// command or an MCP server. Only their calls get secret placeholders
	// in the arguments replaced by values, once the user approves; any
	// other tool gets the placeholders as written, so steel's own tools
	// (memory, schedules) never store a value the model could read back.
	AcceptsSecrets bool
}

// Registry is an ordered set of tools, and the owner of their namespaces.
// Tools given to NewRegistry are steel's own and stay; a provider's (an
// MCP server's) come and go a namespace at a time with Add and Remove, and
// Version tells users when to look again. It's safe for concurrent use.
type Registry struct {
	mu      sync.RWMutex
	tools   []Tool
	index   map[string]int
	taken   map[string]bool   // every namespace in use
	added   map[string]bool   // namespaces from Add, which Remove can drop
	notes   map[string]string // namespace → guidance from its provider
	version int
	recent  *Recent // nil: not tracked
}

// NewRegistry returns a registry of steel's own tools. It panics on
// duplicate names.
func NewRegistry(tools ...Tool) *Registry {
	r := &Registry{index: map[string]int{}, taken: map[string]bool{}, added: map[string]bool{}, notes: map[string]string{}}
	for _, t := range tools {
		if _, dup := r.index[t.Name]; dup {
			panic("tool: duplicate tool " + t.Name)
		}
		r.index[t.Name] = len(r.tools)
		r.tools = append(r.tools, t)
		r.taken[Namespace(t.Name)] = true
	}
	return r
}

// Add gives namespace ns to a provider: its tools, all named ns.*, and its
// guidance, note. It fails if ns is taken.
func (r *Registry) Add(ns, note string, tools ...Tool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ns == "" || r.taken[ns] {
		return fmt.Errorf("tools.%s is taken", ns)
	}
	for _, t := range tools {
		if Namespace(t.Name) != ns {
			return fmt.Errorf("tool %s is outside tools.%s", t.Name, ns)
		}
	}
	for _, t := range tools {
		r.index[t.Name] = len(r.tools)
		r.tools = append(r.tools, t)
	}
	r.taken[ns], r.added[ns] = true, true
	if note != "" {
		r.notes[ns] = note
	}
	r.version++
	return nil
}

// Remove drops namespace ns and its tools, if Add gave it; steel's own
// tools stay. It reports whether it removed anything.
func (r *Registry) Remove(ns string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.added[ns] {
		return false
	}
	r.tools = slices.DeleteFunc(r.tools, func(t Tool) bool { return Namespace(t.Name) == ns })
	clear(r.index)
	for i, t := range r.tools {
		r.index[t.Name] = i
	}
	delete(r.taken, ns)
	delete(r.added, ns)
	delete(r.notes, ns)
	r.version++
	return true
}

// Taken reports whether namespace ns is in use.
func (r *Registry) Taken(ns string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.taken[ns]
}

// Namespace returns a tool's top-level namespace: "shell" for "shell.bash",
// "" for a top-level function.
func Namespace(name string) string {
	ns, _, found := strings.Cut(name, ".")
	if !found {
		return ""
	}
	return ns
}

// Version counts the registry's changes since creation.
func (r *Registry) Version() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.version
}

// Notes returns each namespace's guidance from its provider.
func (r *Registry) Notes() map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return maps.Clone(r.notes)
}

// Get returns the named tool.
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	i, ok := r.index[name]
	if !ok {
		return Tool{}, false
	}
	return r.tools[i], true
}

// TrackRecent records every successful call in rec. Call it before the
// registry is in use.
func (r *Registry) TrackRecent(rec *Recent) {
	r.recent = rec
}

// Recent returns tool names, most recently used successfully first; nil
// when not tracked.
func (r *Registry) Recent() []string {
	if r.recent == nil {
		return nil
	}
	return r.recent.Names()
}

// All returns the registered tools in registration order.
func (r *Registry) All() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]Tool(nil), r.tools...)
}

// Call runs the named tool.
func (r *Registry) Call(ctx context.Context, name string, args json.RawMessage) (any, error) {
	t, ok := r.Get(name)
	if !ok {
		return nil, fmt.Errorf("unknown tool %q: its server may have been removed", name)
	}
	out, err := t.Run(ctx, args)
	if err == nil && r.recent != nil {
		r.recent.Use(name)
	}
	return out, err
}
