// Package mcp connects steel to MCP servers and adapts their tools into
// ordinary tool.Tools, so scripts call them as tools.<server>.<tool>(args)
// like any built-in.
package mcp

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/spiiritual/steel/internal/tool"
)

const (
	// connectTimeout bounds connecting to one server and listing its tools.
	connectTimeout = 30 * time.Second
	// maxInstructions caps one server's instructions shown to the model.
	maxInstructions = 2000
)

// Client holds connections to MCP servers.
type Client struct {
	servers []*server // sorted by name
}

type server struct {
	name         string
	ns           string // JS namespace: tools.<ns>.*
	session      *sdk.ClientSession
	instructions string
	tools        []tool.Tool
}

// Connect connects to all servers concurrently. A server that fails is
// skipped; it and any individual tools that couldn't be adapted are reported
// in the returned warnings. The client is usable with whatever connected.
func Connect(ctx context.Context, servers map[string]ServerConfig, lookup Lookup) (*Client, []error) {
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		c        = &Client{}
		warnings []error
	)
	for _, name := range slices.Sorted(maps.Keys(servers)) {
		wg.Go(func() {
			s, warn, err := dial(ctx, name, servers[name], lookup)
			mu.Lock()
			defer mu.Unlock()
			for _, w := range warn {
				warnings = append(warnings, fmt.Errorf("mcp server %q: %w", name, w))
			}
			if err != nil {
				warnings = append(warnings, fmt.Errorf("mcp server %q skipped: %w", name, err))
				return
			}
			c.servers = append(c.servers, s)
		})
	}
	wg.Wait()
	slices.SortFunc(c.servers, func(a, b *server) int { return cmp.Compare(a.name, b.name) })
	slices.SortFunc(warnings, func(a, b error) int { return cmp.Compare(a.Error(), b.Error()) })
	return c, warnings
}

// dial builds the transport for cfg and connects.
func dial(ctx context.Context, name string, cfg ServerConfig, lookup Lookup) (*server, []error, error) {
	cfg, err := cfg.expanded(lookup)
	if err != nil {
		return nil, nil, err
	}
	var stderr *tailBuffer
	var t sdk.Transport
	kind := cfg.Type
	if kind == "" {
		kind = "http"
		if cfg.Command != "" {
			kind = "stdio"
		}
	}
	switch kind {
	case "stdio":
		if cfg.Command == "" {
			return nil, nil, errors.New(`type "stdio" needs a "command"`)
		}
		cmd := exec.Command(cfg.Command, cfg.Args...)
		cmd.Env = serverEnv(cfg.Env)
		stderr = &tailBuffer{max: 4 << 10}
		cmd.Stderr = stderr
		// A helper process that inherited stderr would otherwise keep Wait
		// (and so Close) blocked until it exits.
		cmd.WaitDelay = time.Second
		t = &sdk.CommandTransport{Command: cmd}
	case "http", "sse":
		u, err := url.Parse(cfg.URL)
		if err != nil || u.Host == "" {
			return nil, nil, fmt.Errorf("type %q needs a valid \"url\"", kind)
		}
		base := http.DefaultTransport.(*http.Transport).Clone()
		hc := &http.Client{Transport: headerTransport{host: u.Host, headers: cfg.Headers, base: base}}
		if kind == "sse" {
			// The SSE stream's GET must answer promptly; bound it, since its
			// context is detached below.
			base.ResponseHeaderTimeout = connectTimeout
			t = detached{&sdk.SSEClientTransport{Endpoint: cfg.URL, HTTPClient: hc}}
		} else {
			t = &sdk.StreamableClientTransport{Endpoint: cfg.URL, HTTPClient: hc}
		}
	default:
		return nil, nil, fmt.Errorf("unknown type %q (want stdio, http, or sse)", kind)
	}
	s, warn, err := connect(ctx, name, t)
	if err != nil {
		err = redactURLs(err)
		if stderr != nil && stderr.Len() > 0 {
			err = fmt.Errorf("%w\nserver stderr:\n%s", err, strings.TrimSpace(stderr.String()))
		}
	}
	return s, warn, err
}

// detached connects its transport with a context that is never cancelled.
// The SDK's SSE transport ties its long-lived event stream to the Connect
// context, which would otherwise end with connect's timeout.
type detached struct{ sdk.Transport }

func (d detached) Connect(ctx context.Context) (sdk.Connection, error) {
	return d.Transport.Connect(context.WithoutCancel(ctx))
}

