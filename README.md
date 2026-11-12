# gostafson

A small, local-first agent harness for one Apple Silicon Mac, written in Go.

- Local models by default (Ollama or LM Studio).
- Pick the model for one message with a tag: `@code fix the nav`. No tag uses the default profile.
- Add `think=true` to a message to turn on the model's reasoning for that turn.
- Optional cloud models through OpenRouter, only when you ask for them.
- Planned: remote access over a messenger, file tools confined to a few directories, approval prompts for risky actions, a JSONL audit log, and small subagents.

It is meant to be small enough to read end to end. There is no agent framework and no general shell access.

**Status:** early. Milestones 1 to 4 work (colored CLI, a web page for the tailnet, a Telegram bot, White Noise through `wn-agent`, running on the host as LaunchDaemons under a dedicated `agent` user): chat over a CLI with tag routing and per-chat history; file tools for `@code` confined to one directory with `os.Root`; a JSONL audit log of every turn and tool call; cloud profiles over OpenRouter with text-only history and `data_collection: deny`; a yes/no prompt before a file is overwritten. See [docs/PRD.md](docs/PRD.md) for the milestones.

## Requirements

- macOS on Apple Silicon
- Go (version in `.tool-versions`; [mise](https://mise.jdx.dev) installs it)
- An OpenAI-compatible model server, e.g. [Ollama](https://ollama.com) with `qwen3.5:9b` pulled

## Quick start

```sh
cp config.example.toml config.local.toml   # edit base_url and models
make test
make models    # once: creates the 32K-context model variant on Ollama
make run
```

```
> What is 17*23? Number only.
391
[default | gostafson-qwen3.5-9b | 0.6s | 58 tok]

> @code Write a one-line JS function that reverses a string.
const reverse = s => s.split('').reverse().join('');
[code | gostafson-qwen3.5-9b | 1.0s | 100 tok]
```

Commands: `/help`, `/profiles`, `/status`, `/reset`, `/exit`. `@code` can list, read, write and search files in its `root` (`var/work/code` in the example config); every turn and tool call goes to `var/audit.jsonl`.

If the models run on another Mac, put `DEPLOY_HOST = <ssh host>` in `Makefile.local` and run `make tunnel` in a second terminal. `docs/reference-architecture.md` section 10 covers development and deployment.

## Configuration

`config.example.toml` documents every setting. Each `[profiles.<name>]` section is a backend (base URL, model, prompt, sampling); its name is also its `@tag`. A profile's API key comes from an environment variable (`api_key_env`) or from a separate `secrets.toml` with mode `0600` (`api_key`).

## Docs

- [docs/PRD.md](docs/PRD.md): requirements, decision log, milestones
- [docs/reference-architecture.md](docs/reference-architecture.md): architecture, components, deployment
- [docs/gcp-comparison.md](docs/gcp-comparison.md): compared with a GCP agent reference architecture

## License

MIT. See [LICENSE](LICENSE).
