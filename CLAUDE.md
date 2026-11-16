# gostafson

A small, local-first agent harness in Go for one Apple Silicon Mac: local models by default, `@tag` model switching (local profiles stick per chat, cloud tags apply to one message), optional cloud models via OpenRouter, a messenger for remote access, and tight limits on what the agent can touch.

## Read first

- `docs/PRD.md`: goals, requirements (FR/NFR), decision log (D1...), milestones with acceptance criteria. Check the decision log before proposing a different approach; add a row when a decision changes.
- `docs/reference-architecture.md`: terms, diagram, components, deployment.
- `docs/gcp-comparison.md`: compare/contrast with the GCP agent reference architecture.

## Status

- M1 (chat over CLI with tag routing): done.
- M2 (file tools, guard with `os.Root`, JSONL audit log): done.
- M3 (cloud profile rules, OpenRouter data-collection preference, approval gate on the CLI): done; `@claude` (now `@sonnet`) and `@cloud` checked live. Cloud profiles: cloud, sonnet, opus, fable, kimi, luna.
- M4 (transports, `setup.sh`, LaunchDaemon, first deploy): done. Deployed on the host as LaunchDaemons running as `agent`; CLI colors, web page, Telegram and White Noise tested live. `make rollback` and a reboot tested.
- M5 (command tool and evals): done. Eval harness done (`make eval`); `qwen3.5:9b` stays the model for now. `run` tool built (PRD D31), deployed and tested from the phone (tree, git init/add/commit with approvals, npm refused). Images on every transport (D34), the CLI line editor (D35), reply labels (D36): deployed. Projects (D37): deployed. Saved conversations (D38): built, not yet deployed.

## Rules

- **Public repo.** Never put host names, personal names or secrets in committed files, comments or commit messages. Say "the host". Host-specific values live only in gitignored files: `Makefile.local`, `config.local.toml`, `config.host.toml`, `config.toml`, `secrets.toml`.
- **Git is manual.** Don't run git commands that change state (add, commit, push, branch). Read-only commands are fine.
- **Dependencies.** Standard library first. Allowed: `modernc.org/sqlite`, `github.com/BurntSushi/toml`, and later one messenger client. Ask before adding anything else. No agent frameworks; no `openai-go` (PRD D12).
- **Security invariants** (tests must cover them; a failing guard test blocks deploy):
  - All file access from tools goes through `os.Root`.
  - No shell: commands run as argv from the fixed list in `internal/tools/run.go`, never `sh -c`, each under `sandbox-exec`; unlisted commands are refused, not offered for approval (PRD D31).
  - Cloud profiles get no file tools by default and text-only history; nothing routes to a cloud profile without an explicit tag (which holds for the chat only if that profile has `sticky = true`, PRD D33) or approval; `default_profile` must be local.
  - Subagents and the decision model can only narrow permissions.
- Keep each `internal/` package small enough to read in one sitting.

## Layout

```
cmd/gostafson-eval/  golden-case runner: pass rate, speed, prompt-cache reuse per model
cmd/gostafson/       main.go: wiring, flags, turn handling; serve.go: transports and per-chat workers; commands.go: /help /profiles /status /reset
internal/config/     TOML config; API keys from env vars (dev) or a 0600 secrets file (host)
internal/client/     OpenAI-compatible chat completions client (net/http)
internal/router/     @tag and think=true parsing; Sticky: per-chat profile from the last local tag (PRD D32)
internal/loop/       agent loop: model call, tools, step limit
internal/tools/      file tools (list_dir, read_file, write_file, search_files) on an *os.Root; run.go: command list, argument checks, sandbox
internal/guard/      default deny, path checks, one os.Root per profile, audit record per call
internal/audit/      JSONL audit log, trace and span IDs
internal/approve/    yes/no approval gate with timeout
internal/sessions/   SQLite: saved conversations (titles, summaries, search) and their history (PRD D38)
internal/media/      image checks, scaling with sips, each chat's current image (PRD D34)
internal/projects/   project dirs, PDF/text/OCR extraction, hybrid search index, project tools (PRD D37)
internal/lineedit/   CLI line editor: raw mode, history, Tab completion, Ctrl+V images (PRD D35)
internal/evals/      eval cases (TOML), one turn per case in a fresh fixture copy, checks
internal/transport/  Transport interface; CLI (colors in style.go), web page (webpage/ embedded), Telegram, White Noise (wn-agent socket)
docs/                PRD and reference architecture
deploy/              setup.sh, LaunchDaemon plists, secrets.sh (used by make setup / push-secrets)
```

## Commands

```sh
make test              # go vet + unit tests
make tunnel            # SSH tunnel to the host's Ollama (needs Makefile.local)
make models            # create gostafson-qwen3.5-9b (32K context) on the host's Ollama (tunnel up)
make eval              # golden cases in evals/ against the host's models (tunnel up); MODELS=a,b EVAL_ARGS="-n 3"
make test-integration  # unit + integration tests against the host's Ollama (tunnel up)
make run               # CLI REPL with config.local.toml (tunnel up); ARGS=-v logs a line per turn
make build             # dist/gostafson for darwin/arm64
make setup             # run deploy/setup.sh on the host with sudo (agent user, dirs, wn-agent, LaunchDaemons)
make push-config       # config.host.toml -> /usr/local/etc/gostafson/config.toml (root-owned)
make push-project NAME=x DIR=~/Docs/x   # a folder's PDFs, text and images into a project's sources on the host
make push-secrets      # OPENROUTER_API_KEY, GOSTAFSON_TELEGRAM_TOKEN, GOSTAFSON_WEB_TOKEN from this shell to the host
make deploy | rollback | versions | logs | service-status | service-log
```

Go is pinned in `.tool-versions` (mise). The local model is `gostafson-qwen3.5-9b` (`qwen3.5:9b` with a 32K context, PRD D15); thinking is off via `reasoning_effort: "none"` unless the message contains `think=true`.