// connect initializes a session over t and adapts the server's tools. Tools
// that can't be named in JS are skipped with a warning; a server left with
// no tools is an error, since nothing would use the connection.
func connect(ctx context.Context, name string, t sdk.Transport) (*server, []error, error) {
	ns := ident(name)
	if ns == "" {
		return nil, nil, errors.New("server name has no usable characters")
	}
	ctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()

	// Declare exactly what steel supports: no roots, sampling, or elicitation.
	// Without explicit options the SDK advertises roots with listChanged.
	client := sdk.NewClient(&sdk.Implementation{Name: "steel", Version: "dev"}, &sdk.ClientOptions{Capabilities: &sdk.ClientCapabilities{}})
	session, err := client.Connect(ctx, t, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("connect: %w", err)
	}
	s := &server{name: name, ns: ns, session: session}
	init := session.InitializeResult()
	if init != nil {
		s.instructions = strings.TrimSpace(init.Instructions)
	}
	if init == nil || init.Capabilities == nil || init.Capabilities.Tools == nil {
		session.Close()
		return nil, nil, errors.New("it offers no tools")
	}

	var warnings []error
	seen := map[string]string{}
	for t, err := range session.Tools(ctx, nil) {
		if err != nil {
			session.Close()
			return nil, nil, fmt.Errorf("list tools: %w", err)
		}
		short := ident(t.Name)
		if short == "" {
			warnings = append(warnings, fmt.Errorf("tool %q skipped: its name has no characters usable in JS", t.Name))
			continue
		}
		if prev, dup := seen[short]; dup {
			warnings = append(warnings, fmt.Errorf("tool %q skipped: %q already uses the JS name %q", t.Name, prev, short))
			continue
		}
		seen[short] = t.Name
		s.tools = append(s.tools, adapt(session, ns+"."+short, t))
	}
	if len(s.tools) == 0 {
		session.Close()
		return nil, warnings, errors.New("it offers no usable tools")
	}
	return s, warnings, nil
}

// adapt turns one MCP tool into a tool.Tool. Without an output schema the
// result is the tool's text content as a string. With one, it's the
// structured content as a JS value, validated against the schema: the spec
// requires conforming structured results, and the generated TypeScript
// promises scripts that shape.
func adapt(session *sdk.ClientSession, name string, t *sdk.Tool) tool.Tool {
	structured := t.OutputSchema != nil
	validator := compileSchema(t.OutputSchema)
	inputValidator := compileSchema(t.InputSchema)
	signature := tool.SchemaSignature(t.InputSchema, t.OutputSchema, "string")
	return tool.Tool{
		Name:        name,
		Description: cmp.Or(t.Description, t.Title),
		Signature:   signature,
		Run: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var args map[string]any
			if err := json.Unmarshal(raw, &args); err != nil {
				return nil, tool.ArgsError(name, signature, err)
			}
			if args == nil {
				args = map[string]any{}
			}
			// Check arguments locally before calling: a wrong guess fails
			// fast with the signature in the error, and costs no request.
			if inputValidator != nil {
				plain, err := plainJSON(args)
				if err == nil {
					err = inputValidator.Validate(plain)
				}
				if err != nil {
					return nil, tool.ArgsError(name, signature, err)
				}
			}
			res, err := session.CallTool(ctx, &sdk.CallToolParams{Name: t.Name, Arguments: args})
			if err != nil {
				// Transport errors embed the endpoint URL, which may carry
				// credentials; they reach the model and saved history.
				return nil, redactURLs(err)
			}
			text := contentText(res.Content)
			if res.IsError {
				return nil, errors.New(cmp.Or(text, "the tool reported an error"))
			}
			if !structured {
				if text == "" && res.StructuredContent != nil {
					return plainJSON(res.StructuredContent)
				}
				return text, nil
			}
			if res.StructuredContent == nil {
				return nil, errors.New("the server returned no structured result although the tool declares an output schema")
			}
			v, err := plainJSON(res.StructuredContent)
			if err != nil {
				return nil, fmt.Errorf("structured result: %w", err)
			}
			if validator != nil {
				if err := validator.Validate(v); err != nil {
					return nil, fmt.Errorf("the server's result doesn't match the tool's output schema: %w", err)
				}
			}
			return v, nil
		},
	}
}

