# gostafson: Product Requirements

Status: draft, 2026-11-12
Companion: [reference-architecture.md](reference-architecture.md) covers how it's built. This file covers what and why, the decisions made so far, and what "done" means for each milestone.

---

## 1. Problem

Personal agent frameworks (OpenClaw, Hermes Agent) do a lot, and by default they can do a lot: broad shell access, many integrations, large codebases. For one person running local models on one Mac, that's more surface area than needed and more than one person can read and trust.

## 2. Goal

A small, local-first agent harness that one person can read end to end:

- Local models by default; cloud models only when asked for.
- A per-message prefix to pick the model: a local profile stays picked for the chat; a cloud profile is picked for one message unless it is marked sticky in config (D33).
- Remote access from a phone over a messenger.
- Confined to a few directories, no general shell, risky actions need a yes.
- A complete audit trail.
- One static binary, one config file, one setup script.

It is also a learning project: every piece should be code the owner wrote and understands, rather than a framework feature configured.

## 3. Non-goals

- Multi-user access or shared hosting.
- Gastown-style multi-agent orchestration (many long-lived agents, shared work ledger). Level 1 subagents only (section 6, FR-8).
- Long-term memory across conversations (maybe later).
- Plugin marketplace, skills system, web browsing automation.
- Running on Linux or Windows (macOS on Apple Silicon only, for now).
- Competing with Hermes or OpenClaw on features.

## 4. User and environment

- **User:** one technical owner, talking to the agent from a phone (messenger) or a terminal (CLI over SSH).
- **Host:** one Apple Silicon Mac, always on. Reference host: Mac mini M4 Pro, 24 GB unified memory. Already runs Ollama and LM Studio (and Hermes Agent, which this harness shares Ollama with).
- **Dev machine:** a separate Mac with SSH access to the host. Builds and deploys from there.
- **Cloud:** an OpenRouter API key for non-local models.

## 5. User stories

1. I send a plain message from my phone and get a fast answer from the local default model.
2. I add `think=true` and the same model reasons before answering.
3. I start a message with `@code` and a local coding model edits files in my web project directory.
4. I start a message with `@cloud` or `@sonnet` and a cloud model answers via OpenRouter, without seeing my files.
5. I send an untagged message that is clearly a coding task, and it goes to the coding model; if the harness isn't sure, or thinks it needs the cloud, it asks me first.
6. The agent wants to run a command that changes files (`rm`, `mv`, `git commit`); I get a yes/no prompt on my phone and nothing runs unless I say yes. A command that isn't on the list is refused without asking.
7. The coding agent hands a "read these files and summarize" subtask to a subagent, so its own context stays small.
8. I can see exactly what the agent did in any turn: model, routing reason, tools, arguments, outcome.
9. I deploy a new version with one command and roll back with one command.

## 6. Functional requirements

