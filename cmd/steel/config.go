package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"github.com/spiiritual/steel/internal/datafile"
	"github.com/spiiritual/steel/internal/llm/openai"
	"github.com/spiiritual/steel/internal/mcp"
	"github.com/spiiritual/steel/internal/secret"
)

// options are steel's flags. Each one in settings also has a saved value in
// config.json, which the flag overrides for one run.
type options struct {
	baseURL, model, system, configPath, dataDir *string
	memoryDir, compactModel                     *string
	memorySystem                                *choice
	allowedUsers                                *idList
	idle, drain                                 *time.Duration
	toolBudget                                  *int
	showVersion                                 *bool
}

func defineFlags(fs *flag.FlagSet) *options {
	home, _ := os.UserHomeDir()
	o := &options{
		allowedUsers: &idList{},
		memorySystem: &choice{value: "optmem", options: memorySystemNames()},
	}
	o.model = fs.String("model", "", summary("model"))
	o.baseURL = fs.String("base-url", openai.DefaultBaseURL, summary("base-url"))
	o.system = fs.String("system", "", summary("system"))
	fs.Var(o.allowedUsers, "telegram-allowed-users", summary("telegram-allowed-users"))
	o.configPath = fs.String("config", defaultConfigPath(), `settings file, edited with "steel config"; secrets go in .env beside it`)
	o.dataDir = fs.String("data", filepath.Join(home, ".steel", "sessions"), summary("data"))
	o.memoryDir = fs.String("memory", filepath.Join(home, ".steel", "memory"), summary("memory"))
	fs.Var(o.memorySystem, "memory-system", summary("memory-system"))
	o.compactModel = fs.String("compact-model", "", summary("compact-model"))
	o.idle = fs.Duration("idle", 30*time.Minute, summary("idle"))
	o.toolBudget = fs.Int("tool-budget", 4000, summary("tool-budget"))
	o.drain = fs.Duration("drain", time.Minute, summary("drain"))
	o.showVersion = fs.Bool("version", false, "print the version and exit")
	return o
}

// summary is a setting's flag help.
func summary(flag string) string {
	s, _ := settingNamed(flag)
	if s.env != "" {
		return s.summary + " (env " + s.env + ")"
	}
	return s.summary
}

func defaultConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".steel", "config.json")
}

// dotenvPath is the .env beside config.json, holding steel's own tokens
// (and settings, if put there), so a test config elsewhere keeps its own.
func dotenvPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), ".env")
}

// secretsPath is secrets.env beside config.json: the user's secrets, for
// tools (tools.secrets) and mcpServers (${NAME}). Kept apart from .env so
// steel's own variables are never taken for the user's secrets.
func secretsPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "secrets.env")
}

// secretPath is the file secret variable envVar is saved in.
func secretPath(configPath, envVar string) string {
	if isOwn(envVar) {
		return dotenvPath(configPath)
	}
	return secretsPath(configPath)
}

// isOwn reports whether envVar is one of steel's own variables: a token
// or a setting's.
func isOwn(envVar string) bool {
	return slices.ContainsFunc(secrets, func(s ownSecret) bool { return s.env == envVar }) ||
		slices.ContainsFunc(settings, func(s setting) bool { return s.env == envVar })
}

// moveUserSecrets moves variables that aren't steel's own from .env, where
// older versions kept them, to secrets.env, returning their names.
func moveUserSecrets(configPath string) ([]string, error) {
	env, err := secret.ReadDotenv(dotenvPath(configPath))
	if err != nil {
		return nil, err
	}
	var moved []string
	for name := range env.Vars() {
		if !isOwn(name) {
			moved = append(moved, name)
		}
	}
	if len(moved) == 0 {
		return nil, nil
	}
	slices.Sort(moved)
	user, err := secret.ReadDotenv(secretsPath(configPath))
	if err != nil {
		return nil, err
	}
	have, vars := user.Vars(), env.Vars()
	for _, name := range moved {
		if _, ok := have[name]; !ok { // secrets.env's is newer
			user.Set(name, vars[name])
		}
		env.Unset(name)
	}
	// secrets.env first: if writing .env fails, nothing is lost.
	if err := user.Write(secretsPath(configPath)); err != nil {
		return nil, err
	}
	return moved, env.Write(dotenvPath(configPath))
}

