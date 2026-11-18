// Command steel is a personal agent harness.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	_ "time/tzdata" // schedules' zones, wherever steel runs

	"github.com/spiiritual/steel/internal/agent"
	"github.com/spiiritual/steel/internal/bashcheck"
	"github.com/spiiritual/steel/internal/channel/telegram"
	"github.com/spiiritual/steel/internal/llm/openai"
	"github.com/spiiritual/steel/internal/mcp"
	"github.com/spiiritual/steel/internal/memory"
	"github.com/spiiritual/steel/internal/schedule"
	"github.com/spiiritual/steel/internal/scratchpad"
	"github.com/spiiritual/steel/internal/secret"
	"github.com/spiiritual/steel/internal/tool"
	"github.com/spiiritual/steel/internal/usage"
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
	o := defineFlags(flag.CommandLine)
	flag.Parse()
	if *o.showVersion {
		fmt.Println("steel", versionString())
		return nil
	}
	fmt.Fprintln(os.Stderr, "steel", versionString())

	cfg, err := readConfig(*o.configPath)
	if err != nil {
		return err
	}
	env, err := loadEnvironment(*o.configPath)
	if err != nil {
		return err
	}
	if err := applyConfig(flag.CommandLine, cfg, env); err != nil {
		return err
	}
	mcpServers, err := cfg.mcpServers()
	if err != nil {
		return err
	}
	// Trimmed, since token files often end in a newline. Removed from the
	// environment so tools (shell.bash) can't read them; env keeps a copy
	// for mcpServers expansion.
	apiKey, _ := env.lookup("OPENAI_API_KEY")
	token, _ := env.lookup("TELEGRAM_BOT_TOKEN")
	apiKey, token = strings.TrimSpace(apiKey), strings.TrimSpace(token)
	for _, s := range secrets {
		os.Unsetenv(s.env)
	}

	if *o.model == "" {
		return errors.New("no model: run `steel config set model <name>`")
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	if token == "" {
		return errors.New("no Telegram bot token: run `steel config set telegram-bot-token`")
	}
	bot := telegram.New(telegram.Options{Token: token, AllowedUsers: *o.allowedUsers})
	defer bot.Close() // runs after sessions.Close, so their last messages go out

	// Ctrl-C quits now, cancelling running work. SIGTERM, which service
	// managers send, stops taking input and lets running work finish first.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	runCtx, stopRun := signal.NotifyContext(ctx, syscall.SIGTERM)
	defer stopRun()

	servers, warnings := mcp.Connect(runCtx, mcpServers, env.expand) // either signal interrupts a slow server
	defer servers.Close()
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, "steel:", w)
	}

	provider := openai.New(*o.baseURL, apiKey)
	usageLog := usage.NewLog(usagePath(*o.configPath), func(err error) {
		fmt.Fprintln(os.Stderr, "steel: recording usage:", err)
	})
	home, _ := os.UserHomeDir()
	// Commands touching steel's own state need approval: its settings,
	// secrets, history and memory.
	protected := []string{filepath.Dir(*o.configPath)}
	for _, dir := range []string{*o.dataDir, *o.memoryDir} {
		if dir != "" {
			protected = append(protected, dir)
		}
	}
	check := func(cmd string) []string {
		return bashcheck.Check(cmd, bashcheck.Options{Dir: cwd, Home: home, Protected: protected})
	}
	// The model uses the user's secrets; steel's own tokens, wherever they
	// come from, are only redacted from tool results.
	tokens := map[string]string{}
	for _, sc := range secrets {
		v, _ := env.lookup(sc.env)
		tokens[sc.env] = strings.TrimSpace(v)
	}
	store, err := secret.Open(secretsPath(*o.configPath), secret.Options{Redact: tokens})
	if err != nil {
		return err
	}
	bot.SetSecrets(store)
	builtin := []tool.Tool{tool.Bash(cwd, check)}
	builtin = append(builtin, secret.Tools(store)...)
	system := systemPrompt(*o.system)
	var mem memory.Memory
	if *o.memoryDir != "" {
		opened, err := openMemory(o, usageLog.Track(logCalls(provider, "compaction", os.Stderr), "compaction"))
		if err != nil {
			return err
		}
		defer opened.close() // after sessions.Close, so the last turns are logged
		mem, system = opened.memory, opened.system
		builtin = append(builtin, opened.tools...)
	}
	sched, err := schedule.Open(schedule.Options{
		Path:   filepath.Join(filepath.Dir(*o.configPath), "schedules.json"),
		Places: bot,
		Logf: func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "steel: "+format+"\n", args...)
		},
	})
	if err != nil {
		return err
	}
	defer sched.Close() // after sessions.Close, which ends the runs it waits on
	builtin = append(builtin, schedule.Tools(sched)...)
	bot.SetSchedules(scheduleList{sched})
	tools := tool.NewRegistry(withoutCollisions(builtin, servers.Tools())...)
	// recent.json sits next to the config, so a test config elsewhere keeps
	// its own history.
	recent, err := tool.LoadRecent(filepath.Join(filepath.Dir(*o.configPath), "recent.json"), func(err error) {
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
		Provider:    usageLog.Track(logCalls(provider, "turn", os.Stderr), "turn"),
		Model:       *o.model,
		System:      system,
		Memory:      mem,
		NewToolbox:  func(h agent.Hooks) agent.Toolbox { return newPad(tools, guidance, store, *o.toolBudget, h) },
		Dir:         *o.dataDir,
		IdleTimeout: *o.idle,
	}, func(id string, e agent.Event) {
		// The channel shows a failed turn to the user; the log keeps it
		// for whoever runs steel.
		if d, ok := e.(agent.TurnDone); ok && d.Err != nil {
			fmt.Fprintf(os.Stderr, "steel: %s: turn failed: %v\n", id, d.Err)
		}
		bot.Emit(id, e)
	})
	if err != nil {
		return err
	}
	defer sessions.Close() // cancels whatever is still running
	sched.Start(scheduleRunner{sessions})

	err = bot.Run(runCtx, sessions)
	// Before draining, so no new run starts.
	sched.Stop()
	if runCtx.Err() != nil && ctx.Err() == nil { // SIGTERM, not Ctrl-C
		fmt.Fprintf(os.Stderr, "steel: finishing running work (up to %s; SIGTERM or Ctrl-C again to quit now)\n", *o.drain)
		again, stopAgain := signal.NotifyContext(ctx, syscall.SIGTERM)
		drainCtx, cancel := context.WithTimeout(again, *o.drain)
		if derr := sessions.Drain(drainCtx); derr != nil {
			fmt.Fprintln(os.Stderr, "steel: cancelling unfinished work:", derr)
		}
		cancel()
		stopAgain()
	}
	return err
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