| ID | Requirement | Milestone |
|---|---|---|
| FR-1 | **Transport.** A `Transport` interface (`Name`, `Receive`, `Send`) plus optional `Asker` (approvals), `Indicator` (busy/typing) and `Sequential` (CLI). Several run at once; chat IDs are prefixed per transport (`web`, `tg:<id>`); chats run in parallel, messages within a chat in order; an approval goes back over the transport its message came from. Implementations: CLI, web page on the tailnet, Telegram bot, White Noise (D10). | M1, M4 |
| FR-2 | **Profiles.** Named backend profiles in config: endpoint, model, sampling, allowed tools, one root directory for file tools. Context length is set on the model (D15). Initial set: `default`, `code`, `cloud`, `claude`. On 2026-11-13 `claude` was renamed `sonnet` (each tag names its model) and `opus` (`anthropic/claude-opus-5.5`), `fable` (`anthropic/claude-fable-5.2`), `kimi` (`moonshotai/kimi-k3`) and `luna` (`openai/gpt-5.6-luna`) were added. A new model is a new profile in config, picked with its tag; there is no `/model` command (tags already switch, and cloud profiles never stick, D32). | M1, M2 |
| FR-3 | **Tag routing.** A leading `@tag` selects a profile and is stripped from the message. A local profile's tag sticks: later untagged messages in that chat go to it until another tag, `@default` or `/reset`; a message that is only a tag just switches. A cloud profile's tag applies to its own message only (D32), unless the profile has `sticky = true` (D33); replies from a sticky cloud profile start with `[profile]`. Unknown tags return an error listing valid tags. | M1, M5 |
| FR-4 | **Thinking switch.** Profiles with `think_toggle` send `reasoning_effort: "none"` unless the message contains `think=true`. Profiles without it never send the field. | M1 |
| FR-5 | **Sessions.** History per chat ID in SQLite, last N turns sent as context, `/reset` clears. | M1 |
| FR-6 | **Agent loop.** Model call, tool execution, repeat; max 8 tool steps and a per-turn timeout; on a limit, stop and say so. | M1, M2 |
| FR-7 | **File tools.** `list_dir`, `read_file`, `write_file`, `search_files`, each confined to the profile's roots via `os.Root`. | M2 |
| FR-8 | **Subagents.** `delegate(profile, task, tools)` runs a fresh loop with a policy equal to or narrower than the parent's, depth 1, its own step limit, summary capped at ~2K tokens. | M7 |
| FR-9 | **Command tool.** `run` executes argv (no shell) from a fixed list, each command with its own allowed flags and argument rules, inside the root and under `sandbox-exec`; commands that change files ask first; anything not on the list is refused (D31). | M5 |
| FR-10 | **Approval gate.** Risky actions pause the turn and ask yes/no over the transport; anything but `y`/`yes`, including no reply within `approval.timeout` (default 60 s), means no. A transport that can't ask means no. In M3 the risky action is `write_file` over an existing file; the prompt shows the size change (D22). | M3, M4 |
| FR-11 | **Cloud profiles.** OpenRouter backends; no file tools by default; `provider.data_collection` defaults to `deny` for openrouter.ai (`zdr = true` optional); history sent to a remote profile is text only (D21); `default_profile` must be local; an untagged message reaches the cloud only through a sticky cloud profile the owner tagged (D33); the decision model sending an untagged message to cloud always requires approval (M6). | M3 |
| FR-12 | **Decision routing.** Untagged messages are classified by a local decision model (`Decide(state, options) → probabilities`) over Jev's `/v1/systemone` API. Only the new message is sent, cut to about 300 characters; each option's one-line description comes from config. Below threshold, or a cloud result, asks first. Decision server down or slow (>500 ms) routes to `default`. | M6 |
| FR-13 | **Audit log.** JSONL line per turn and per tool call: time, chat, profile, `routed_by` (tag/decision/default), decision probability, model, tokens, tool, args (long strings shortened), guard decision and reason, duration. OpenTelemetry-style IDs: each turn is a trace with a root span, each tool call a child span. Subagent transcripts reuse the parent's trace ID. | M2 |
| FR-14 | **Config.** One TOML file; `config.example.toml` in the repo. Per profile, an API key comes from an environment variable (`api_key_env`) or a `0600` secrets file (`api_key`). An unset variable makes that profile unavailable without blocking the others. | M1 |
| FR-15 | **Service.** LaunchDaemon running as the `agent` user, starts at boot, restarts on crash. | M4 |
| FR-16 | **Deploy.** `make deploy` / `make rollback` per reference-architecture section 10. | M4 |
| FR-17 | **Images.** Every transport accepts images (web: paste, drop or attach; Telegram: photos and image files; White Noise: photo attachments via `download_media`; CLI: Ctrl+V from the clipboard, or an image file's path, as dragged into the terminal). Images are checked and scaled to 1,568 px with `sips`; a profile whose model can't read images says so. The chat's latest image stays attached for follow-ups until a new image or `/reset`; cloud profiles only get images sent with that message (D34). | M5 |
| FR-18 | **CLI line editor.** Raw-mode editing in Go without a dependency: arrows and Emacs keys, history, Tab completion of `/` commands and `@` tags with a dim hint, multi-line paste, Ctrl+V for clipboard images (D35). | M5 |

## 7. Non-functional requirements

**Security and confinement**
- NFR-1: The harness runs as a dedicated macOS user with no sudo and no read access to the owner's home.
- NFR-2: All file access goes through `os.Root` jails. No path from a model is opened any other way.
- NFR-3: No shell execution. Commands are argv only, from an allowlist.
- NFR-4: The binary and its directory are owned by the admin account, not writable by `agent`.
- NFR-5: Subagent and decision-model features can only narrow permissions, never widen them. Covered by tests that block deploy on failure.

**Privacy**
- NFR-6: No file contents, session history or audit data leave the host unless the owner explicitly sends a turn to a cloud profile.
- NFR-7: Local model servers (Ollama, LM Studio, decision server) bind to localhost only. Dev access is via SSH tunnel.
- NFR-8: The public repo contains no host names, personal names or secrets. Host-specific values live in gitignored files (`Makefile.local`, `config.local.toml`).

**Performance and footprint (reference host)**
- NFR-9: Default profile, short question, warm model: first token in under 2 s.
- NFR-10: Decision routing adds under 200 ms warm for a short message (measured 184 ms with short descriptions; 275 ms with ~80 tokens of descriptions).
- NFR-11: Harness process itself under 100 MB RSS, idle CPU near zero.
- NFR-12: Models in use fit the GPU memory limit (about 16–18 GB on 24 GB) alongside whatever Hermes keeps loaded, or swap with a stated cold-start cost.

**Maintainability**
- NFR-13: Go (1.24+ for `os.Root`; pinned to 1.27.4 in `.tool-versions`), standard library first. Third-party deps limited to `modernc.org/sqlite`, `BurntSushi/toml`, and the chosen messenger client.
- NFR-14: No agent framework (LangChain, ADK, etc.) in the harness.
- NFR-15: Each `internal/` package readable in one sitting.

## 8. Decision log

| # | Date | Decision | Why | Alternatives rejected |
|---|---|---|---|---|
| D1 | 2026-11-12 | Build our own harness instead of configuring Hermes | Learning goal; small, auditable surface | Hermes with locked-down toolsets plus a router in front |
| D2 | 2026-11-12 | Tag routing (`@code`, `@cloud`, `@claude`), one turn only (local tags sticky since D32) | Predictable, free, user-controlled | LLM router as the only mechanism |
| D3 | 2026-11-12 | OpenRouter for cloud models | One key, any model, OpenAI-compatible | Maple (enclave privacy; can be added later as one more profile), direct provider APIs |
| D4 | 2026-11-12 | Cloud profiles get no file tools by default; never auto-route to cloud | Data leaves the host only when the owner says so | Trusting provider data policies |
| D5 | 2026-11-12 | Jev-style decision model for untagged messages, tags still override | Gives a probability, ~100 ms, small | Small chat model as classifier (slower, no calibrated confidence); `escalate` tool only |
| D6 | 2026-11-12 | Subagents at level 1 only (`delegate` tool, depth 1) | Covers the useful case cheaply; memory limits parallelism anyway | Task queue (maybe later), Gastown-style orchestration (separate system) |
| D7 | 2026-11-12 | Go instead of Python | `os.Root` path jail, static binary, goroutines, small dep tree for a public repo | Python (faster start; evals stay in Python) |
| D8 | 2026-11-12 | Build on dev Mac, `scp` versioned binary, symlink switch, LaunchDaemon | Simple, rollback in one command, agent can't modify its binary | Building on the host; LaunchAgent (needs GUI login) |
| D9 | 2026-11-12 | Develop locally against the host's models over an SSH tunnel | Fast loop, Ollama stays on localhost | Exposing Ollama on the LAN |
| D10 | 2026-11-12 | CLI transport first; messenger choice deferred | Everything else can be built and tested without it | Picking a messenger up front |
| D11 | 2026-11-12 | No host or personal names in repo content | Repo will be public | |
| D12 | 2026-11-12 | Hand-written chat completions client on `net/http` instead of `openai-go` | `openai-go` v3 pulls the Azure and AWS SDKs into the module graph; the surface needed is ~150 lines | `openai-go` |
| D13 | 2026-11-12 | `qwen3.5:9b` for `default` and, until M5, `code`; thinking off via `reasoning_effort: "none"` | Already on the host; supports tools and thinking; tested: 0.4 s with thinking off vs 13 s on | `qwen3:8b` with `/no_think` prompt switches |
| D14 | 2026-11-12 | Bitwarden Secrets Manager is the source of truth for the OpenRouter key, but gostafson never calls it. Dev: `bwset OPENROUTER_API_KEY` then `api_key_env`. Host: `make push-secrets` writes a `0600` secrets file owned by `agent` | One place to store and rotate the key; the daemon has no boot-time network or keychain dependency; the host holds one limited key instead of a `bws` token that can read every secret its machine account can see | `bws` called at runtime on the host; key in a `.env` file |
| D15 | 2026-11-12 | Context length set by a model variant: `make models` creates `gostafson-qwen3.5-9b` (`qwen3.5:9b` with `num_ctx` 32768) through Ollama's `/api/create` | Ollama's `/v1` ignores per-request context size and loads at 4096 tokens, which tool results overflow. Measured on the host: 9.1 GB at 16K, 9.7 GB at 32K. A variant affects only gostafson, not Hermes | `OLLAMA_CONTEXT_LENGTH` on the host (changes every client); switching the client to Ollama's native `/api/chat` (loses LM Studio and OpenRouter compatibility) |
| D16 | 2026-11-12 | `default` is chat only in M2; `code` gets the four file tools | Tool definitions add about 1K prompt tokens and slow every short question; revisit with M5 evals | Notes root for `default` |
| D17 | 2026-11-12 | One root per profile | Paths stay simple for the model (relative, "." is the top); no root-name prefix to get wrong | A list of roots with named prefixes |
| D18 | 2026-11-12 | Guard is default deny: an unlisted or unknown tool name is rejected and logged, never passed through. A tool call that can't be written to the audit log fails | Matches Agent Gateway's default deny in the GCP architecture (gcp-comparison.md section 5) | Silently ignoring unknown tool calls |
| D19 | 2026-11-12 | MIT license | Owner's choice | |
| D20 | 2026-11-12 | Decision model: mlx-decision serving `pplx-decider-flash-v1.2` at 4 bits | On the host: 9/9 test messages routed as expected, same as 8-bit; 2.8 GB; 184–275 ms on short messages; Jev-compatible API, so hosted Jev or another local model can be swapped in by base URL. Reported level with Jev on classification | Laya (English only, lower accuracy in mlx-decision's benchmarks: TREC 79.0); 8-bit copy (same answers, 4.8 GB); hosted Jev (messages leave the host, per-call cost) |
| D21 | 2026-11-12 | History sent to a remote profile is text only: tool results are dropped, and each tool call becomes a note naming the tool and its path only (`[read_file a.txt ran on the local machine; its output is not shared with this model]`); user and assistant text stays. (Notes added the same day: without them, Claude read earlier `@code` replies as fabricated.) | File contents a tool read never leave the host as a side effect of tagging a later message; follow-up questions still have the conversation. Assistant replies that quoted a file still go; `/reset` before a cloud question for a clean start | Full history (breaks NFR-6); no history (cloud follow-ups lose context); asking each time tool results are present (frequent prompts, approve-without-reading) |
| D22 | 2026-11-12 | Approval is required for `write_file` over an existing file, with the size change in the prompt (`overwrite index.html (2,340 -> 1,120 bytes)? [y/N]`); new files need none | Asks only when something can be lost; a large size drop is the usual sign a model truncated a file | Every write (too many prompts for multi-file tasks); none until M5's `run` tool (approval gate unused) |
| D23 | 2026-11-12 | D10 resolved: three transports, CLI kept. White Noise (end-to-end encrypted, the app already used with Hermes), Telegram (easy, has push), and a web page on the tailnet (no third party) | Owner's choice; each covers a gap in the others | Signal (needs a second phone number) |
| D24 | 2026-11-12 | Web page: listens only on a literal loopback or Tailscale IP (config refuses all-interfaces and LAN); login with a token once, then an `HttpOnly`, `SameSite=Strict` cookie holding an HMAC of the token; POSTs need an `X-Gostafson` header; strict CSP with no inline script; server-sent events for replies, spinner and approvals | Private to the tailnet, nothing stored that reveals the token, no new dependency | Tailscale identity headers (needs `tailscale serve`), no auth on the tailnet |
| D25 | 2026-11-12 | Telegram: long polling on the standard library; only `allowed_users` (required) in private chats are answered; approvals use inline Yes/No buttons; replies go as escaped Telegram HTML (bold, code) with a plain-text retry; errors never include the token-bearing URL | Small and dependency-free. Accepted: Telegram sees message content (not end-to-end encrypted), so `@code` file content shown in replies passes through Telegram | A Telegram client library |
| D26 | 2026-11-12 | CLI styling with plain ANSI codes: gradient banner, colored prompt, spinner with rotating verbs, colored code/errors/footer; off when stdout isn't a terminal or `NO_COLOR` is set | No dependency; tests and pipes see plain text | A TUI library (bubbletea, lipgloss) |
| D27 | 2026-11-12 | White Noise through gostafson's own `wn-agent` (separate `--home`, socket and Marmot account), spoken to in Go over its control socket (`marmot.agent-control.v2`: NDJSON, `account_list`, `subscribe_inbound`, `send_final`); only `allowed_senders` answered; invites limited by wn-agent's welcomer allowlist | End-to-end encrypted, and the White Noise app is already in use. The protocol subset needed is small (~300 lines of Go, no dependency); learned from Hermes' plugin (wn-agent 0.15.0) since the source isn't on GitHub | Reusing Hermes' account (two programs answering one chat) |
| D28 | 2026-11-12 | Host config is written on the dev Mac (`config.host.toml`, gitignored) and installed by `make push-config` as `/usr/local/etc/gostafson/config.toml`, owned by root, mode 644 | The agent can read its config but not change it, so it can't widen its own roots, tools or allowlists; the config has no secrets, so world-readable is fine; one place to edit, like `Makefile.local` | Editing `config.toml` on the host (owned by whom? an agent-writable config undoes the guard) |
| D29 | 2026-11-12 | `wn-agent` runs as `agent` under its own LaunchDaemon (`com.gostafson.wn-agent`) from a copy in `/usr/local/libexec/gostafson`; the White Noise account lives in `/Users/agent/.marmot-agents/gostafson` | Survives reboots; the account's key isn't in the admin's home; `agent` can't read the admin's home (mode 750), so it can't run the admin's copy | Running it as the admin user next to Hermes' instance |
| D30 | 2026-11-12 | Web search for `@cloud` through OpenRouter's web plugin (`plugins = [{ id = "web", engine = "exa", max_results = 5 }]`), not a search tool | No new key or code path; nothing leaves the host that wasn't already going to OpenRouter; Exa's $0.007 per request is predictable (native provider search is priced by the provider) | A Brave Search tool now (a search tool on a profile with file tools is an exfiltration path; revisit for `default` only, with the guard keeping search and file tools apart) |
| D31 | 2026-11-13 | `run` tool: a fixed list in code (read-only `ls`, `tree`, `wc`, `head`, `tail`, `diff`, `grep`, `rg`, `find`, `file`, `du`, `jq`, `node --check`, `mkdir`, `git status/diff/log/show`; with approval `rm`, `mv`, `cp`, `git init/add/commit`), each with allowed flags; paths checked like the file tools plus `os.Root` resolution (no symlinks out) and nothing under `.git/`; unlisted commands refused, not offered for approval; each run under `sandbox-exec` (no network, writes only in the root, no file contents or listings under `/Users` except the root), fixed program paths, scrubbed environment, 30 s limit, 16 KB output; git with hooks, fsmonitor, pager, external diff and textconv off and no system or global config | A child process isn't confined by `os.Root`, and the agent user can read its own secrets file, so arguments are checked and the OS sandbox backs them up. Approving any argv from a phone is one tap from a shell (`node -e`). `.git/config` can make git run programs, so the model can't write there | Approval for unlisted commands (PRD v1 wording); interpreters, `npm`/`npx` (install scripts run code, network), `tidy` (macOS copy predates HTML5); a container per command (heavier; revisit for M7) |
| D32 | 2026-11-13 | Sticky local profiles: a tag for a local profile becomes the chat's profile until another tag, `@default` or `/reset`; tags for remote profiles apply to one message; the audit log records `routed_by: "sticky"`; on transports without the footer a sticky reply starts with `[profile]`; kept in memory (a restart returns chats to `default`). Explicit and sticky choices come before the decision model (M6), which only sees untagged messages in chats on `default` | Follow-ups ("yes", "now fix the CSS") went to `default`, which has no tools, and the 9B model answered nonsense (seen live: "Message"). Not sticking cloud profiles keeps the rule that nothing goes to OpenRouter without a tag on that message | One-turn tags (D2; simple, but every follow-up needs a tag); sticky cloud profiles (an untagged message could leave the machine); storing the sticky profile in SQLite (survives restarts; not needed yet) |
| D33 | 2026-11-13 | Per-profile `sticky = true` for remote profiles: once tagged, untagged messages in that chat go to that cloud profile until another sticky tag, `@default` or `/reset`; a non-sticky cloud tag in between applies to its message only; every reply from a sticky cloud profile starts with `[profile]` on every transport; `/profiles` shows `sticky` or `one message per tag`; set on all six cloud profiles in the owner's configs | The owner wanted `@opus` to hold like `@code`. It is still an explicit choice (a tag starts it, and the owner opts in per profile in config); the prefix on every reply shows that messages are leaving the machine. File tools stay off and history stays text-only | Cloud tags one message only (D32: safest, but every follow-up needs the tag); a `/model` command (tags already switch) |
| D34 | 2026-11-13 | Images on every transport through one path: transports hand over the bytes; `internal/media` checks the type (PNG, JPEG, GIF, WebP, HEIC), refuses over 20 MB or 4 per message, scales the long side to 1,568 px and pads a side under 64 px with white (Qwen's vision encoder needs 32 px, and Ollama's runner crashed on a 724x31 text screenshot) with macOS `sips` (GIF, WebP and HEIC become PNG); in the CLI a tag typed after pasted images (`[image #1] @opus ...`) is moved to the front; images go to the model as OpenAI `image_url` parts. Vision support is asked of the server on first use (Ollama capabilities, OpenRouter input modalities). The chat's latest image is kept as a file (0600, under the sessions directory) and re-attached to its message for follow-ups until a new image or `/reset`; history stores only its SHA-256 name; older images become a note. A cloud profile gets an image only with the message it was sent with, never from history. The audit log records name, type, size and SHA-256, not the image. Telegram's command menu is set with `setMyCommands` | One preparation step for four transports; no image library (`sips` ships with macOS). Re-attaching only the latest image keeps follow-ups working without re-sending every image in a long chat. Cloud models get only what the owner deliberately sent them | Sending an image only with its own message (follow-ups lose it); keeping every image in history (cost grows with each turn); `golang.org/x/image` for resizing (a dependency) |
| D35 | 2026-11-13 | A line editor for the CLI written in Go (`internal/lineedit`, raw mode through `syscall` on macOS, ~600 lines): Claude Code-style input with Ctrl+V for clipboard images (read with `osascript`, inserted as `[image #1]`; ⌘V too where the terminal sends an empty bracketed paste for an image-only clipboard), Tab and a dim hint for `/` and `@`, history, bracketed paste, Emacs keys; plain line input when stdin isn't a terminal | The owner wanted the CLI to work like Claude Code's, and a terminal can't paste image bytes, so the CLI has to catch Ctrl+V itself. No dependency needed | `golang.org/x/term` plus a readline library (new dependencies); `/paste` as a command (works, but not like Claude Code) |
| D36 | 2026-11-13 | Each saved reply records the profile that wrote it (stored, never sent); when history goes to a model, replies from other profiles start with `[reply from @default (gostafson-qwen3.5-9b), a different model, not you]`; replies saved before this stay unlabeled. The image note for cloud profiles says the model that answered saw the image | History is shared across profiles, so a model took every earlier answer as its own. Seen live: `@opus` "corrected" image answers the local model had made from images Opus never saw, and called `@cloud`'s web-search results fabricated | Separate history per profile (loses the context the owner relies on); no labels (models defend or retract answers they didn't write) |

## 9. Milestones and acceptance criteria

**M1: Chat over CLI with tag routing**
- `make run` with the tunnel up gives a REPL; plain messages answered by `qwen3.5:9b` with no thinking.
- Status (2026-11-12): done. Unit tests, `make test-integration`, and a scripted CLI session against the host pass.
- `think=true` produces a reasoning response; `@code` answers from the coding model; an unknown tag returns the list of valid tags.
- History survives a restart; `/reset` clears it.

**M2: File tools, guard, audit**
- `@code` can read and write files inside its root; attempts to read `../`, absolute paths outside the root, or a symlink pointing outside are rejected and logged.
- Guard has table-driven tests for escape attempts; `make test` fails if any pass.
- Every turn and tool call appears in `audit.jsonl` with the FR-13 fields, including trace and span IDs.
- A tool name the profile doesn't list, or that doesn't exist, is rejected and logged (D18).
- `@code` runs on the 32K context variant (D15).
- Status (2026-11-12): done. Guard escape tests (16 cases: `..`, absolute, `~`, symlinks to files and directories, writes through symlinks) pass; a scripted CLI session against the host wrote and read files, and a `../../` read was denied and logged.

**M3: Cloud profiles and approval on CLI**
- `@cloud` and `@claude` (now `@sonnet`) answer via OpenRouter; their requests contain no tool definitions for file access.
- OpenRouter requests carry the data-collection preference.
- An approval prompt appears on the CLI and times out to "no".
- History sent to remote profiles has no tool calls or tool results (D21); `default_profile` can't be remote.
- Status (2026-11-12): built. Unit tests cover the request body (no `tools`, `provider.data_collection: "deny"`), text-only history, approval answers and timeout, and the remote-default rule. On the host, an overwrite was refused on `n` and written on `y`, both logged with `approval`. Live checks: `@claude` (anthropic/claude-sonnet-5.5) answered with `data_collection: deny`; `@cloud` (google/gemini-3.8-flash with the web plugin, D30) answered from Telegram on the deployed service. As expected from D21, `@claude` could answer a question about a file because `@code`'s reply had quoted it.

**M4: Messenger and service**
- Messenger chosen (D10, D23) and adapter working: send from phone, get a reply.
- Approval prompts arrive on the phone.
- `setup.sh` provisions a clean host; the LaunchDaemon starts at boot as `agent`; `make deploy` and `make rollback` work end to end.
- Status (2026-11-12): done. Several transports at once with per-chat workers; colored CLI (D26); web page (D24); Telegram bot (D25); White Noise (D27), with its own `wn-agent` account running as `agent` (D29). `setup.sh` provisioned the host (agent user, directories, `wn-agent`, both LaunchDaemons, sudoers rule); `make push-config`, `make push-secrets` and `make deploy` installed and started gostafson as `agent`. Tested live against the deployed service: Telegram, White Noise and the web page from the phone; on the web page, a new file written without a prompt, an overwrite approved and one denied (the file unchanged), all three in the audit log. Phone transports send replies without the status footer; the web page keeps each approval question and its answer in the chat. `make rollback` switched back to the previous build and restarted the service. After a reboot both LaunchDaemons came back; gostafson's first start failed because the Tailscale address wasn't up yet, so the web page now retries its address in the background and the other transports start without it.

**M5: Command tool and evals**
- `run` executes allowlisted argv inside the jail; commands that change files ask first; others are refused (D31).
- Golden set in `evals/` covers each profile; coding model chosen by tool-call reliability and speed.
- Speed is measured per tool step, and each candidate is checked for Ollama prompt-cache reuse (the same prompt sent twice: the second call should evaluate almost no prompt tokens). `qwen3.5:9b` doesn't reuse the cache, so every tool step re-reads the whole prompt (~1,700 tokens, ~4 s on the host); `mistral:7b` does (0.02 s). Measured 2026-11-12.
- Status (2026-11-12): in progress. `gostafson-eval` (`make eval`) runs 14 golden cases in `evals/cases.toml` (chat, list/read/search, create, approved and denied overwrite, multi-step edit, missing file, path outside the root, prompt injection in a file) on a fresh copy of `evals/fixtures/site` per case, and reports pass rate, model-call time, turn time and prompt-cache reuse (18 cases since the `run` tool). Results, one run each unless noted: `qwen3.5:9b` 42/42 (3 runs; median model call 2.0 s, mean turn 6.3 s, cache not reused); `qwen3:8b` 39/42 (3 runs; 0.8 s, 2.5 s, cache reused; failed the same case each time by searching instead of reading the file); `qwen2.5:14b` 9/14 (followed the injection); `gemma4:e2b` 8/14 (followed the injection: wrote `pwned.txt`); `llama3.2:3b` 6/14; `mistral:7b` 3/14; `phi4-mini` 3/14 (invented a `/etc/passwd` line). `run` tool built (D31): unit tests for every allowed form and refusal, symlink escapes, `.git/` paths, and the sandbox (a private file and the root's parents unreadable, writes outside the root and network refused). With `run`, `qwen3.5:9b` passed 54/54 (3 runs of 18 cases); the median model call rose from 2.0 s to 3.3 s because the tool description adds about 300 tokens that the model re-reads at each step (no prompt cache). Fixes the evals forced: a short tool description with an example call (the long one doubled the prompt and the model called `rm` as a tool name), `done; no output` instead of `(no output)` (the model kept retrying `git add .`), and splitting a command given as one string. Tested live on the host from White Noise (2026-11-13): `tree`, `git init`/`add`/`commit` with an approval for each, `npm install` refused. Memory (2026-11-13): 24 GB host, `iogpu.wired_limit_mb` at its default. `gostafson-qwen3.5-9b` with a 32K context takes 9.07 GiB fully on the GPU (8.6 GiB with flash attention and a `q8_0` KV cache; the hybrid model's KV cache is small, so the saving is small). Hermes's main model runs on OpenRouter, not Ollama. The host had two `ollama serve` processes on port 11434 (two LaunchAgents: one on 127.0.0.1 with flash attention and `q8_0`, one on all interfaces with defaults); `localhost` reached either one depending on IPv4 or IPv6, each server loads its own copy of a model, and with both loaded (17.7 GiB) free memory fell to 8%. Fix: one Ollama server on 127.0.0.1 with flash attention and `q8_0`, and `base_url` set to `127.0.0.1`. Images on every transport (D34) and the CLI line editor (D35): unit tests for preparation and scaling with `sips`, the image store, the wire format, refs-only storage, each transport (web upload, Telegram photo and menu, White Noise `download_media`), and the editor (editing keys, history, completion, paste, Ctrl+V, dropped paths); live: a dragged image and an untagged follow-up through the CLI in a pseudo-terminal, and two image eval cases (4/4). Sticky local profiles (D32) with multi-turn eval cases (`turns`, `want_profiles`): 6/6 live. White Noise replies now sort below the messages they answer (the sender's clock was 1.6 s ahead; replies are held until a later whole second).

