package scratchpad

import (
	"strings"
	"testing"

	"github.com/spiiritual/steel/internal/tool"
)

func TestLayoutDeclaresEverythingWithinBudget(t *testing.T) {
	l := newLayout([]tool.Tool{
		{Name: "fs.read_file", Description: "Read a file.", Signature: "(args: { path: string }): Promise<string>"},
		{Name: "web.search", Description: "Search.\nReturns ranked hits.", Signature: "(args: { q: string }): Promise<{ url: string }[]>"},
		{Name: "fs.list", Signature: "(): Promise<string[]>"},
		{Name: "my-ns.do", Signature: "(): Promise<void>"},
	}, 0, nil, nil)
	want := `declare const tools: {
  fs: {
    /** Read a file. */
    read_file(args: { path: string }): Promise<string>;
    list(): Promise<string[]>;
  };
  web: {
    /**
     * Search.
     * Returns ranked hits.
     */
    search(args: { q: string }): Promise<{ url: string }[]>;
  };
  "my-ns": {
    do(): Promise<void>;
  };
};
`
	if l.decl != want || l.index != "" || len(l.docs) != 0 {
		t.Errorf("got:\n%s\nwant:\n%s\nindex: %q", l.decl, want, l.index)
	}
}

func TestLayoutOverBudgetIndexesCompactly(t *testing.T) {
	tools := []tool.Tool{
		{Name: "fs.read_file", Signature: "(args: { path: string }): Promise<string>"},
		{Name: "github.create_issue", Description: strings.Repeat("long ", 40), Signature: "(args: {\n  /** Issue title */\n  title: string;\n}): Promise<{ id: number }>"},
		{Name: "github.list_issues", Signature: "(): Promise<string>"},
		{Name: "tiny.ping", Signature: "(): Promise<string>"},
	}
	// Room for fs and tiny, not github: namespaces fill in order, and a later
	// small one still gets full declarations after a big one is indexed.
	budget := len(render(tools[:1])) + len(render(tools[3:])) + 1
	l := newLayout(tools, budget, nil, map[string]string{
		"fs":     "Paths are relative.",
		"github": "Use the GitHub tools for repos. Search before creating issues.",
	})
	if !strings.Contains(l.decl, "read_file(") || !strings.Contains(l.decl, "ping(") || strings.Contains(l.decl, "github") {
		t.Errorf("declared:\n%s", l.decl)
	}
	// Fully declared namespaces get their guidance upfront, quoted.
	if l.notes != "tools.fs.*:\n> Paths are relative.\n\n" {
		t.Errorf("notes = %q", l.notes)
	}
	// Indexed namespaces get a one-line guidance summary; indexed tools a
	// name and short description. The rest waits for the first call.
	want := "tools.github: Use the GitHub tools for repos\n" +
		"  create_issue — " + strings.TrimSpace(strings.Repeat("long ", 16)) + "…\n" +
		"  list_issues\n"
	if l.index != want {
		t.Errorf("index = %q\nwant    %q", l.index, want)
	}
	if !strings.Contains(l.docs["github.create_issue"], "Issue title") || !strings.Contains(l.nsNotes["github"], "Search before creating issues.") {
		t.Errorf("docs = %q, notes = %q", l.docs, l.nsNotes)
	}
}

func TestLayoutRecentlyUsedFirst(t *testing.T) {
	tools := []tool.Tool{
		{Name: "a.one", Signature: "(): Promise<string>"},
		{Name: "b.one", Signature: "(): Promise<string>"},
		{Name: "b.two", Signature: "(): Promise<string>"},
	}
	// Room for one namespace: the recently used one gets it, and its tools
	// are listed most recent first.
	l := newLayout(tools, len(render(tools[1:]))+1, []string{"b.two", "b.one"}, nil)
	if !strings.Contains(l.decl, "b: {\n    two(") || strings.Contains(l.decl, "a: {") || l.index != "tools.a:\n  one\n" {
		t.Errorf("decl:\n%s\nindex: %q", l.decl, l.index)
	}
}
