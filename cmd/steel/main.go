// Command steel is a personal agent harness.
package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spiiritual/steel/internal/agent"
	"github.com/spiiritual/steel/internal/channel/telegram"
	"github.com/spiiritual/steel/internal/channel/terminal"
	"github.com/spiiritual/steel/internal/llm/openai"
	"github.com/spiiritual/steel/internal/mcp"
	"github.com/spiiritual/steel/internal/scratchpad"
	"github.com/spiiritual/steel/internal/tool"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "steel:", err)
		os.Exit(1)
	}
}

func run() error {
	if ok, err := subcommand(os.Args[1:]); ok {
		return err
	}
	home, _ := os.UserHomeDir()
	baseURL := flag.String("base-url", cmp.Or(os.Getenv("OPENAI_BASE_URL"), openai.DefaultBaseURL), "OpenAI-compatible API base URL (env OPENAI_BASE_URL)")
	model := flag.String("model", os.Getenv("OPENAI_MODEL"), "model name (env OPENAI_MODEL)")
	system := flag.String("system", "", "system prompt")
	configPath := flag.String("config", filepath.Join(home, ".steel", "config.json"), `config file; its "mcpServers" block uses the same format as Claude Desktop and Claude Code`)
	dataDir := flag.String("data", filepath.Join(home, ".steel", "sessions"), "directory for saved conversation history")
	idle := flag.Duration("idle", 30*time.Minute, "close a session's scratchpad after this much inactivity")
	toolBudget := flag.Int("tool-budget", 4000, "approximate tokens of full tool declarations shown to the model; namespaces beyond it get one line per tool, documented on first use (0 = index every namespace, negative = no limit)")
	drain := flag.Duration("drain", time.Minute, "on SIGTERM, how long to let running turns and background jobs finish before exiting; keep it under your service manager's stop timeout")
	showVersion := flag.Bool("version", false, "print the version and exit")
	channelName := flag.String("channel", "terminal", `where to chat: "terminal", or "telegram" (bot token in env TELEGRAM_BOT_TOKEN; allowed user IDs in the config's "telegram": {"allowedUsers": [...]})`)
	flag.Parse()
	if *showVersion {
		fmt.Println("steel", versionString())
		return nil
	}
	fmt.Fprintln(os.Stderr, "steel", versionString())

	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}

	if *model == "" {
		return errors.New("no model: pass -model or set OPENAI_MODEL")
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	var ch interface {
		Emit(id string, e agent.Event)
		Run(ctx context.Context, ss *agent.Sessions) error
	}
	switch *channelName {
	case "terminal":
		ch = terminal.New(os.Stdin, os.Stdout)
	case "telegram":
		// Trimmed, since token files often end in a newline. Removed from
		// the environment so tools (shell.bash) can't read it.
		token := strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN"))
		os.Unsetenv("TELEGRAM_BOT_TOKEN")
		if token == "" {
			return errors.New("-channel telegram needs the bot token in TELEGRAM_BOT_TOKEN")
		}
		bot := telegram.New(telegram.Options{Token: token, AllowedUsers: cfg.Telegram.AllowedUsers})
		defer bot.Close() // runs after sessions.Close, so their last messages go out
		ch = bot
	default:
		return fmt.Errorf("unknown -channel %q: want terminal or telegram", *channelName)
	}

	// Ctrl-C quits now, cancelling running work. SIGTERM, which service
	// managers send, stops taking input and lets running work finish first.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	runCtx, stopRun := signal.NotifyContext(ctx, syscall.SIGTERM)
	defer stopRun()

	servers, warnings := mcp.Connect(runCtx, cfg.MCPServers) // either signal interrupts a slow server
	defer servers.Close()
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, "steel:", w)
	}

	builtin := []tool.Tool{tool.ReadFile(cwd), tool.Bash(cwd)}
	tools := tool.NewRegistry(withoutCollisions(builtin, servers.Tools())...)
	// recent.json sits next to the config, so a test config elsewhere keeps
	// its own history.
	recent, err := tool.LoadRecent(filepath.Join(filepath.Dir(*configPath), "recent.json"), func(err error) {
		fmt.Fprintln(os.Stderr, "steel: saving recently used tools:", err)
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "steel: ignoring recently used tools:", err)
	}
	tools.TrackRecent(recent)
	// A server whose namespace a builtin owns had its tools skipped; its
	// guidance would otherwise describe the builtin.
	guidance := servers.Guidance()
	for _, t := range builtin {
		delete(guidance, namespace(t.Name))
	}
	sessions, err := agent.NewSessions(agent.Config{
		Provider:    openai.New(*baseURL, os.Getenv("OPENAI_API_KEY")),
		Model:       *model,
		System:      *system,
		NewToolbox:  func(h agent.Hooks) agent.Toolbox { return newPad(tools, guidance, *toolBudget, h) },
		Dir:         *dataDir,
		IdleTimeout: *idle,
	}, ch.Emit)
	if err != nil {
		return err
	}
	defer sessions.Close() // cancels whatever is still running

	err = ch.Run(runCtx, sessions)
	if runCtx.Err() != nil && ctx.Err() == nil { // SIGTERM, not Ctrl-C
		fmt.Fprintf(os.Stderr, "steel: finishing running work (up to %s; SIGTERM or Ctrl-C again to quit now)\n", *drain)
		again, stopAgain := signal.NotifyContext(ctx, syscall.SIGTERM)
		drainCtx, cancel := context.WithTimeout(again, *drain)
		if derr := sessions.Drain(drainCtx); derr != nil {
			fmt.Fprintln(os.Stderr, "steel: cancelling unfinished work:", derr)
		}
		cancel()
		stopAgain()
	}
	return err
}