// namespace returns a tool's top-level namespace: "shell" for "shell.bash".
func namespace(name string) string {
	ns, _, _ := strings.Cut(name, ".")
	return ns
}

// newPad builds a session's scratchpad, routing its nested tool calls and
// finished background jobs back into the session.
func newPad(tools *tool.Registry, guidance map[string]string, secrets *secret.Store, toolBudget int, h agent.Hooks) *scratchpad.Pad {
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
		Approver: func(ctx context.Context, a tool.Approval) (bool, error) {
			return h.Approve(ctx, a.Tool, a.Action, a.Reasons)
		},
		Caller:      h.Session,
		Secrets:     secrets,
		SecretAsker: h.AskSecret,
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

// scheduleRunner runs schedules in sessions.
type scheduleRunner struct{ *agent.Sessions }

func (r scheduleRunner) Start(ctx context.Context, id, text string, notes []string) (string, error) {
	reply, err := r.Sessions.Start(ctx, id, text, notes)
	if errors.Is(err, agent.ErrBusy) {
		err = schedule.ErrBusy
	}
	return reply, err
}

// scheduleList shows schedules in Telegram's /schedules.
type scheduleList struct{ *schedule.Scheduler }

func (l scheduleList) Overview() []telegram.ScheduleEntry {
	var out []telegram.ScheduleEntry
	for _, sc := range l.List() {
		out = append(out, telegram.ScheduleEntry{ID: sc.ID, Name: sc.Name, Text: sc.Describe(), From: sc.From})
	}
	return out
}

func (l scheduleList) CancelSchedule(ctx context.Context, id string) (string, error) {
	sc, err := l.Cancel(ctx, id)
	return sc.Name, err
}

var _ schedule.Runner = scheduleRunner{}
