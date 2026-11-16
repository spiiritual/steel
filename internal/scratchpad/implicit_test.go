package scratchpad

import (
	"errors"
	"testing"
)

func TestImplicitReturn(t *testing.T) {
	for _, tc := range []struct{ code, want string }{
		{"await tools.t.echo({})", "return await tools.t.echo({})"},
		{"const a = 1;\na + 1", "const a = 1;\nreturn a + 1"},
		{"x = 5;", "return x = 5;"},
		{"// héllo ünïcode ✓\nlet s = 'ünï';\ns.length", "// héllo ünïcode ✓\nlet s = 'ünï';\nreturn s.length"},
		{"(1, 2)", "return (1, 2)"},
		{"return 1", "return 1"},                                  // explicit return wins
		{"if (a) return 1;\nb()", "if (a) return 1;\nreturn b()"}, // early return still works
		{"const y = 2", "const y = 2"},                            // declaration: unchanged
		{"for (;;) { break }", "for (;;) { break }"},              // block: unchanged
		{"xs.map(x => { return x })", "return xs.map(x => { return x })"},
		// Tagged templates report the wrong start; they must be left alone
		// rather than rewritten into different valid code.
		{"String.raw`C:\\x`", "String.raw`C:\\x`"},
		{"html`<b>`.trim()", "html`<b>`.trim()"},
		{"a\n`x`", "a\n`x`"},
		{"", ""},
	} {
		got := tc.code
		if body, err := parseBody(tc.code); err == nil {
			got = withImplicitReturn(tc.code, body)
		}
		if got != tc.want {
			t.Errorf("implicit return of %q = %q, want %q", tc.code, got, tc.want)
		}
	}
}

func TestPrepare(t *testing.T) {
	if _, err := prepare("}); 5; ({a:1"); !errors.Is(err, errEscapes) {
		t.Errorf("wrapper escape: err = %v, want errEscapes", err)
	}
	// Syntax errors pass through so the compiler reports them with positions.
	if src, err := prepare("return ("); err != nil || src != scriptPrefix+"return ("+scriptSuffix {
		t.Errorf("syntax error: src = %q, err = %v", src, err)
	}
}

func TestScriptReturnsLastExpression(t *testing.T) {
	p := New(testTools(), Options{})
	defer p.Close()

	if r := run(t, p, `await tools.t.echo({v: 1})`); r.Value != `echo {"v":1}` {
		t.Errorf("result = %+v", r)
	}
	if r := run(t, p, "const n = 2;\n// ünï\nn * 21"); r.Value != "42" {
		t.Errorf("result = %+v", r)
	}
}
