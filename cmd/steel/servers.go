package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/spiiritual/steel/internal/mcp"
	"github.com/spiiritual/steel/internal/tool"
)

// serverTools lets the model add and remove MCP servers while steel runs:
// tools.steel.add_server, .remove_server and .servers. Changes are saved in
// config.json's mcpServers, so they last, and each needs the user's
// approval, since a server runs a program or reaches a URL with the
// user's secrets.
type serverTools struct {
	configPath string
	client     *mcp.Client
	registry   *tool.Registry
	lookup     mcp.Lookup

	mu sync.Mutex // one change to config.json at a time
}

func (st *serverTools) tools() []tool.Tool {
	type addArgs struct {
		Name    string            `json:"name" desc:"Short name, e.g. \"github\"; its tools become tools.<name>.*"`
		Command string            `json:"command,omitempty" desc:"For a local server: the program to run, e.g. \"npx\""`
		Args    []string          `json:"args,omitempty" desc:"The program's arguments, e.g. [\"-y\", \"@modelcontextprotocol/server-github\"]"`
		Env     map[string]string `json:"env,omitempty" desc:"Environment variables for the program; put a secret in as \"${NAME}\", e.g. {\"GITHUB_TOKEN\": \"${GITHUB_TOKEN}\"}"`
		URL     string            `json:"url,omitempty" desc:"For a remote server: its URL"`
		Headers map[string]string `json:"headers,omitempty" desc:"HTTP headers for a remote server; put a secret in as \"${NAME}\", e.g. {\"Authorization\": \"Bearer ${API_TOKEN}\"}"`
		Type    string            `json:"type,omitempty" desc:"\"stdio\", \"http\" or \"sse\"; default stdio with command, http with url"`
	}
	type added struct {
		Namespace string   `json:"namespace"`
		Tools     []string `json:"tools"`
		Warnings  []string `json:"warnings,omitempty"`
	}
	type nameArgs struct {
		Name string `json:"name" desc:"The server's name, as tools.steel.servers lists it"`
	}
	type listed struct {
		Name      string `json:"name"`
		Connected bool   `json:"connected"`
		Runs      string `json:"runs"`
	}
	return []tool.Tool{
		tool.New("steel.add_server",
			"Connect an MCP server and save it in steel's config, once the user approves; its tools are usable from the next script, with their documentation in this script's result. A server's secrets (tokens, keys) go in env or headers as \"${NAME}\", NAME being a secret from tools.secrets.list (ask for a missing one with tools.secrets.add); steel fills in the value, which you never see.",
			func(ctx context.Context, a addArgs) (added, error) {
				cfg := mcp.ServerConfig{Type: a.Type, Command: a.Command, Args: a.Args, Env: a.Env, URL: a.URL, Headers: a.Headers}
				s, warn, err := st.add(ctx, a.Name, cfg)
				if err != nil {
					return added{}, err
				}
				out := added{Namespace: "tools." + s.Namespace}
				for _, t := range s.Tools {
					out.Tools = append(out.Tools, t.Name)
				}
				for _, w := range warn {
					out.Warnings = append(out.Warnings, w.Error())
				}
				return out, nil
			}),
		tool.New("steel.remove_server",
			"Disconnect an MCP server and remove it from steel's config, once the user approves. Its tools stop working.",
			func(ctx context.Context, a nameArgs) (string, error) {
				return st.remove(ctx, a.Name)
			}),
		tool.New("steel.servers",
			"The MCP servers in steel's config, whether each is connected, and what it runs or reaches.",
			func(ctx context.Context, _ struct{}) ([]listed, error) {
				servers, err := st.saved()
				if err != nil {
					return nil, err
				}
				out := []listed{}
				for _, name := range slices.Sorted(maps.Keys(servers)) {
					out = append(out, listed{name, st.client.Connected(name), runs(servers[name])})
				}
				return out, nil
			}),
	}
}

