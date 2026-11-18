package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/spiiritual/steel/internal/scratchpad"
	"github.com/spiiritual/steel/internal/tool"
)

// startServer runs an in-process MCP server and connects a steel client.
func startServer(t *testing.T, name string) *Client {
	t.Helper()
	srv := sdk.NewServer(&sdk.Implementation{Name: "test", Version: "1"}, &sdk.ServerOptions{Instructions: "Use add for sums."})

	type addIn struct {
		A int `json:"a"`
		B int `json:"b"`
	}
	type addOut struct {
		Sum int `json:"sum"`
	}
	sdk.AddTool(srv, &sdk.Tool{Name: "add", Description: "Add two numbers."},
		func(_ context.Context, _ *sdk.CallToolRequest, in addIn) (*sdk.CallToolResult, addOut, error) {
			return nil, addOut{Sum: in.A + in.B}, nil
		})
	srv.AddTool(&sdk.Tool{Name: "say-hi", InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}}}`)},
		func(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			var args struct{ Name string }
			_ = json.Unmarshal(req.Params.Arguments, &args)
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "hi " + args.Name}}}, nil
		})
	srv.AddTool(&sdk.Tool{Name: "fail", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: "nope"}}}, nil
		})
	countSchema := json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`)
	srv.AddTool(&sdk.Tool{Name: "bad-shape", InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: countSchema},
		func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{StructuredContent: map[string]any{"n": "oops"}}, nil
		})
	srv.AddTool(&sdk.Tool{Name: "text-only", InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: countSchema},
		func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "n is 3"}}}, nil
		})

	ct, st := sdk.NewInMemoryTransports()
	ss, err := srv.Connect(context.Background(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	s, _, err := connect(context.Background(), name, ct)
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{servers: []*server{s}}
	t.Cleanup(c.Close)
	return c
}

func byName(tools []tool.Tool) map[string]tool.Tool {
	m := map[string]tool.Tool{}
	for _, t := range tools {
		m[t.Name] = t
	}
	return m
}

func TestAdaptedTools(t *testing.T) {
	c := startServer(t, "test-srv")
	tools := byName(c.Servers()[0].Tools)
	ctx := context.Background()

	add, ok := tools["test_srv.add"]
	if !ok {
		t.Fatalf("tools = %v", tools)
	}
	if add.Signature != "(args: { a: number; b: number }): Promise<{ sum: number }>" || add.Description != "Add two numbers." {
		t.Errorf("add = %q / %q", add.Signature, add.Description)
	}
	out, err := add.Run(ctx, json.RawMessage(`{"a":2,"b":3}`))
	if b, _ := json.Marshal(out); err != nil || string(b) != `{"sum":5}` {
		t.Errorf("add result = %s, %v", b, err)
	}

	hi := tools["test_srv.say_hi"] // dashes become underscores
	if hi.Signature != "(args: { name?: string }): Promise<string>" {
		t.Errorf("say_hi signature = %q", hi.Signature)
	}
	if out, err := hi.Run(ctx, json.RawMessage(`{"name":"ann"}`)); err != nil || out != "hi ann" {
		t.Errorf("say_hi = %v, %v", out, err)
	}

	if _, err := tools["test_srv.fail"].Run(ctx, json.RawMessage(`{}`)); err == nil || err.Error() != "nope" {
		t.Errorf("fail err = %v", err)
	}

	if got := c.Servers()[0].Guidance; got != "Use add for sums." {
		t.Errorf("guidance = %q", got)
	}
}

// End to end: an MCP tool is just another function in the scratchpad.
func TestMCPToolsInScratchpad(t *testing.T) {
	c := startServer(t, "math")
	pad := scratchpad.New(tool.NewRegistry(c.Servers()[0].Tools...), scratchpad.Options{})
	defer pad.Close()

	r, err := pad.Run(context.Background(), `
const [x, y] = await Promise.all([tools.math.add({a: 1, b: 2}), tools.math.add({a: 3, b: 4})]);
let msg;
try { await tools.math.fail() } catch (e) { msg = e.message }
x.sum + y.sum + " " + msg`)
	if err != nil || r.Value != "10 math.fail: nope" {
		t.Errorf("result = %+v, err = %v", r, err)
	}
}

func TestIdent(t *testing.T) {
	for in, want := range map[string]string{"create-issue": "create_issue", "my.server": "my_server", "2fa": "_2fa", "--x--": "x", "ok_name": "ok_name", "$catalog": "catalog", "$2fa": "_2fa"} {
		if got := ident(in); got != want {
			t.Errorf("ident(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStructuredResultsAreValidated(t *testing.T) {
	tools := byName(startServer(t, "srv").Servers()[0].Tools)
	ctx := context.Background()

	if _, err := tools["srv.bad_shape"].Run(ctx, json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "doesn't match the tool's output schema") {
		t.Errorf("wrong shape accepted: %v", err)
	}
	if _, err := tools["srv.text_only"].Run(ctx, json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "no structured result") {
		t.Errorf("missing structured result accepted: %v", err)
	}
	// A conforming result passes (and arrives as plain JSON values).
	out, err := tools["srv.add"].Run(ctx, json.RawMessage(`{"a":1,"b":1}`))
	if m, ok := out.(map[string]any); err != nil || !ok || m["sum"] != float64(2) {
		t.Errorf("valid result = %#v, %v", out, err)
	}
}

func TestCompileSchemaSkipsUnsupported(t *testing.T) {
	if compileSchema(map[string]any{"$schema": "http://json-schema.org/draft-04/schema#", "type": "object"}) != nil {
		t.Error("draft-04 schema should skip validation")
	}
	if compileSchema(map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"$ref": "#/nowhere"}}}) != nil {
		t.Error("unresolvable $ref should skip validation")
	}
	if compileSchema(map[string]any{"type": "object"}) == nil {
		t.Error("plain schema should compile")
	}
}
