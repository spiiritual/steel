package scratchpad

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spiiritual/steel/internal/tool"
)

func testTools() *tool.Registry {
	return tool.NewRegistry(
		tool.Tool{Name: "t.echo", Run: func(_ context.Context, a json.RawMessage) (any, error) { return "echo " + string(a), nil }},
		tool.Tool{Name: "t.fail", Run: func(context.Context, json.RawMessage) (any, error) { return nil, errors.New("boom") }},
		tool.Tool{Name: "t.sleep", Run: func(ctx context.Context, _ json.RawMessage) (any, error) {
			select {
			case <-time.After(200 * time.Millisecond):
				return "slept", nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}},
		tool.Tool{Name: "t.stat", Run: func(context.Context, json.RawMessage) (any, error) {
			return struct {
				Size  int      `json:"size"`
				Lines []string `json:"lines"`
			}{3, []string{"a", "b"}}, nil
		}},
	)
}

func TestStructuredToolResult(t *testing.T) {
	p := New(testTools(), Options{})
	defer p.Close()

	// Go results arrive as plain JS objects keyed by their json tags.
	r := run(t, p, `const s = await tools.t.stat(); return s.size + s.lines.length + (s.lines instanceof Array ? "array" : "?")`)
	if r.Error != "" || r.Value != "5array" {
		t.Errorf("result = %+v", r)
	}
}

func run(t *testing.T, p *Pad, code string) Result {
	t.Helper()
	r, err := p.Run(context.Background(), code)
	if err != nil {
		t.Fatalf("Run(%q): %v", code, err)
	}
	return r
}

func TestStatePersistsAcrossCells(t *testing.T) {
	p := New(testTools(), Options{})
	defer p.Close()

	if r := run(t, p, `x = 41; globalThis.y = 1; const local = 5; return {a: [1, 2]}`); r.Error != "" || r.Value != "{\n  \"a\": [\n    1,\n    2\n  ]\n}" {
		t.Fatalf("cell 1 = %+v", r)
	}
	if r := run(t, p, `return x + y`); r.Value != "42" {
		t.Errorf("globals not kept: %+v", r)
	}
	if r := run(t, p, `return typeof local`); r.Value != "undefined" {
		t.Errorf("const leaked across cells: %+v", r)
	}
}

func TestHelperFromEarlierCellUsesToolsAndConsole(t *testing.T) {
	p := New(testTools(), Options{})
	defer p.Close()

	run(t, p, `globalThis.h = async () => { console.log("in h"); return await tools.t.echo({v: 1}) }`)
	r := run(t, p, `return await h()`)
	if r.Error != "" || r.Value != `echo {"v":1}` {
		t.Errorf("result = %+v", r)
	}
	if len(r.Logs) != 1 || r.Logs[0] != "in h" {
		t.Errorf("logs = %q, want [in h] attributed to the calling cell", r.Logs)
	}
}

func TestToolsRunInParallel(t *testing.T) {
	p := New(testTools(), Options{})
	defer p.Close()

	start := time.Now()
	r := run(t, p, `return (await Promise.all([tools.t.sleep(), tools.t.sleep(), tools.t.sleep()])).join(",")`)
	if r.Value != "slept,slept,slept" {
		t.Fatalf("result = %+v", r)
	}
	if d := time.Since(start); d > 450*time.Millisecond {
		t.Errorf("3×200ms calls took %v; not parallel", d)
	}
}

func TestErrors(t *testing.T) {
	p := New(testTools(), Options{})
	defer p.Close()

	if r := run(t, p, `try { await tools.t.fail() } catch (e) { return "caught: " + e.message }`); r.Value != "caught: t.fail: boom" {
		t.Errorf("caught tool error = %+v", r)
	}
	if r := run(t, p, "const a = 1;\nnull.x"); !strings.Contains(r.Error, "cell:2") {
		t.Errorf("uncaught error should point at line 2: %+v", r)
	}
	if r := run(t, p, `return (`); r.Error == "" {
		t.Errorf("syntax error not reported: %+v", r)
	}
	if r := run(t, p, `return await tools.t.echo("nope")`); !strings.Contains(r.Error, "must be an object") {
		t.Errorf("bad args not rejected: %+v", r)
	}
	if r := run(t, p, `return 1`); r.Value != "1" {
		t.Errorf("pad unusable after errors: %+v", r)
	}
}

func TestTimeoutResetsPad(t *testing.T) {
	p := New(testTools(), Options{Timeout: 300 * time.Millisecond})
	defer p.Close()

	run(t, p, `globalThis.keep = 1`)
	start := time.Now()
	r := run(t, p, `console.log("spinning"); while (true) {}`)
	if !strings.Contains(r.Error, "timed out") {
		t.Fatalf("result = %+v", r)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("timeout took %v", d)
	}
	if len(r.Logs) != 1 || r.Logs[0] != "spinning" {
		t.Errorf("logs before timeout lost: %q", r.Logs)
	}
	if r := run(t, p, `return typeof keep`); r.Value != "undefined" {
		t.Errorf("state survived reset: %+v", r)
	}
}