// setting is a flag whose value config.json can save. Its docs are the only
// ones: flag help, "steel config" and "steel config help" all show them.
type setting struct {
	flag    string   // also its name in "steel config"
	path    []string // where config.json keeps it
	env     string   // environment variable that overrides config.json, if any
	summary string   // one line
	details string   // what "steel config help" adds: format, examples, gotchas
}

// settings in the order "steel config" lists them.
var settings = []setting{
	{
		flag: "model", path: []string{"model"}, env: "OPENAI_MODEL",
		summary: "model name, as the API at base-url spells it",
		details: `Required: steel won't start without one.
Use the exact ID the provider expects, e.g. "gpt-4o-mini" for OpenAI or
"anthropic/claude-sonnet-4" for OpenRouter.
Example: steel config set model gpt-4o-mini`,
	},
	{
		flag: "compact-model", path: []string{"compactModel"},
		summary: "cheap model that writes memory's summaries",
		details: `Memory summarizes the model's notes in pairs, and pairs of those, in
the background: about one small call (under 5 KB) per note. Any model
from the same base-url will do; a cheap one is plenty. Empty uses model.
Example: steel config set compact-model gpt-4.1-nano`,
	},
	{
		flag: "base-url", path: []string{"baseURL"}, env: "OPENAI_BASE_URL",
		summary: "OpenAI-compatible API base URL",
		details: `Any server that speaks the OpenAI chat completions API: OpenAI,
OpenRouter, a local Ollama or llama.cpp server, and so on. Include the
version path, without /chat/completions.
Example: steel config set base-url http://localhost:11434/v1`,
	},
	{
		flag: "system", path: []string{"system"},
		summary: "system prompt",
		details: `Instructions sent ahead of every conversation. A change applies from
steel's next start, to saved conversations too.
Example: steel config set system "Answer briefly."`,
	},
	{
		flag: "telegram-allowed-users", path: []string{"telegram", "allowedUsers"},
		summary: "Telegram user IDs allowed to chat, comma-separated",
		details: `Empty means nobody: the bot ignores every message. These are numeric
user IDs, not @usernames; message @userinfobot on Telegram to get yours.
Example: steel config set telegram-allowed-users 123456789,987654321`,
	},
	{
		flag: "data", path: []string{"dataDir"},
		summary: "directory for saved conversation history",
		details: `One JSON file per conversation, saved before every model call.
Empty keeps history in memory only, lost when steel exits.
Example: steel config set data ~/steel-history`,
	},
	{
		flag: "memory", path: []string{"memory"},
		summary: "directory of steel's memory, shared by every chat",
		details: `The model notes what's worth keeping there, one line at a time, kept
forever and summarized in the background (by compact-model). Every new
conversation starts by reading it. Empty turns memory off.
The format is OptMem's: point this at ~/.optmem/memory to share one memory
with agents that use OptMem's memo tool.
Example: steel config set memory ~/Sync/steel-memory`,
	},
	{
		flag: "memory-system", path: []string{"memorySystem"},
		summary: `how memory works: "optmem"`,
		details: `optmem: the model notes what it learns; each conversation starts with
its notes, recent ones whole and old ones summarized into a tree it can
search and zoom (github.com/VictorTaelin/OptMem).
Example: steel config set memory-system optmem`,
	},
	{
		flag: "idle", path: []string{"idle"},
		summary: "close a conversation's scratchpad after this much inactivity",
		details: `Frees the memory of the JavaScript scratchpad behind run_code; its
variables are gone, but the conversation's history is kept. 0 never closes
it. A Go duration: 90s, 30m, 2h.
Example: steel config set idle 2h`,
	},
	{
		flag: "tool-budget", path: []string{"toolBudget"},
		summary: "approximate tokens of full tool declarations shown to the model",
		details: `Tools that fit, most recently used first, get their full declarations
in the prompt; the rest get one line each, and the model reads their docs
before first use. 0 indexes every tool, a negative number shows everything
in full.
Example: steel config set tool-budget 8000`,
	},
	{
		flag: "drain", path: []string{"drain"},
		summary: "on SIGTERM, how long running work may take to finish",
		details: `On SIGTERM steel stops taking messages and lets running turns and
background jobs finish for up to this long, then exits. Keep it under your
service manager's stop timeout. Ctrl-C or a second SIGTERM quits at once.
A Go duration: 30s, 1m, 5m.
Example: steel config set drain 30s`,
	},
}

