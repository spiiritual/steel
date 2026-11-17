package mcp

import (
	"fmt"
	"os"
	"strings"
)

// ServerConfig is one entry of the standard "mcpServers" config block, as
// used by Claude Desktop, Claude Code, and Cursor. Every string field
// supports ${VAR} and ${VAR:-default} expansion from steel's environment.
type ServerConfig struct {
	// Type is "stdio", "http", or "sse". Empty means stdio when Command is
	// set and http when URL is set.
	Type    string            `json:"type,omitempty"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// expanded returns cfg with ${VAR} references expanded in every field.
func (cfg ServerConfig) expanded() (ServerConfig, error) {
	var err error
	ex := func(s string) string {
		if err != nil {
			return s
		}
		var out string
		out, err = expandVars(s)
		return out
	}
	out := ServerConfig{Type: cfg.Type, Command: ex(cfg.Command), URL: ex(cfg.URL)}
	for _, a := range cfg.Args {
		out.Args = append(out.Args, ex(a))
	}
	if cfg.Env != nil {
		out.Env = map[string]string{}
		for k, v := range cfg.Env {
			out.Env[k] = ex(v)
		}
	}
	if cfg.Headers != nil {
		out.Headers = map[string]string{}
		for k, v := range cfg.Headers {
			out.Headers[k] = ex(v)
		}
	}
	return out, err
}

// expandVars expands ${VAR} and ${VAR:-default}, as Claude Code does. Any
// other "$" is literal, so tokens like "tok$en" survive. A ${VAR} that is
// unset and has no default is an error rather than a silent empty string.
func expandVars(s string) (string, error) {
	var b strings.Builder
	for {
		i := strings.Index(s, "${")
		if i < 0 {
			b.WriteString(s)
			return b.String(), nil
		}
		end := strings.IndexByte(s[i:], '}')
		if end < 0 {
			b.WriteString(s)
			return b.String(), nil
		}
		b.WriteString(s[:i])
		expr := s[i+2 : i+end]
		name, def, hasDef := strings.Cut(expr, ":-")
		val, set := os.LookupEnv(name)
		switch {
		case hasDef && val == "":
			val = def
		case !set:
			return "", fmt.Errorf("environment variable %s is not set (use ${%s:-default} for a fallback)", name, name)
		}
		b.WriteString(val)
		s = s[i+end+1:]
	}
}

// baseEnv is the environment stdio servers get, plus their configured env:
// the same allowlist the reference MCP SDKs use, so secrets in steel's own
// environment (like OPENAI_API_KEY) don't leak into every server.
var baseEnv = []string{"HOME", "LOGNAME", "PATH", "SHELL", "TERM", "USER"}

func serverEnv(extra map[string]string) []string {
	var env []string
	for _, k := range baseEnv {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}