// add asks the user, connects, adds the tools, documents them with the
// script's result, and saves the server. The lock isn't held while the
// user decides, so one unanswered approval doesn't hold up others.
func (st *serverTools) add(ctx context.Context, name string, cfg mcp.ServerConfig) (mcp.Server, []error, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return mcp.Server{}, nil, errors.New("give the server a name")
	}
	if ns := mcp.Namespace(name); st.registry.Taken(ns) {
		return mcp.Server{}, nil, fmt.Errorf("tools.%s is taken; pick another name", ns)
	}
	if err := st.notSaved(name); err != nil {
		return mcp.Server{}, nil, err
	}
	shown, _ := json.MarshalIndent(cfg, "", "  ")
	if err := tool.Approve(ctx, tool.Approval{
		Tool:    "steel.add_server",
		Action:  "Add the MCP server " + name + ":\n" + string(shown),
		Reasons: []string{runs(cfg), "is saved in config.json"},
	}); err != nil {
		return mcp.Server{}, nil, err
	}
	s, warn, err := st.client.Add(ctx, name, cfg, st.lookup)
	if err != nil {
		return mcp.Server{}, warn, fmt.Errorf("connecting to %s: %w", name, err)
	}
	if err := st.registry.Add(s.Namespace, s.Guidance, s.Tools...); err != nil {
		st.client.Remove(name)
		return mcp.Server{}, warn, err
	}
	st.mu.Lock()
	err = st.notSaved(name) // checked again: the config may have changed meanwhile
	if err == nil {
		err = st.save(name, cfg)
	}
	st.mu.Unlock()
	if err != nil {
		st.registry.Remove(s.Namespace)
		st.client.Remove(name)
		return mcp.Server{}, warn, err
	}
	names := make([]string, len(s.Tools))
	for i, t := range s.Tools {
		names[i] = t.Name
	}
	tool.Disclose(ctx, names...)
	return s, warn, nil
}

// notSaved fails if a server named name is in config.json.
func (st *serverTools) notSaved(name string) error {
	servers, err := st.saved()
	if err != nil {
		return err
	}
	if _, ok := servers[name]; ok {
		return fmt.Errorf("a server named %q is in the config already; remove it first to replace it", name)
	}
	return nil
}

// remove asks the user, drops the server from the config, and disconnects.
func (st *serverTools) remove(ctx context.Context, name string) (string, error) {
	servers, err := st.saved()
	if err != nil {
		return "", err
	}
	cfg, saved := servers[name]
	if !saved && !st.client.Connected(name) {
		return "", fmt.Errorf("no server named %q; tools.steel.servers lists them", name)
	}
	action := "Remove the MCP server " + name + " (" + runs(cfg) + ")"
	if !saved {
		action = "Disconnect the MCP server " + name + ", which is no longer in config.json"
	}
	if err := tool.Approve(ctx, tool.Approval{
		Tool:    "steel.remove_server",
		Action:  action,
		Reasons: []string{"is removed from config.json"},
	}); err != nil {
		return "", err
	}
	st.mu.Lock()
	err = st.save(name, nil)
	st.mu.Unlock()
	if err != nil {
		return "", err
	}
	if st.client.Remove(name) {
		// Only a namespace a server was given goes; steel's own stay.
		st.registry.Remove(mcp.Namespace(name))
	}
	return "Removed " + name + ".", nil
}

// saved reads the servers in config.json as it is now.
func (st *serverTools) saved() (map[string]mcp.ServerConfig, error) {
	c, err := readConfig(st.configPath)
	if err != nil {
		return nil, err
	}
	return c.mcpServers()
}

// save writes server name's config, cfg, or removes it if cfg is nil,
// keeping the rest of config.json as it is now. Requires st.mu.
func (st *serverTools) save(name string, cfg any) error {
	c, err := readConfig(st.configPath)
	if err != nil {
		return err
	}
	if err := c.put([]string{"mcpServers", name}, cfg); err != nil {
		return err
	}
	return c.write()
}

// runs says what a server config runs or reaches, for the user.
func runs(cfg mcp.ServerConfig) string {
	if cfg.Command != "" {
		return "runs " + strings.TrimSpace(cfg.Command+" "+strings.Join(cfg.Args, " "))
	}
	return "connects to " + cfg.URL
}
