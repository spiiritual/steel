package scratchpad

import (
	"fmt"
	"strings"

	"github.com/spiiritual/steel/internal/tool"
)

// Declarations renders the global tools object as TypeScript, nesting dotted
// tool names into namespaces and using each tool's Signature verbatim.
func Declarations(tools []tool.Tool) string {
	root := &ns{}
	for _, t := range tools {
		parts := strings.Split(t.Name, ".")
		n := root
		for _, p := range parts[:len(parts)-1] {
			n = n.child(p)
		}
		n.fns = append(n.fns, fnDecl{name: parts[len(parts)-1], t: t})
	}
	var b strings.Builder
	b.WriteString("declare const tools: ")
	root.write(&b, 0)
	b.WriteString(";\n")
	return b.String()
}

type fnDecl struct {
	name string
	t    tool.Tool
}

// ns is one namespace level; children and functions keep registration order.
type ns struct {
	names    []string
	children map[string]*ns
	fns      []fnDecl
}

func (n *ns) child(name string) *ns {
	if c, ok := n.children[name]; ok {
		return c
	}
	if n.children == nil {
		n.children = map[string]*ns{}
	}
	c := &ns{}
	n.children[name] = c
	n.names = append(n.names, name)
	return c
}

func (n *ns) write(b *strings.Builder, depth int) {
	in := strings.Repeat("  ", depth+1)
	b.WriteString("{\n")
	for _, name := range n.names {
		b.WriteString(in + tool.TSKey(name) + ": ")
		n.children[name].write(b, depth+1)
		b.WriteString(";\n")
	}
	for _, f := range n.fns {
		writeDoc(b, in, f.t.Description)
		sig := strings.ReplaceAll(f.t.Signature, "\n", "\n"+in) // multi-line object types
		fmt.Fprintf(b, "%s%s%s;\n", in, tool.TSKey(f.name), sig)
	}
	b.WriteString(strings.Repeat("  ", depth) + "}")
}

func writeDoc(b *strings.Builder, indent, desc string) {
	desc = strings.TrimSpace(strings.ReplaceAll(desc, "*/", "* /"))
	if desc == "" {
		return
	}
	lines := strings.Split(desc, "\n")
	if len(lines) == 1 {
		fmt.Fprintf(b, "%s/** %s */\n", indent, desc)
		return
	}
	b.WriteString(indent + "/**\n")
	for _, l := range lines {
		b.WriteString(indent + " * " + l + "\n")
	}
	b.WriteString(indent + " */\n")
}
