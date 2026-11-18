package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/spiiritual/steel/internal/mcp"
	"github.com/spiiritual/steel/internal/secret"
	"github.com/spiiritual/steel/internal/tool"
)

// A server added at runtime asks the user first, gets a secret entered
// after steel started through ${NAME}, adds its tools, and is saved in
// config.json beside what's there; removing it undoes all three.
func TestAddAndRemoveServer(t *testing.T) {
	srv := sdk.NewServer(&sdk.Implementation{Name: "ping", Version: "1"}, nil)
	srv.AddTool(&sdk.Tool{Name: "ping", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "pong"}}}, nil
		})
	var auth atomic.Value
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return srv }, nil)
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.Store(r.Header.Get("Authorization"))
		handler.ServeHTTP(w, r)
	}))
	defer hs.Close()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	os.WriteFile(configPath, []byte(`{"model":"m"}`), 0o600)
	store, err := secret.Open(filepath.Join(dir, "secrets.env"), secret.Options{})
	if err != nil {
		t.Fatal(err)
	}
	store.Set("PING_TOKEN", "tok-123") // entered after startup

	client, _ := mcp.Connect(context.Background(), nil, nil)
	defer client.Close()
	st := &serverTools{configPath: configPath, client: client, lookup: store.Value}
	st.registry = tool.NewRegistry(st.tools()...)

	var asked []tool.Approval
	ctx := tool.WithApprover(context.Background(), func(_ context.Context, a tool.Approval) (bool, error) {
		asked = append(asked, a)
		return true, nil
	})
	args := `{"name":"ping","url":"` + hs.URL + `","headers":{"Authorization":"Bearer ${PING_TOKEN}"}}`
	out, err := st.registry.Call(ctx, "steel.add_server", json.RawMessage(args))
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(out); !strings.Contains(string(b), `"tools":["ping.ping"]`) {
		t.Errorf("add_server returned %s", b)
	}
	if len(asked) != 1 || !strings.Contains(asked[0].Action, "${PING_TOKEN}") || strings.Contains(asked[0].Action, "tok-123") {
		t.Errorf("approval %+v", asked)
	}
	if got, _ := st.registry.Call(ctx, "ping.ping", json.RawMessage(`{}`)); got != "pong" {
		t.Errorf("ping.ping = %v", got)
	}
	if got := auth.Load(); got != "Bearer tok-123" {
		t.Errorf("server got Authorization %v", got)
	}
	b, _ := os.ReadFile(configPath)
	if !strings.Contains(string(b), `"model": "m"`) || !strings.Contains(string(b), "${PING_TOKEN}") || strings.Contains(string(b), "tok-123") {
		t.Errorf("config.json:\n%s", b)
	}

	if _, err := st.registry.Call(ctx, "steel.add_server", json.RawMessage(`{"name":"steel","url":"`+hs.URL+`"}`)); err == nil || !strings.Contains(err.Error(), "taken") {
		t.Errorf("a server took steel's own namespace: %v", err)
	}

	if _, err := st.registry.Call(ctx, "steel.remove_server", json.RawMessage(`{"name":"ping"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.registry.Call(ctx, "ping.ping", json.RawMessage(`{}`)); err == nil {
		t.Error("removed server's tool still runs")
	}
	if b, _ := os.ReadFile(configPath); strings.Contains(string(b), "mcpServers") {
		t.Errorf("config.json after removal:\n%s", b)
	}
}
