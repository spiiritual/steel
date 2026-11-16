package scratchpad

import (
	"errors"

	"github.com/dop251/goja/ast"
	"github.com/dop251/goja/parser"
)

// Scripts run as the body of this async function.
const (
	scriptPrefix = "(async (console, log) => {"
	scriptSuffix = "\n})"
)

var (
	errSyntax  = errors.New("syntax error")
	errEscapes = errors.New("the code closes the function it runs in; check for unbalanced braces or parentheses")
)

// prepare wraps code as the script function, applying the implicit return.
// Code with a syntax error is wrapped as-is so the compiler reports it with
// positions; code that parses but escapes the wrapper is rejected.
func prepare(code string) (string, error) {
	body, err := parseBody(code)
	switch {
	case errors.Is(err, errSyntax):
		return scriptPrefix + code + scriptSuffix, nil
	case err != nil:
		return "", err
	}
	return scriptPrefix + withImplicitReturn(code, body) + scriptSuffix, nil
}

// parseBody parses code as the script function's body and returns its
// top-level statements.
func parseBody(code string) ([]ast.Statement, error) {
	prog, err := parser.ParseFile(nil, "", scriptPrefix+code+scriptSuffix, 0)
	if err != nil {
		return nil, errSyntax
	}
	if len(prog.Body) != 1 {
		return nil, errEscapes
	}
	es, ok := prog.Body[0].(*ast.ExpressionStatement)
	if !ok {
		return nil, errEscapes
	}
	fn, ok := es.Expression.(*ast.ArrowFunctionLiteral)
	if !ok {
		return nil, errEscapes
	}
	block, ok := fn.Body.(*ast.BlockStatement)
	if !ok {
		return nil, errEscapes
	}
	return block.List, nil
}

// withImplicitReturn makes a script return the value of its last line when
// that line is an expression, the way a REPL shows the last value. A last
// line that is already a return, a declaration, or a block is left alone.
// The rewrite is kept only if it reparses to the same statements with the
// last one now returning exactly the original expression; otherwise (for
// example tagged templates, whose reported start is wrong) the script runs
// unchanged.
func withImplicitReturn(code string, body []ast.Statement) string {
	if len(body) == 0 {
		return code
	}
	stmt, ok := body[len(body)-1].(*ast.ExpressionStatement)
	if !ok {
		return code
	}
	off := int(stmt.Idx0()) - 1 - len(scriptPrefix)
	if off < 0 || off > len(code) {
		return code
	}
	// The parser drops parentheses, so for "(a, b)" the start points inside
	// them. Step back over "(" (and spaces between them) to the first one.
	i := off
	for i > 0 && (code[i-1] == '(' || code[i-1] == ' ' || code[i-1] == '\t') {
		i--
	}
	for i < off && code[i] != '(' {
		i++
	}
	off = i

	const ret = "return "
	out := code[:off] + ret + code[off:]
	nb, err := parseBody(out)
	if err != nil || len(nb) != len(body) {
		return code
	}
	r, ok := nb[len(nb)-1].(*ast.ReturnStatement)
	if !ok || r.Argument == nil || int(r.Argument.Idx1()) != int(stmt.Idx1())+len(ret) {
		return code
	}
	return out
}
