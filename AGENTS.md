# AGENTS.md

steel is a personal agent harness in Go. It's used mainly through Telegram and talks to any OpenAI-compatible API.

## Commands

- Before every commit: `gofmt -l .` (no output), `go vet ./...`, `go test -race ./...`.
- Run: `go run ./cmd/steel config set model <model>` once (`steel config` lists every setting), then `go run ./cmd/steel`. Flags override saved settings for one run (see `-help`).
- Release: `git tag -a vX.Y.Z -m "steel vX.Y.Z" && git push origin vX.Y.Z`. CI tests, builds, signs and publishes; never upload release assets by hand.

## Layout

| Path | What it is |
|---|---|
| `cmd/steel` | Wiring, flags, `config`/`update`/`rollback` subcommands |
| `internal/agent` | Sessions: inbox, turns, history, events. Knows nothing about channels |
| `internal/channel/{terminal,telegram}` | Channels: send user text into `agent.Sessions`, render `agent.Event`s |
| `internal/llm`, `internal/llm/openai` | Provider interface and the OpenAI-compatible streaming client |
| `internal/scratchpad` | The goja JS runtime behind `run_code`: tool layout, disclosure, background jobs |
| `internal/tool` | `Tool`, `Registry`, builtins, Go types → TypeScript, JSON Schema → TypeScript, recent-tools list |
| `internal/mcp` | MCP client: standard `mcpServers` config, tool adaptation, server guidance |
| `internal/datafile` | Versioned, atomic JSON files for all saved state |
| `internal/update` | Signed-release check, download, verify, swap, rollback |

## Design decisions

Don't undo these without discussing it first.

- **Code mode.** The model's only tool is `run_code`. Tools are async functions on a global `tools` object inside a persistent JS scratchpad. Call a `run_code` call a **script**, never a "cell".
- **Signatures come from the source of truth.** Go types (`tool.New`) for builtins, JSON Schema for MCP tools, rendered as inline TypeScript with no named types, so each signature stands alone.
- **Progressive disclosure.** Namespaces get full declarations while they fit the budget; the rest are an index of name + short description. **No tool runs before the model has seen its full documentation**: calls to indexed tools are refused until a script result has carried their docs (`tools.$docs(...)` or a refused call). Docs travel in `Result.Docs`, never as values a script could hide.
- **Plugins are standards, not Go.** Extending steel means MCP servers and config files (same `mcpServers` format as Claude Desktop/Claude Code). Never require users to write Go.
- **Layers stay separate.** `agent` doesn't know about channels; channels don't know about the model or tools.

## Invariants

- **Saved state** goes through `internal/datafile`. When a format changes, bump its version and migrate on load. Never overwrite a file written by a newer version.
- **Secrets** come from environment variables or `~/.steel/.env` (written 0600 by `steel config set`), never `config.json`, logs or error messages. `.env` values are looked up by steel, never exported to its environment, and tokens steel itself uses are removed from the environment after reading, so tools can't see them. The repo is public: never commit secrets.
- **Access control fails closed.** An empty Telegram allowlist means nobody.
- **Shutdown.** SIGTERM drains (stop input, let work finish, bounded by `-drain`); Ctrl-C or a second SIGTERM quits now. History is saved before every model call.
- **Releases** are verified against `update.ReleaseKey`; a signature's trusted comment must be `steel <tag>`. Changing the key strands every installed copy.

## Conventions

- **Names matter.** Pick clear, specific names; rename when a better one appears.
- **Standard library first.** A dependency needs a reason.
- **Comments say why** and state invariants; don't narrate code.
- **Tests** cover behavior a user or caller would notice. Deterministic and race-free; fakes over mocks (e.g. `httptest` servers that behave like the real API). No tests of wiring or echoes. When a test pins a bug, fix the test.
- **No README or other docs files** unless asked.
- **Commits** only with the owner's approval. Subjects are short and plain: `area: what changed`.

## Changing what the model sees

Tool descriptions, the `run_code` text, the index format, disclosure: measure, don't assume.

- Benchmark on the same weak model steel targets. A stronger model defeats the point.
- Compare variants on identical prompts with several reps each. Report correctness, scripts per task, failed calls and tokens.
- Include prompts where the obvious guess is wrong (units, misleading names), not only easy ones.