// compileSchema compiles a JSON Schema for validation. It returns nil when
// there is no schema or it can't be used (unsupported draft, bad $ref):
// validation is then skipped rather than failing every call.
func compileSchema(schema any) *jsonschema.Resolved {
	if schema == nil {
		return nil
	}
	b, err := json.Marshal(schema)
	if err != nil {
		return nil
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(b, &s); err != nil {
		return nil
	}
	switch s.Schema {
	case "", "http://json-schema.org/draft-07/schema#", "https://json-schema.org/draft-07/schema#", "https://json-schema.org/draft/2020-12/schema":
	default:
		return nil // the validator supports only draft-07 and 2020-12
	}
	resolved, err := s.Resolve(nil)
	if err != nil {
		return nil
	}
	return resolved
}

// plainJSON converts v to plain JSON values (map[string]any, []any, float64,
// ...), which is what the validator expects and what scripts receive.
func plainJSON(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	err = json.Unmarshal(b, &out)
	return out, err
}

// contentText flattens MCP content into text. Non-text content becomes a
// short placeholder.
func contentText(content []sdk.Content) string {
	var parts []string
	for _, c := range content {
		switch c := c.(type) {
		case *sdk.TextContent:
			parts = append(parts, c.Text)
		case *sdk.ImageContent:
			parts = append(parts, fmt.Sprintf("[image: %s, %d bytes]", c.MIMEType, len(c.Data)))
		case *sdk.AudioContent:
			parts = append(parts, fmt.Sprintf("[audio: %s, %d bytes]", c.MIMEType, len(c.Data)))
		case *sdk.ResourceLink:
			parts = append(parts, fmt.Sprintf("[resource: %s]", c.URI))
		case *sdk.EmbeddedResource:
			if c.Resource != nil && c.Resource.Text != "" {
				parts = append(parts, c.Resource.Text)
			} else if c.Resource != nil {
				parts = append(parts, fmt.Sprintf("[resource: %s]", c.Resource.URI))
			}
		}
	}
	return strings.Join(parts, "\n")
}

// Tools returns every connected server's tools, grouped by server name.
func (c *Client) Tools() []tool.Tool {
	var out []tool.Tool
	for _, s := range c.servers {
		out = append(out, s.tools...)
	}
	return out
}

// Guidance returns each server's instructions, capped, keyed by the
// namespace its tools live in ("github" for tools.github.*). Servers without
// instructions are left out.
func (c *Client) Guidance() map[string]string {
	out := map[string]string{}
	for _, s := range c.servers {
		text := strings.TrimSpace(s.instructions)
		if text == "" {
			continue
		}
		if len(text) > maxInstructions {
			cut := maxInstructions
			for cut > 0 && !utf8.RuneStart(text[cut]) {
				cut--
			}
			text = text[:cut] + " [truncated]"
		}
		out[s.ns] = text
	}
	return out
}

// Close disconnects from every server concurrently, stopping stdio server
// processes.
func (c *Client) Close() {
	var wg sync.WaitGroup
	for _, s := range c.servers {
		wg.Go(func() { s.session.Close() })
	}
	wg.Wait()
}

var nonIdent = regexp.MustCompile(`[^\p{L}\p{N}_$]+`)

// ident turns a name into a JS identifier: "create-issue" → "create_issue".
// Letters and digits from any script are kept, so "搜索" stays usable. "$"
// is trimmed from the ends, so no namespace can collide with tools.$catalog.
func ident(name string) string {
	s := strings.Trim(nonIdent.ReplaceAllString(name, "_"), "_$")
	if s != "" && s[0] >= '0' && s[0] <= '9' {
		s = "_" + s
	}
	return s
}

var urlRE = regexp.MustCompile(`(https?://[^/\s"'?#]+)[^\s"']*`)

// redactURLs keeps only scheme and host of URLs in err's message: paths and
// queries of MCP endpoints often carry API keys.
func redactURLs(err error) error {
	msg := err.Error()
	red := urlRE.ReplaceAllString(msg, "$1/…")
	if red == msg {
		return err
	}
	return errors.New(red)
}

// headerTransport adds configured headers (e.g. Authorization) to requests
// for the configured host only, so redirects to other hosts don't receive
// them.
type headerTransport struct {
	host    string
	headers map[string]string
	base    http.RoundTripper
}

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if len(h.headers) > 0 && r.URL.Host == h.host {
		r = r.Clone(r.Context())
		for k, v := range h.headers {
			r.Header.Set(k, v)
		}
	}
	return h.base.RoundTrip(r)
}

// tailBuffer keeps the last max bytes written, for error reports.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if over := len(t.b) - t.max; over > 0 {
		t.b = t.b[over:]
	}
	return len(p), nil
}

func (t *tailBuffer) Len() int       { t.mu.Lock(); defer t.mu.Unlock(); return len(t.b) }
func (t *tailBuffer) String() string { t.mu.Lock(); defer t.mu.Unlock(); return string(t.b) }
