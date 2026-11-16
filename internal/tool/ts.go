package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"time"
)

// New builds a Tool from a typed function. The TypeScript signature the model
// sees is generated from A and R, so the Go types are the single source of
// truth. Struct fields use their json tags for names; a `desc` tag becomes a
// doc comment; pointer and omitempty fields are optional. Unknown argument
// fields are rejected.
func New[A, R any](name, description string, fn func(ctx context.Context, args A) (R, error)) Tool {
	argsT := reflect.TypeFor[A]()
	params := "args: " + tsType(argsT, 0, nil)
	if argsT.Kind() == reflect.Struct && len(fields(argsT)) == 0 {
		params = ""
	}
	return Tool{
		Name:        name,
		Description: description,
		Signature:   fmt.Sprintf("(%s): Promise<%s>", params, tsType(reflect.TypeFor[R](), 0, nil)),
		Run: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var args A
			if len(raw) > 0 {
				dec := json.NewDecoder(bytes.NewReader(raw))
				dec.DisallowUnknownFields()
				if err := dec.Decode(&args); err != nil {
					return nil, fmt.Errorf("invalid arguments: %w", err)
				}
			}
			return fn(ctx, args)
		},
	}
}

var (
	timeType      = reflect.TypeFor[time.Time]()
	marshalerType = reflect.TypeFor[json.Marshaler]()
)

// tsType renders t as the TypeScript type of its JSON encoding. depth sets
// indentation for multi-line objects; seen guards against recursive types.
func tsType(t reflect.Type, depth int, seen map[reflect.Type]bool) string {
	if t == timeType {
		return "string"
	}
	if t.Kind() != reflect.Pointer && t.Implements(marshalerType) {
		return "unknown" // custom encoding: shape unknowable from the type
	}
	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		return "number"
	case reflect.Pointer:
		return tsType(t.Elem(), depth, seen) + " | null"
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			return "string" // encoding/json base64-encodes byte slices
		}
		el := tsType(t.Elem(), depth, seen)
		if t.Elem().Kind() == reflect.Pointer { // "T | null" needs grouping
			el = "(" + el + ")"
		}
		return el + "[]"
	case reflect.Map:
		return "Record<string, " + tsType(t.Elem(), depth, seen) + ">"
	case reflect.Struct:
		if seen[t] {
			return "unknown" // recursive type
		}
		if seen == nil {
			seen = map[reflect.Type]bool{}
		}
		seen[t] = true
		defer delete(seen, t)
		return tsObject(t, depth, seen)
	default: // interface, func, chan, ...
		return "unknown"
	}
}

type field struct {
	name     string
	optional bool
	desc     string
	t        reflect.Type
}

// fields lists t's JSON-visible fields, flattening embedded structs.
func fields(t reflect.Type) []field {
	var out []field
	for f := range t.Fields() {
		if !f.IsExported() && !f.Anonymous {
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		ft := f.Type
		if f.Anonymous && name == "" {
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				out = append(out, fields(ft)...)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		optional := strings.Contains(opts, "omitempty") || strings.Contains(opts, "omitzero")
		if ft.Kind() == reflect.Pointer {
			optional, ft = true, ft.Elem()
		}
		out = append(out, field{name: name, optional: optional, desc: f.Tag.Get("desc"), t: ft})
	}
	return out
}

// tsObject renders a struct inline, or multi-line when any field has a doc.
func tsObject(t reflect.Type, depth int, seen map[reflect.Type]bool) string {
	fs := fields(t)
	if len(fs) == 0 {
		return "{}"
	}
	multiline := false
	for _, f := range fs {
		if f.desc != "" {
			multiline = true
		}
	}
	member := func(f field, d int) string {
		opt := ""
		if f.optional {
			opt = "?"
		}
		return fmt.Sprintf("%s%s: %s;", TSKey(f.name), opt, tsType(f.t, d, seen))
	}
	if !multiline {
		parts := make([]string, len(fs))
		for i, f := range fs {
			parts[i] = member(f, depth)
		}
		return "{ " + strings.TrimSuffix(strings.Join(parts, " "), ";") + " }"
	}
	in := strings.Repeat("  ", depth+1)
	var b strings.Builder
	b.WriteString("{\n")
	for _, f := range fs {
		if f.desc != "" {
			fmt.Fprintf(&b, "%s/** %s */\n", in, strings.ReplaceAll(f.desc, "*/", "* /"))
		}
		b.WriteString(in + member(f, depth+1) + "\n")
	}
	b.WriteString(strings.Repeat("  ", depth) + "}")
	return b.String()
}

var identRE = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)

// TSKey quotes names that aren't valid JS identifiers, for use as object keys.
func TSKey(name string) string {
	if identRE.MatchString(name) {
		return name
	}
	q, _ := json.Marshal(name)
	return string(q)
}