// ownSecret is a token steel itself uses, saved in .env.
type ownSecret struct {
	name    string // its name in "steel config"
	env     string // its variable in .env or the environment
	summary string
	details string
}

// secrets steel itself uses. Any other upper-case name is a user secret,
// saved in secrets.env, for tools and mcpServers entries (${NAME}).
var secrets = []ownSecret{
	{
		name: "openai-api-key", env: "OPENAI_API_KEY",
		summary: "API key for base-url",
		details: `Sent as a bearer token to whichever API base-url points at, so use
that provider's key (an OpenRouter key for OpenRouter). Local servers that
need no key can leave it unset.`,
	},
	{
		name: "telegram-bot-token", env: "TELEGRAM_BOT_TOKEN",
		summary: "Telegram bot token",
		details: `Create a bot by messaging @BotFather on Telegram and sending /newbot;
it replies with a token like 123456789:AAE.... Anyone with the token
controls the bot: keep it out of chats, scripts and shell history.`,
	},
}

func settingNamed(name string) (setting, bool) {
	i := slices.IndexFunc(settings, func(s setting) bool { return s.flag == name })
	if i < 0 {
		return setting{}, false
	}
	return settings[i], true
}

// secretVar returns the variable a "steel config" secret name refers to.
func secretVar(name string) (string, bool) {
	for _, s := range secrets {
		if s.name == name {
			return s.env, true
		}
	}
	return name, secret.ValidName(name) == nil
}

// unknownName explains a name that's neither a setting nor a secret.
func unknownName(name string) error {
	best, bestDist := "", 3 // farther than this isn't a typo
	for _, s := range settings {
		if d := editDistance(name, s.flag); d < bestDist {
			best, bestDist = s.flag, d
		}
	}
	for _, s := range secrets {
		if d := editDistance(name, s.name); d < bestDist {
			best, bestDist = s.name, d
		}
	}
	if best != "" {
		return fmt.Errorf("no setting %q; did you mean %s?", name, best)
	}
	return fmt.Errorf("no setting %q; steel config help lists them", name)
}

// editDistance is the Levenshtein distance between a and b.
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

// environment is where steel looks up its own variables: its environment,
// then .env. It's a snapshot, so steel can remove its tokens from the real
// environment and still expand them in mcpServers. secrets holds
// secrets.env, for steel config to list.
type environment struct {
	os, dotenv, secrets map[string]string
}

func loadEnvironment(configPath string) (environment, error) {
	moved, err := moveUserSecrets(configPath)
	if err != nil {
		return environment{}, fmt.Errorf("moving secrets from .env to secrets.env: %w", err)
	}
	for _, name := range moved {
		fmt.Fprintf(os.Stderr, "steel: moved %s from %s to %s\n", name, dotenvPath(configPath), secretsPath(configPath))
	}
	d, err := secret.ReadDotenv(dotenvPath(configPath))
	if err != nil {
		return environment{}, err
	}
	user, err := secret.ReadDotenv(secretsPath(configPath))
	if err != nil {
		return environment{}, err
	}
	e := environment{os: map[string]string{}, dotenv: d.Vars(), secrets: user.Vars()}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		e.os[k] = v
	}
	return e, nil
}

