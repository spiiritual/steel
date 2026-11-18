package scratchpad

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/spiiritual/steel/internal/tool"
)

// layout is how a pad presents its tools to the model. It's fixed when the
// pad is created, so the run_code description stays identical, and prompt
// caches stay warm, for the pad's whole life.
type layout struct {
	decl  string // TypeScript declarations of the namespaces shown in full
	notes string // guidance for namespaces shown in full; "" if none
	index string // the other tools, one line each under a namespace summary; "" if none
	// What the first call into the index shows instead of running: each
	// tool's full declaration, and once per namespace its guidance.
	docs    map[string]string // tool name → declaration
	nsNotes map[string]string // namespace → quoted guidance
}

// newLayout orders tools by recent use, then declares them in full, one
// tool at a time, while they fit within budget bytes (<= 0: unlimited), so
// a big namespace still gets its most used tools declared. The rest go in
// the index: per namespace, a one-line summary of its guidance unless it
// was shown in full already, then one line per tool with its full name and
// short description.
//
// recent lists tool names, most recently used first; tools not in it keep
// registration order after those that are. guidance maps namespaces to
// notes from their provider (an MCP server's instructions).
func newLayout(tools []tool.Tool, budget int, recent []string, guidance map[string]string) layout {
	full, indexed := partition(byRecency(tools, recent), budget)
	l := layout{docs: map[string]string{}, nsNotes: map[string]string{}}

	// A namespace with any tool declared gets its guidance in full upfront.
	var notes strings.Builder
	shown := map[string]bool{}
	for _, t := range full {
		ns := tool.Namespace(t.Name)
		if text := guidance[ns]; text != "" && ns != "" && !shown[ns] {
			fmt.Fprintf(&notes, "tools.%s.*:\n%s\n", ns, quote(text))
		}
		shown[ns] = true
	}
	l.decl, l.notes = render(full), notes.String()

	var idx strings.Builder
	for _, g := range byNamespace(indexed) {
		if g.name == "" {
			idx.WriteString("tools:\n")
		} else {
			idx.WriteString("tools." + g.name + ":")
			if text := guidance[g.name]; text != "" && !shown[g.name] {
				idx.WriteString(" " + shortDesc(text, 160))
				l.nsNotes[g.name] = quote(text)
			}
			idx.WriteString("\n")
		}
		for _, t := range g.tools {
			// The full name, as tools.$docs takes it: a weak model copies
			// what it sees, and a name relative to the heading above
			// sent it to $docs("agent_browser_open"), which doesn't exist.
			idx.WriteString("  " + t.Name)
			if d := shortDesc(t.Description, 80); d != "" {
				idx.WriteString(" — " + d)
			}
			idx.WriteString("\n")
			l.docs[t.Name] = render([]tool.Tool{t})
		}
	}
	l.index = idx.String()
	if l.index != "" {
		// What the index needs to be used, declared first, where a model
		// looks for what it can call: how to read an indexed tool's docs,
		// and how to search for one it can't spot.
		l.decl = strings.Replace(l.decl, "declare const tools: {\n", "declare const tools: {\n"+indexTools(indexed[0].Name), 1)
	}
	return l
}

// indexTools declares tools.$catalog and tools.$docs, which pad.go defines.
// Its example names a real indexed tool: a placeholder like "ns.tool" gets
// copied literally.
func indexTools(example string) string {
	return `  /**
   * Every tool, declared or indexed, to search with code when you can't spot
   * the one you need. For example:
   * tools.$catalog.filter(t => /issue/i.test(t.description)).map(t => t.name + ": " + t.description).join("\n")
   */
  $catalog: { name: string; description: string; signature: string }[];
  /**
   * Asks for the documentation of indexed tools, named exactly as the index
   * lists them, e.g. tools.$docs(` + strconv.Quote(example) + `). Ask for every
   * tool you plan to use at once. It comes back under [documentation] with
   * this script's result, and the tools run from the next script on.
   */
  $docs(...names: string[]): string;
`
}

// byRecency sorts tools in recent's order, most recent first; the rest
// follow in their original order.
func byRecency(tools []tool.Tool, recent []string) []tool.Tool {
	rank := map[string]int{}
	for i, name := range recent {
		if _, ok := rank[name]; !ok {
			rank[name] = i
		}
	}
	out := slices.Clone(tools)
	slices.SortStableFunc(out, func(a, b tool.Tool) int {
		ra, okA := rank[a.Name]
		rb, okB := rank[b.Name]
		switch {
		case okA && okB:
			return cmp.Compare(ra, rb)
		case okA:
			return -1
		case okB:
			return 1
		}
		return 0
	})
	return out
}

// partition splits tools, in order, into those whose declarations fit
// within budget and the rest. A tool too big for what's left is skipped,
// so later smaller ones can still fit.
func partition(tools []tool.Tool, budget int) (full, indexed []tool.Tool) {
	used := 0
	for _, t := range tools {
		// Alone, a tool's declaration carries the wrapping of its
		// namespaces, which declared together they share: an overestimate
		// that keeps the total within budget.
		size := len(render([]tool.Tool{t}))
		if budget <= 0 || used+size <= budget {
			full = append(full, t)
			used += size
			continue
		}
		indexed = append(indexed, t)
	}
	return full, indexed
}

// group is one top-level namespace's tools; name is "" for top-level functions.
type group struct {
	name  string
	tools []tool.Tool
}

// byNamespace groups tools by top-level namespace, in order of first
// appearance, keeping their order within each.
func byNamespace(tools []tool.Tool) []group {
	var groups []group
	pos := map[string]int{}
	for _, t := range tools {
		ns := tool.Namespace(t.Name)
		i, ok := pos[ns]
		if !ok {
			i = len(groups)
			pos[ns] = i
			groups = append(groups, group{name: ns})
		}
		groups[i].tools = append(groups[i].tools, t)
	}
	return groups
}

// quote marks text as quoted, line by line, so headings in it can't pose as
// steel's own sections.
func quote(text string) string {
	var b strings.Builder
	for _, line := range strings.Split(text, "\n") {
		b.WriteString(strings.TrimRight("> "+line, " ") + "\n")
	}
	return b.String()
}

// shortDesc reduces a description to its first sentence, at most limit
// bytes, for index entries: enough to pick the right tool, cheap enough to
// list many. The first call to an indexed tool shows the rest.
func shortDesc(desc string, limit int) string {
	d := strings.Join(strings.Fields(desc), " ")
	if i := strings.Index(d, ". "); i >= 0 {
		d = d[:i]
	}
	d = strings.TrimSuffix(d, ".")
	if len(d) > limit {
		cut := limit
		for cut > 0 && !utf8.RuneStart(d[cut]) {
			cut--
		}
		d = strings.TrimRight(d[:cut], " ,;:") + "…"
	}
	return d
}

// render declares tools as one TypeScript `tools` object.
func render(tools []tool.Tool) string {
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