**M6: Decision routing**
- 50–100 real messages labeled from the audit log.
- Decision model beats "no tag means default" on that set, or is left off.
- Threshold set; cloud results always ask; server down falls back to `default`.

**M7: Subagents**
- `delegate` works from `@code` for a read-only summary task.
- Tests prove a child can't get roots or tools the parent doesn't have, and can't delegate further.
- Child transcript appears in the audit log under the parent turn.

## 10. Open questions

| Question | Needed by |
|---|---|
| Credit limit on the OpenRouter key (models: one profile each, FR-2) | open |
| Coding model: `qwen3-coder:30b` (needs model swapping) or `qwen2.5-coder:14b` (tool calls less reliable); prompt-cache reuse is a criterion (M5) | M5 |
| GPU wired-memory limit on the host, and whether to raise it: answered (M5 status): left at the default; one 9 GiB model fits with room to spare | done |
| How Hermes and the harness share Ollama without memory contention: answered (M5 status): Hermes's main model is on OpenRouter; the contention found was two Ollama servers on the host, each loading its own copy | done |

## 11. Risks

| Risk | Mitigation |
|---|---|
| Local models emit unreliable tool calls | Evals per model (M5); keep tool schemas small; step limit |
| Guard bug lets a path or command escape | `os.Root`, separate OS user, tests block deploy, audit log |
| Prompt injection via file or web content triggers risky tools | No shell, allowlist, approval gate, cloud profiles have no file tools |
| Decision-model projects are weeks old and change fast | Pin versions, `Decide()` interface, fall back to `default` |
| Messenger library immature (Marmot) | Transport interface; CLI always available; Signal as fallback |
| Memory contention with Hermes and multiple models | One local model at a time where needed; `keep_alive` tuning; NFR-12 check |
| Host name or personal details leak into the public repo | Gitignored local files (NFR-8); check before first push |