// lookup prefers the environment, unless a variable there is set but empty.
func (e environment) lookup(name string) (string, bool) {
	if v := e.os[name]; v != "" {
		return v, true
	}
	if v, ok := e.dotenv[name]; ok {
		return v, true
	}
	v, ok := e.os[name]
	return v, ok
}

// source says where name is set, for steel config.
func (e environment) source(name string) string {
	switch {
	case e.os[name] != "":
		return "environment"
	case e.dotenv[name] != "":
		return ".env"
	case e.secrets[name] != "":
		return "secrets.env"
	}
	return ""
}

// configFile is config.json as a JSON tree, so editing one setting keeps the
// rest of the file, including keys this steel doesn't know. It stays plain
// JSON so an "mcpServers" block can be pasted from Claude Desktop or Code.
type configFile struct {
	path string
	root map[string]json.RawMessage
}

// readConfig reads path; a missing file is an empty config.
func readConfig(path string) (*configFile, error) {
	c := &configFile{path: path}
	b, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(b, &c.root)
	} else if errors.Is(err, fs.ErrNotExist) {
		err = nil
	}
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	if c.root == nil {
		c.root = map[string]json.RawMessage{}
	}
	return c, nil
}

func (c *configFile) get(path []string) (json.RawMessage, bool) {
	node := c.root
	for _, k := range path[:len(path)-1] {
		var next map[string]json.RawMessage // fresh: Unmarshal into a map merges
		if json.Unmarshal(node[k], &next) != nil {
			return nil, false
		}
		node = next
	}
	raw, ok := node[path[len(path)-1]]
	return raw, ok && string(raw) != "null"
}

// text returns the value saved at path in flag syntax.
func (c *configFile) text(path []string) (string, bool, error) {
	raw, ok := c.get(path)
	if !ok {
		return "", false, nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", false, err
	}
	scalar := func(v any) (string, bool) {
		switch v := v.(type) {
		case string:
			return v, true
		case float64:
			return strconv.FormatFloat(v, 'f', -1, 64), true
		case bool:
			return strconv.FormatBool(v), true
		}
		return "", false
	}
	if s, ok := scalar(v); ok {
		return s, true, nil
	}
	if list, ok := v.([]any); ok {
		parts := make([]string, len(list))
		for i, e := range list {
			if parts[i], ok = scalar(e); !ok {
				break
			}
		}
		if ok {
			return strings.Join(parts, ","), true, nil
		}
	}
	return "", false, fmt.Errorf("config %s: %s: unexpected value %s", c.path, strings.Join(path, "."), raw)
}

// put saves v at path, or deletes it if v is nil.
func (c *configFile) put(path []string, v any) error {
	return putJSON(c.root, path, v)
}

// putJSON creates objects on the way to path, and drops ones it empties.
func putJSON(node map[string]json.RawMessage, path []string, v any) error {
	k := path[0]
	if len(path) == 1 {
		if v == nil {
			delete(node, k)
			return nil
		}
		b, err := json.Marshal(v)
		node[k] = b
		return err
	}
	var child map[string]json.RawMessage
	if raw, ok := node[k]; ok && json.Unmarshal(raw, &child) != nil {
		return fmt.Errorf("%q in the config file is not an object", k)
	}
	if child == nil {
		child = map[string]json.RawMessage{}
	}
	if err := putJSON(child, path[1:], v); err != nil {
		return err
	}
	if len(child) == 0 {
		delete(node, k)
		return nil
	}
	b, err := json.Marshal(child)
	node[k] = b
	return err
}

func (c *configFile) write() error {
	b, err := json.MarshalIndent(c.root, "", "  ")
	if err != nil {
		return err
	}
	return datafile.WriteFile(c.path, append(b, '\n'))
}

func (c *configFile) mcpServers() (map[string]mcp.ServerConfig, error) {
	raw, ok := c.get([]string{"mcpServers"})
	if !ok {
		return nil, nil
	}
	var m map[string]mcp.ServerConfig
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("config %s: mcpServers: %w", c.path, err)
	}
	return m, nil
}