// config is steel's config file.
type config struct {
	MCPServers map[string]mcp.ServerConfig `json:"mcpServers"`
	Telegram   struct {
		AllowedUsers []int64 `json:"allowedUsers"`
	} `json:"telegram"`
}

// loadConfig reads path; a missing file means an empty config.
func loadConfig(path string) (config, error) {
	var c config
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err == nil {
		err = json.Unmarshal(b, &c)
	}
	if err != nil {
		return c, fmt.Errorf("config %s: %w", path, err)
	}
	return c, nil
}

// withoutCollisions appends extra to builtin, skipping (with a warning) tools
// in a namespace a builtin uses, e.g. from an MCP server named "fs", and any
// tool whose name is already taken.
func withoutCollisions(builtin, extra []tool.Tool) []tool.Tool {
	reserved := map[string]bool{}
	taken := map[string]bool{}
	for _, t := range builtin {
		reserved[namespace(t.Name)] = true
		taken[t.Name] = true
	}
	warned := map[string]bool{}
	for _, t := range extra {
		if ns := namespace(t.Name); reserved[ns] {
			if !warned[ns] {
				warned[ns] = true
				fmt.Fprintf(os.Stderr, "steel: skipping MCP tools in tools.%s: steel's built-in tools use that namespace; rename the server in the config\n", ns)
			}
			continue
		}
		if taken[t.Name] {
			fmt.Fprintf(os.Stderr, "steel: skipping MCP tool %s: name already taken; rename the server in the config\n", t.Name)
			continue
		}
		taken[t.Name] = true
		builtin = append(builtin, t)
	}
	return builtin
}

// namespace returns a tool's top-level namespace: "fs" for "fs.read_file".
func namespace(name string) string {
	ns, _, _ := strings.Cut(name, ".")
	return ns
}

// newPad builds a session's scratchpad, routing its nested tool calls and
// finished background jobs back into the session.
func newPad(tools *tool.Registry, guidance map[string]string, toolBudget int, h agent.Hooks) *scratchpad.Pad {
	declBudget := toolBudget * 4 // ~4 bytes per token
	switch {
	case toolBudget < 0:
		declBudget = -1
	case toolBudget == 0:
		declBudget = 1 // nothing fits, so everything is indexed; 0 would mean the default
	}
	return scratchpad.New(tools, scratchpad.Options{
		DeclBudget: declBudget,
		Guidance:   guidance,
		OnCall: func(e scratchpad.CallEvent) {
			if !e.Done {
				h.Emit(agent.ToolStart{Name: e.Name, Args: e.Args, Nested: true})
				return
			}
			h.Emit(agent.ToolEnd{Name: e.Name, Result: e.Result, Err: e.Err, Nested: true})
		},
		OnJobDone: func(id string, r scratchpad.Result) {
			h.Deliver(fmt.Sprintf("[job %s finished]\n%s", id, r))
		},
	})
}
