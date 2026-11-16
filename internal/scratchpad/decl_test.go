package scratchpad

import (
	"testing"

	"github.com/spiiritual/steel/internal/tool"
)

func TestDeclarations(t *testing.T) {
	got := Declarations([]tool.Tool{
		{Name: "fs.read_file", Description: "Read a file.", Signature: "(args: { path: string }): Promise<string>"},
		{Name: "web.search", Description: "Search.\nReturns ranked hits.", Signature: "(args: { q: string }): Promise<{ url: string }[]>"},
		{Name: "fs.list", Signature: "(): Promise<string[]>"},
		{Name: "my-ns.do", Signature: "(): Promise<void>"},
	})
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
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}