// value returns a setting's text and where it came from: an environment
// variable, config.json, or "" for neither.
func (s setting) value(c *configFile, env environment) (text, from string, err error) {
	if s.env != "" {
		if src := env.source(s.env); src != "" {
			v, _ := env.lookup(s.env)
			return v, s.env + " in " + src, nil
		}
	}
	text, ok, err := c.text(s.path)
	if !ok || err != nil {
		return "", "", err
	}
	return text, filepath.Base(c.path), nil
}

// applyConfig sets each setting the command line didn't from the
// environment or config.json, validating it as the flag would.
func applyConfig(fs *flag.FlagSet, c *configFile, env environment) error {
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	for _, s := range settings {
		if explicit[s.flag] {
			continue
		}
		text, from, err := s.value(c, env)
		if err != nil {
			return err
		}
		if from == "" {
			continue
		}
		if err := fs.Set(s.flag, text); err != nil {
			return fmt.Errorf("%s: invalid value %q from %s: %w", s.flag, text, from, err)
		}
	}
	return nil
}

const configUsage = `usage: steel config [-config path] [command]

  steel config                     list settings and secrets, and where each comes from
  steel config help [name]         explain every setting and secret, or one
  steel config set <name> <value>  save a setting in config.json
  steel config set <secret>        save a secret, typed at a hidden prompt or piped in:
                                   steel's own in .env, any other NAME in secrets.env
  steel config unset <name>        remove a saved setting or secret
  steel config edit                open config.json in $EDITOR, e.g. to add mcpServers

Each setting is also a flag (steel -help) that overrides it for one run.
Variables in steel's environment take precedence over .env and secrets.env.

`

func runConfig(args []string) error {
	fset := flag.NewFlagSet("config", flag.ExitOnError)
	configPath := fset.String("config", defaultConfigPath(), "settings file; secrets go in .env beside it")
	fset.Usage = func() {
		fmt.Fprint(fset.Output(), configUsage)
		fset.PrintDefaults()
	}
	fset.Parse(args)
	args = fset.Args()
	if len(args) > 0 && args[0] == "edit" && len(args) == 1 {
		return editConfig(*configPath) // without reading, so a broken file can be fixed
	}
	c, err := readConfig(*configPath)
	if err != nil {
		return err
	}
	switch {
	case len(args) == 0:
		return showConfig(c)
	case args[0] == "help":
		return showHelp(c, args[1:])
	case args[0] == "set" && len(args) >= 2 && len(args) <= 3:
		return setConfig(c, args[1], args[2:])
	case args[0] == "unset" && len(args) == 2:
		return unsetConfig(c, args[1])
	}
	fset.Usage()
	os.Exit(2)
	return nil
}

