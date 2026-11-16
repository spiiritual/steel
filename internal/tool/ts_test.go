package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type inner struct {
	URL string `json:"url"`
}

type node struct {
	Name     string  `json:"name"`
	Children []*node `json:"children"`
}

type embedded struct {
	ID int `json:"id"`
}

func TestSignatureFromTypes(t *testing.T) {
	type args struct {
		embedded
		Path    string            `json:"path" desc:"File path"`
		Limit   *int              `json:"limit"`
		Tags    []string          `json:"tags,omitempty"`
		Meta    map[string]int    `json:"meta"`
		Raw     []byte            `json:"raw"`
		When    time.Time         `json:"when"`
		Hits    []*inner          `json:"hits"`
		Odd     string            `json:"site-name"`
		Any     any               `json:"any"`
		Skip    string            `json:"-"`
		private string            //nolint:unused
		Nested  struct{ Ok bool } `json:"nested"`
	}
	got := New("x.y", "", func(context.Context, args) (node, error) { return node{}, nil }).Signature
	want := `(args: {
  id: number;
  /** File path */
  path: string;
  limit?: number;
  tags?: string[];
  meta: Record<string, number>;
  raw: string;
  when: string;
  hits: ({ url: string } | null)[];
  "site-name": string;
  any: unknown;
  nested: { Ok: boolean };
}): Promise<{ name: string; children: (unknown | null)[] }>`
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}

	if got := New("x.z", "", func(context.Context, struct{}) (string, error) { return "", nil }).Signature; got != "(): Promise<string>" {
		t.Errorf("no-arg signature = %q", got)
	}
}

func TestRunDecodesStrictly(t *testing.T) {
	type args struct {
		Path string `json:"path"`
	}
	tl := New("t", "", func(_ context.Context, a args) (string, error) { return "got " + a.Path, nil })

	if out, err := tl.Run(context.Background(), json.RawMessage(`{"path":"a"}`)); err != nil || out != "got a" {
		t.Errorf("Run = %v, %v", out, err)
	}
	if _, err := tl.Run(context.Background(), json.RawMessage(`{"file":"a"}`)); err == nil || !strings.Contains(err.Error(), `unknown field "file"`) {
		t.Errorf("typo'd field not rejected: %v", err)
	}
}
