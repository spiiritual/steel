package tool

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestSchemaSignature(t *testing.T) {
	input := json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "File  path\n(absolute)"},
			"limit": {"type": "integer"},
			"mode": {"enum": ["fast", "deep"]},
			"kind": {"const": "issue"},
			"tags": {"type": "array", "items": {"type": ["string", "null"]}},
			"meta": {"type": "object", "additionalProperties": {"type": "number"}},
			"owner": {"$ref": "#/$defs/user"},
			"site-name": {"anyOf": [{"type": "string"}, {"type": "null"}]}
		},
		"required": ["path"],
		"$defs": {
			"user": {"type": "object", "properties": {"login": {"type": "string"}, "manager": {"$ref": "#/$defs/user"}}, "required": ["login"]}
		}
	}`)
	output := map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "integer"}}, "required": []string{"id"}}

	got := SchemaSignature(input, output, "string")
	want := `(args: {
  /** File path (absolute) */
  path: string;
  limit?: number;
  mode?: "fast" | "deep";
  kind?: "issue";
  tags?: (string | null)[];
  meta?: Record<string, number>;
  owner?: { login: string; manager?: unknown };
  "site-name"?: string | null;
}): Promise<{ id: number }>`
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}

	if got := SchemaSignature(json.RawMessage(`{"type":"object","properties":{}}`), nil, "string"); got != "(): Promise<string>" {
		t.Errorf("empty input = %q", got)
	}
	if got := SchemaSignature(nil, nil, "string"); got != "(): Promise<string>" {
		t.Errorf("nil schemas = %q", got)
	}
}

func TestSchemaSignatureEdgeCases(t *testing.T) {
	raw := func(s string) json.RawMessage { return json.RawMessage(s) }
	tests := []struct {
		name          string
		input, output any
		want          string
	}{
		{
			name: "boolean subschemas",
			input: raw(`{"type":"object","properties":{
				"any": true, "none": false,
				"list": {"type":"array","items":true},
				"ref": {"$ref":"#/$defs/x"}, "nothing": {"$ref":"#/$defs/n"},
				"map": {"type":"object","additionalProperties":true}
			},"$defs":{"x":true,"n":false}}`),
			output: true,
			want:   `(args: { any?: unknown; none?: never; list?: unknown[]; ref?: unknown; nothing?: never; map?: Record<string, unknown> }): Promise<unknown>`,
		},
		{
			name:   "boolean subschemas from map",
			input:  map[string]any{"type": "object", "properties": map[string]any{"value": true}},
			output: false,
			want:   `(args: { value?: unknown }): Promise<never>`,
		},
		{
			name:  "null properties",
			input: raw(`{"type":"object","properties":null}`),
			want:  `(): Promise<string>`,
		},
		{
			name:   "nested null properties",
			input:  raw(`{"type":"object","properties":{"o":{"type":"object","properties":null}}}`),
			output: raw(`{"type":"object","properties":null}`),
			want:   `(args: { o?: Record<string, unknown> }): Promise<Record<string, unknown>>`,
		},
		{
			name:  "tuple items",
			input: raw(`{"type":"object","properties":{"pt":{"type":"array","items":[{"type":"string"},{"type":"number"}]}}}`),
			want:  `(args: { pt?: [string, number] }): Promise<string>`,
		},
		{
			name:   "unparseable schemas",
			input:  raw(`"nope"`),
			output: raw(`not json`),
			want:   `(args: Record<string, unknown>): Promise<unknown>`,
		},
		{
			name:   "unmarshalable schemas",
			input:  make(chan int),
			output: func() {},
			want:   `(args: Record<string, unknown>): Promise<unknown>`,
		},
		{
			name: "required first from map",
			input: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"limit": map[string]any{"type": "integer"},
					"query": map[string]any{"type": "string"},
					"sort":  map[string]any{"type": "string"},
					"area":  map[string]any{"type": "string"},
				},
				"required": []string{"query", "area"},
			},
			want: `(args: { area: string; query: string; limit?: number; sort?: string }): Promise<string>`,
		},
		{
			name:  "required first keeps wire order",
			input: raw(`{"type":"object","properties":{"b":{"type":"string"},"z":{"type":"string"},"a":{"type":"string"}},"required":["a","z"]}`),
			want:  `(args: { z: string; a: string; b?: string }): Promise<string>`,
		},
		{
			name:  "anyOf with properties",
			input: raw(`{"type":"object","properties":{"id":{"type":"string"},"name":{"type":"string"}},"anyOf":[{"required":["id"]},{"required":["name"]}]}`),
			want:  `(args: { id?: string; name?: string }): Promise<string>`,
		},
		{
			name:  "top-level ref",
			input: raw(`{"$ref":"#/$defs/args","$defs":{"args":{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}}}`),
			want:  `(args: { q: string }): Promise<string>`,
		},
		{
			name:  "top-level ref to empty object",
			input: raw(`{"$ref":"#/definitions/none","definitions":{"none":{"type":"object","additionalProperties":false}}}`),
			want:  `(): Promise<string>`,
		},
		{
			name:  "additionalProperties only",
			input: raw(`{"type":"object","additionalProperties":{"type":"string"}}`),
			want:  `(args: Record<string, string>): Promise<string>`,
		},
		{
			name:  "additionalProperties false",
			input: raw(`{"type":"object","additionalProperties":false}`),
			want:  `(): Promise<string>`,
		},
		{
			name:  "top-level oneOf",
			input: raw(`{"oneOf":[{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]},{"type":"object","properties":{"b":{"type":"number"}},"required":["b"]}]}`),
			want:  `(args: { a: string } | { b: number }): Promise<string>`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SchemaSignature(tt.input, tt.output, "string"); got != tt.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, tt.want)
			}
		})
	}
}

// TestSchemaSignatureBranchingRefs checks that $defs which each reference
// the next one twice, expanding to 2^n subtrees, still render small and fast.
func TestSchemaSignatureBranchingRefs(t *testing.T) {
	const n = 18
	defs := make([]string, n)
	for i := range n - 1 {
		next := fmt.Sprintf(`{"$ref":"#/$defs/d%d"}`, i+1)
		defs[i] = fmt.Sprintf(`"d%d":{"type":"object","properties":{"left":%s,"right":%s}}`, i, next, next)
	}
	defs[n-1] = fmt.Sprintf(`"d%d":{"type":"string"}`, n-1)
	schema := json.RawMessage(`{"type":"object","properties":{"root":{"$ref":"#/$defs/d0"}},"$defs":{` + strings.Join(defs, ",") + `}}`)

	start := time.Now()
	got := SchemaSignature(schema, nil, "string")
	if len(got) >= 10_000 {
		t.Errorf("signature is %d bytes for a %d-byte schema", len(got), len(schema))
	}
	if !strings.Contains(got, "unknown") {
		t.Errorf("signature not truncated: %s", got)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("rendering took %v", d)
	}
}
