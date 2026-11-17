package tool

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// SchemaSignature renders a TypeScript signature (the part after the
// function name) from JSON Schemas, for tools described by schema rather
// than Go types, such as MCP tools. An input that accepts no arguments
// gives "()"; one that cannot be parsed gives "args: Record<string,
// unknown>". A nil output schema gives result, e.g. "string"; one that
// cannot be parsed gives "unknown".
func SchemaSignature(input, output any, result string) string {
	params := ""
	switch in, size, err := parseSchema(input); {
	case err != nil:
		params = "args: Record<string, unknown>"
	case in != nil && hasArgs(in):
		params = "args: " + renderSchema(in, size)
	}
	switch out, size, err := parseSchema(output); {
	case err != nil:
		result = "unknown"
	case out != nil:
		result = renderSchema(out, size)
	}
	return fmt.Sprintf("(%s): Promise<%s>", params, result)
}

// jsonSchema is the subset of JSON Schema that maps to TypeScript.
type jsonSchema struct {
	Bool                 *bool // boolean schema: true accepts anything, false nothing
	Ref                  string
	Type                 []string // "string" or ["string","null"]
	Description          string
	Properties           orderedProps
	Required             []string
	Items                *jsonSchema
	TupleItems           []*jsonSchema // "items": [A, B]
	AdditionalProperties *jsonSchema
	Enum                 []any
	Const                json.RawMessage
	AnyOf                []*jsonSchema
	OneOf                []*jsonSchema
	Defs                 map[string]*jsonSchema
	Definitions          map[string]*jsonSchema
}

// UnmarshalJSON decodes a schema object or boolean schema. Each keyword
// decodes independently and malformed keywords are dropped, so one odd
// keyword cannot discard the whole schema.
func (s *jsonSchema) UnmarshalJSON(b []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		var v bool
		if json.Unmarshal(b, &v) != nil {
			return err
		}
		s.Bool = &v
		return nil
	}
	field := func(name string, v any) {
		if raw, ok := fields[name]; ok {
			_ = json.Unmarshal(raw, v)
		}
	}
	field("$ref", &s.Ref)
	field("description", &s.Description)
	field("properties", &s.Properties)
	field("required", &s.Required)
	field("additionalProperties", &s.AdditionalProperties)
	field("enum", &s.Enum)
	field("anyOf", &s.AnyOf)
	field("oneOf", &s.OneOf)
	field("$defs", &s.Defs)
	field("definitions", &s.Definitions)
	if c, ok := fields["const"]; ok {
		s.Const = c
	}
	if t, ok := fields["type"]; ok {
		var one string
		if json.Unmarshal(t, &one) == nil {
			s.Type = []string{one}
		} else {
			_ = json.Unmarshal(t, &s.Type)
		}
	}
	if it, ok := fields["items"]; ok {
		if bytes.HasPrefix(bytes.TrimSpace(it), []byte("[")) {
			_ = json.Unmarshal(it, &s.TupleItems)
		} else {
			_ = json.Unmarshal(it, &s.Items)
		}
	}
	return nil
}

func (s *jsonSchema) isFalse() bool { return s.Bool != nil && !*s.Bool }

type schemaProp struct {
	name string
	s    *jsonSchema
}

// orderedProps keeps properties in schema order, which is usually meaningful.
type orderedProps []schemaProp

func (o *orderedProps) UnmarshalJSON(b []byte) error {
	if string(bytes.TrimSpace(b)) == "null" {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return errors.New("properties: want object")
	}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		var s jsonSchema
		if err := dec.Decode(&s); err != nil {
			return err
		}
		*o = append(*o, schemaProp{name: t.(string), s: &s})
	}
	return nil
}

// parseSchema accepts any JSON-marshalable schema value and also returns
// its encoded size. A nil or JSON null value gives a nil schema and no
// error.
func parseSchema(v any) (*jsonSchema, int, error) {
	if v == nil {
		return nil, 0, nil
	}
	b, ok := v.(json.RawMessage)
	if !ok {
		var err error
		if b, err = json.Marshal(v); err != nil {
			return nil, 0, err
		}
	}
	if t := bytes.TrimSpace(b); len(t) == 0 || string(t) == "null" {
		return nil, 0, nil
	}
	var s jsonSchema
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, 0, err
	}
	return &s, len(b), nil
}

// hasArgs reports whether input, after resolving a top-level $ref, takes
// arguments: it has properties, allows additional properties, or is a
// composition. An unresolvable $ref counts as taking arguments.
func hasArgs(input *jsonSchema) bool {
	s := input
	var seen []string
	for s != nil && s.Ref != "" {
		if slices.Contains(seen, s.Ref) {
			return true
		}
		seen = append(seen, s.Ref)
		s = resolveRef(input, s.Ref)
	}
	if s == nil {
		return true
	}
	return len(s.Properties) > 0 ||
		s.AdditionalProperties != nil && !s.AdditionalProperties.isFalse() ||
		len(s.AnyOf) > 0 || len(s.OneOf) > 0
}

// renderBudget bounds how far a rendering may exceed its schema's size, in
// bytes. Branching $refs expand exponentially; once the budget is spent,
// remaining subtrees render as "unknown".
const renderBudget = 4096

// schemaRenderer renders one schema; root resolves local $refs.
type schemaRenderer struct {
	root *jsonSchema
	left int // approximate output bytes left
}

func renderSchema(s *jsonSchema, size int) string {
	r := &schemaRenderer{root: s, left: renderBudget + size}
	return r.ts(s, 0, nil)
}

// spend charges the members of a union against the budget and returns them.
func (r *schemaRenderer) spend(ts ...string) []string {
	for _, t := range ts {
		r.left -= len(t) + len(" | ")
	}
	return ts
}

