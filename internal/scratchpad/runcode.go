package scratchpad

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spiiritual/steel/internal/llm"
)

// runCodeParams is the JSON Schema the model API needs for run_code. It's the
// only schema in steel: tools inside the pad are described in TypeScript.
const runCodeParams = `{"type":"object","properties":{"code":{"type":"string","description":"JavaScript to run"},"reset":{"type":"boolean","description":"Clear all scratchpad state before running"}},"required":["code"]}`

// Defs advertises run_code, the model's only tool.
func (p *Pad) Defs() []llm.ToolDef {
	return []llm.ToolDef{{
		Name: "run_code",
		Description: fmt.Sprintf(`Run JavaScript in your persistent scratchpad. This is how you use tools: they are async functions on the global `+"`tools`"+` object, declared below.

- `+"`code`"+` runs as the body of an async function: use top-level await, and `+"`return`"+` a value to get it back (strings verbatim, anything else as JSON). console.log output is returned too.
- State persists between calls only through globals: `+"`globalThis.data = ...`"+` (or a bare `+"`data = ...`"+`). const/let/function declarations are local to one call.
- Run independent tool calls in parallel: `+"`await Promise.all([...])`"+`.
- A failed tool call throws an Error; use try/catch to handle it.
- Filter and summarize in code; return only what you need instead of large raw outputs.
- A call that runs longer than %s is stopped and the scratchpad is reset, losing all state. Pass reset: true to clear state yourself.

`+"```ts\n%s```", p.opts.Timeout, Declarations(p.reg.All())),
		Parameters: json.RawMessage(runCodeParams),
	}}
}

// Call runs a run_code call from the model.
func (p *Pad) Call(ctx context.Context, name string, raw json.RawMessage) (string, error) {
	if name != "run_code" {
		return "", fmt.Errorf("unknown tool %q; use run_code", name)
	}
	var args struct {
		Code  string `json:"code"`
		Reset bool   `json:"reset"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if args.Reset {
		p.Reset()
	}
	r, err := p.Run(ctx, args.Code)
	if err != nil {
		return "", err
	}
	return r.String(), nil
}