func showConfig(c *configFile) error {
	env, err := loadEnvironment(c.path)
	if err != nil {
		return err
	}
	defaults := flag.NewFlagSet("", flag.ContinueOnError)
	defineFlags(defaults)
	fmt.Printf("settings: %s\ntokens:   %s\nsecrets:  %s\n\n", c.path, dotenvPath(c.path), secretsPath(c.path))
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, s := range settings {
		text, from, err := s.value(c, env)
		if err != nil {
			return err
		}
		if from == "" {
			text, from = defaults.Lookup(s.flag).DefValue, "default"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", s.flag, shorten(text, 40), from, s.summary)
	}
	fmt.Fprintln(w, "\t\t\t")
	for _, s := range secrets {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", s.name, secretState(env, s.env), s.env, s.summary)
	}
	for _, k := range slices.Sorted(maps.Keys(env.secrets)) {
		fmt.Fprintf(w, "%s\t%s\t\t%s\n", k, secretState(env, k), "for tools, and mcpServers as ${"+k+"}")
	}
	w.Flush()
	servers, err := c.mcpServers()
	if err != nil {
		return err
	}
	names := strings.Join(slices.Sorted(maps.Keys(servers)), ", ")
	fmt.Printf("\nmcp servers: %s (steel config edit)\n", cmp.Or(names, "none"))
	fmt.Println("steel config help <name> explains a setting.")
	return nil
}

// showHelp prints the docs of the named settings and secrets, or of all.
func showHelp(c *configFile, names []string) error {
	env, err := loadEnvironment(c.path)
	if err != nil {
		return err
	}
	defaults := flag.NewFlagSet("", flag.ContinueOnError)
	defineFlags(defaults)
	all := len(names) == 0
	if all {
		for _, s := range settings {
			names = append(names, s.flag)
		}
		for _, s := range secrets {
			names = append(names, s.name)
		}
	}
	for i, name := range names {
		if i > 0 {
			fmt.Println()
		}
		if s, ok := settingNamed(name); ok {
			where := "config.json: " + strings.Join(s.path, ".")
			if s.env != "" {
				where += "; " + s.env + " overrides it"
			}
			text, from, err := s.value(c, env)
			if err != nil {
				return err
			}
			if from == "" {
				text, from = defaults.Lookup(s.flag).DefValue, "default"
			}
			fmt.Printf("%s  (%s)\n", s.flag, where)
			printIndented(s.summary + ".\n" + s.details)
			printIndented(fmt.Sprintf("Default: %s\nNow: %s, from %s", shorten(defaults.Lookup(s.flag).DefValue, 60), shorten(text, 60), from))
			continue
		}
		envVar, ok := secretVar(name)
		if !ok {
			return unknownName(name)
		}
		doc := "A secret for tools (as {{secret:" + envVar + "}}) and mcpServers entries (as ${" + envVar + "})."
		if j := slices.IndexFunc(secrets, func(s ownSecret) bool { return s.env == envVar }); j >= 0 {
			doc = secrets[j].summary + ".\n" + secrets[j].details
		}
		fmt.Printf("%s  (%s: %s)\n", name, filepath.Base(secretPath(c.path, envVar)), envVar)
		printIndented(doc)
		printIndented(fmt.Sprintf("Now: %s\nSet it: steel config set %s", secretState(env, envVar), name))
	}
	if all {
		fmt.Println("\nAny other upper-case NAME is a secret too: steel config set NAME saves it in secrets.env,\nfor tools to use and mcpServers entries in config.json as ${NAME}.")
	}
	return nil
}

func printIndented(s string) {
	for line := range strings.SplitSeq(s, "\n") {
		fmt.Println("  " + line)
	}
}

// shorten fits a value on one line of n runes.
func shorten(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		s = string(r[:n-3]) + "..."
	}
	return cmp.Or(s, "(empty)")
}

func secretState(env environment, name string) string {
	if src := env.source(name); src != "" {
		return "set in " + src
	}
	return "not set"
}

func setConfig(c *configFile, name string, value []string) error {
	if s, ok := settingNamed(name); ok {
		if len(value) != 1 {
			return fmt.Errorf("usage: steel config set %s <value>", name)
		}
		v, err := settingJSON(name, value[0])
		if err != nil {
			return err
		}
		if err := c.put(s.path, v); err != nil {
			return err
		}
		if err := c.write(); err != nil {
			return err
		}
		fmt.Printf("%s = %s, saved in %s\n", name, shorten(value[0], 60), c.path)
		if s.env != "" && os.Getenv(s.env) != "" {
			fmt.Printf("note: %s in your environment overrides it\n", s.env)
		}
		return nil
	}
	envVar, ok := secretVar(name)
	if !ok {
		return unknownName(name)
	}
	if len(value) != 0 {
		return fmt.Errorf("type the secret at the prompt or pipe it in, so it stays out of shell history: steel config set %s", name)
	}
	tok, err := readSecret(envVar)
	if err != nil {
		return err
	}
	path := secretPath(c.path, envVar)
	d, err := secret.ReadDotenv(path)
	if err != nil {
		return err
	}
	d.Set(envVar, tok)
	if err := d.Write(path); err != nil {
		return err
	}
	fmt.Printf("%s saved in %s\n", envVar, path)
	if os.Getenv(envVar) != "" {
		fmt.Printf("note: %s in your environment overrides it\n", envVar)
	}
	return nil
}

