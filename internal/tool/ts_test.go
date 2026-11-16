package tool

import (
	"context"
	"encoding/json"
	"fmt"
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
  limit: number | null;
  tags?: string[];
  meta: Record<string, number> | null;
  raw: string | null;
  when: string;
  hits: ({ url: string } | null)[] | null;
  "site-name": string;
  any: unknown;
  nested: { Ok: boolean };
}): Promise<{ name: string; children: (unknown | null)[] | null }>`
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}

	if got := New("x.z", "", func(context.Context, struct{}) (string, error) { return "", nil }).Signature; got != "(): Promise<string>" {
		t.Errorf("no-arg signature = %q", got)
	}
}

// level encodes as JSON text through a value-receiver MarshalText.
type level int

func (l level) MarshalText() ([]byte, error) { return []byte(fmt.Sprintf("L%d", int(l))), nil }

// TestSignatureMatchesEncoding pins each typing rule against what
// encoding/json actually produces for the zero value.
func TestSignatureMatchesEncoding(t *testing.T) {
	type res struct {
		Sum    [4]byte           `json:"sum"`   // arrays aren't base64
		Bytes  []byte            `json:"bytes"` // base64 string, nil is null
		P      *int              `json:"p"`     // nil pointer is null
		PO     *int              `json:"po,omitempty"`
		PZ     *int              `json:"pz,omitzero"`
		PS     *[]int            `json:"ps,omitempty"` // present pointer to nil slice is null
		L      []int             `json:"l"`
		LO     []int             `json:"lo,omitempty"`
		LL     [][]int           `json:"ll"`
		M      map[string]int    `json:"m"`
		MO     map[string]int    `json:"mo,omitempty"`
		Lvl    level             `json:"lvl"`    // TextMarshaler
		Lvls   []level           `json:"lvls"`   // TextMarshaler elements
		LvlMap map[string]*level `json:"lvlMap"` // pointer values admit null
		When   time.Time         `json:"when"`
	}
	got := New("x.y", "", func(context.Context, struct{}) (res, error) { return res{}, nil }).Signature
	want := "(): Promise<{ sum: number[]; bytes: string | null; p: number | null; po?: number; pz?: number; " +
		"ps?: number[] | null; l: number[] | null; lo?: number[]; ll: (number[] | null)[] | null; " +
		"m: Record<string, number> | null; mo?: Record<string, number>; lvl: string; lvls: string[] | null; " +
		"lvlMap: Record<string, string | null> | null; when: string }>"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}

	enc, err := json.Marshal(res{})
	if err != nil {
		t.Fatal(err)
	}
	wantEnc := `{"sum":[0,0,0,0],"bytes":null,"p":null,"l":null,"ll":null,"m":null,"lvl":"L0","lvls":null,"lvlMap":null,"when":"0001-01-01T00:00:00Z"}`
	if string(enc) != wantEnc {
		t.Errorf("encoding drifted from the rules above:\n%s\nwant:\n%s", enc, wantEnc)
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