// ts renders s as a TypeScript type. depth sets indentation; seen guards
// recursive refs.
func (r *schemaRenderer) ts(s *jsonSchema, depth int, seen []string) string {
	return strings.Join(r.union(s, depth, seen), " | ")
}

// union renders s as the members of a TypeScript union.
func (r *schemaRenderer) union(s *jsonSchema, depth int, seen []string) []string {
	if s == nil || r.left <= 0 {
		return []string{"unknown"}
	}
	if s.Bool != nil {
		if *s.Bool {
			return r.spend("unknown")
		}
		return r.spend("never")
	}
	if s.Ref != "" {
		target := resolveRef(r.root, s.Ref)
		if target == nil || slices.Contains(seen, s.Ref) {
			return r.spend("unknown")
		}
		return r.union(target, depth, append(seen, s.Ref))
	}
	if s.Const != nil {
		return r.spend(string(s.Const))
	}
	if len(s.Enum) > 0 {
		lits := make([]string, len(s.Enum))
		for i, v := range s.Enum {
			j, _ := json.Marshal(v)
			lits[i] = string(j)
		}
		return r.spend(lits...)
	}
	// A composition beside properties or a type usually only constrains
	// the base shape (e.g. anyOf required sets), so the base wins.
	if alts := slices.Concat(s.AnyOf, s.OneOf); len(alts) > 0 && len(s.Type) == 0 && len(s.Properties) == 0 {
		var ts []string
		for _, a := range alts {
			for _, m := range r.union(a, depth, seen) {
				if !slices.Contains(ts, m) {
					ts = append(ts, m)
				}
			}
		}
		return ts
	}

	types := s.Type
	if len(types) == 0 {
		switch {
		case len(s.Properties) > 0 || s.AdditionalProperties != nil:
			types = []string{"object"}
		case s.Items != nil || s.TupleItems != nil:
			types = []string{"array"}
		default:
			return r.spend("unknown")
		}
	}
	out := make([]string, len(types))
	for i, t := range types {
		out[i] = r.typeName(s, t, depth, seen)
	}
	return out
}

// typeName renders one JSON type name of s.
func (r *schemaRenderer) typeName(s *jsonSchema, t string, depth int, seen []string) string {
	switch t {
	case "string", "boolean", "null":
		return r.spend(t)[0]
	case "number", "integer":
		return r.spend("number")[0]
	case "array":
		r.spend("[]")
		if s.TupleItems != nil {
			items := make([]string, len(s.TupleItems))
			for i, it := range s.TupleItems {
				items[i] = r.ts(it, depth, seen)
			}
			return "[" + strings.Join(items, ", ") + "]"
		}
		if s.Items == nil {
			return "unknown[]"
		}
		members := r.union(s.Items, depth, seen)
		it := strings.Join(members, " | ")
		if len(members) > 1 {
			it = "(" + it + ")"
		}
		return it + "[]"
	case "object":
		if len(s.Properties) == 0 {
			r.spend("Record<string, >")
			if s.AdditionalProperties != nil {
				return "Record<string, " + r.ts(s.AdditionalProperties, depth, seen) + ">"
			}
			return "Record<string, unknown>"
		}
		return r.object(s, depth, seen)
	default:
		return r.spend("unknown")[0]
	}
}

// object renders properties inline, or multi-line when any has a doc,
// matching the style of signatures generated from Go types. Required
// properties come first, each group in schema order: schemas built from Go
// maps arrive sorted by name, which would bury required properties.
func (r *schemaRenderer) object(s *jsonSchema, depth int, seen []string) string {
	props := make([]schemaProp, 0, len(s.Properties))
	for _, required := range []bool{true, false} {
		for _, p := range s.Properties {
			if slices.Contains(s.Required, p.name) == required {
				props = append(props, p)
			}
		}
	}
	member := func(p schemaProp, d int) string {
		opt := "?"
		if slices.Contains(s.Required, p.name) {
			opt = ""
		}
		key := TSKey(p.name)
		r.spend(key, opt, ": ;")
		return fmt.Sprintf("%s%s: %s;", key, opt, r.ts(p.s, d, seen))
	}
	multiline := slices.ContainsFunc(props, func(p schemaProp) bool { return p.s.Description != "" })
	if !multiline {
		parts := make([]string, len(props))
		for i, p := range props {
			parts[i] = member(p, depth)
		}
		return "{ " + strings.TrimSuffix(strings.Join(parts, " "), ";") + " }"
	}
	in := strings.Repeat("  ", depth+1)
	var b strings.Builder
	b.WriteString("{\n")
	for _, p := range props {
		if d := docLine(p.s.Description); d != "" {
			r.spend(in, d, "/**  */")
			fmt.Fprintf(&b, "%s/** %s */\n", in, d)
		}
		r.spend(in)
		b.WriteString(in + member(p, depth+1) + "\n")
	}
	b.WriteString(strings.Repeat("  ", depth) + "}")
	return b.String()
}

// resolveRef resolves "#/$defs/X" and "#/definitions/X" against root.
func resolveRef(root *jsonSchema, ref string) *jsonSchema {
	if name, ok := strings.CutPrefix(ref, "#/$defs/"); ok {
		return root.Defs[name]
	}
	if name, ok := strings.CutPrefix(ref, "#/definitions/"); ok {
		return root.Definitions[name]
	}
	return nil
}

// docLine flattens a description into one comment-safe line.
func docLine(desc string) string {
	return strings.ReplaceAll(strings.Join(strings.Fields(desc), " "), "*/", "* /")
}