// settingJSON validates text as the flag would and returns the value to
// save: numbers and lists as JSON ones, the rest as typed.
func settingJSON(name, text string) (any, error) {
	fset := flag.NewFlagSet("", flag.ContinueOnError)
	defineFlags(fset)
	if err := fset.Set(name, text); err != nil {
		return nil, fmt.Errorf("%s: invalid value %q: %w", name, text, err)
	}
	switch v := fset.Lookup(name).Value.(flag.Getter).Get().(type) {
	case int:
		return v, nil
	case []int64:
		if v == nil {
			v = []int64{} // saved as [], not null
		}
		return v, nil
	}
	return text, nil
}

func unsetConfig(c *configFile, name string) error {
	if s, ok := settingNamed(name); ok {
		if err := c.put(s.path, nil); err != nil {
			return err
		}
		if err := c.write(); err != nil {
			return err
		}
		fmt.Printf("%s removed from %s\n", name, c.path)
		return nil
	}
	envVar, ok := secretVar(name)
	if !ok {
		return unknownName(name)
	}
	path := secretPath(c.path, envVar)
	d, err := secret.ReadDotenv(path)
	if err != nil {
		return err
	}
	if !d.Unset(envVar) {
		fmt.Printf("%s wasn't in %s\n", envVar, path)
		return nil
	}
	if err := d.Write(path); err != nil {
		return err
	}
	fmt.Printf("%s removed from %s\n", envVar, path)
	return nil
}

// readSecret reads a secret from a hidden prompt, or from stdin when it's
// piped in.
func readSecret(name string) (string, error) {
	var b []byte
	var err error
	if fd := int(os.Stdin.Fd()); term.IsTerminal(fd) {
		fmt.Fprintf(os.Stderr, "%s (input hidden): ", name)
		b, err = term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
	} else {
		b, err = io.ReadAll(os.Stdin)
	}
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", fmt.Errorf("no %s given; to remove it: steel config unset %s", name, name)
	}
	return s, nil
}

func editConfig(path string) error {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		if err := datafile.WriteFile(path, []byte("{}\n")); err != nil {
			return err
		}
	}
	editor := cmp.Or(os.Getenv("VISUAL"), os.Getenv("EDITOR"), "vi")
	// Through sh, so an editor with arguments ("code -w") works.
	cmd := exec.Command("sh", "-c", editor+` "$1"`, "sh", path)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", editor, err)
	}
	c, err := readConfig(path)
	if err != nil {
		return err
	}
	if _, err := c.mcpServers(); err != nil {
		return err
	}
	check := flag.NewFlagSet("", flag.ContinueOnError)
	defineFlags(check)
	if err := applyConfig(check, c, environment{}); err != nil {
		return fmt.Errorf("config %s: %w", path, err)
	}
	return nil
}

// choice is a flag that takes one of a fixed set of values.
type choice struct {
	value   string
	options []string
}

func (c *choice) String() string {
	if c == nil {
		return ""
	}
	return c.value
}

func (c *choice) Set(s string) error {
	if !slices.Contains(c.options, s) {
		return fmt.Errorf("want one of: %s", strings.Join(c.options, ", "))
	}
	c.value = s
	return nil
}

func (c *choice) Get() any { return c.value }

// idList is a flag of comma-separated integer IDs.
type idList []int64

func (l *idList) String() string {
	if l == nil {
		return ""
	}
	parts := make([]string, len(*l))
	for i, id := range *l {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, ",")
}

func (l *idList) Set(s string) error {
	var ids []int64
	for f := range strings.FieldsFuncSeq(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		id, err := strconv.ParseInt(f, 10, 64)
		if err != nil {
			return fmt.Errorf("%q is not a numeric ID", f)
		}
		ids = append(ids, id)
	}
	*l = ids
	return nil
}

func (l *idList) Get() any { return []int64(*l) }
