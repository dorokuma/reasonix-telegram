# reasonix-telegram

Telegram bot bridge for [Reasonix](https://github.com/esengine/DeepSeek-Reasonix) (DeepSeek-native coding agent).

One long-lived `reasonix serve` per chat, session resume across bridge restarts, streaming via `sendMessageDraft` with `sendMessage` finalize.

## Features

- Pure chat mode: `tools.enabled = ["none"]` in dedicated workdir
- Per-chat Reasonix session (JSONL under `STATE_DIR`)
- Streaming replies (draft preview → final message)
- Slash commands: `/stop`, `/status`, `/new`, `/restart`, …

## Build

```bash
go build -o reasonix-telegram .
```

Requires Go 1.24+ and `reasonix` on `PATH` (build from [DeepSeek-Reasonix](https://github.com/esengine/DeepSeek-Reasonix) `main-v2`).

## Configure

Copy `.env.example` to `/etc/reasonix-telegram.env` (or export variables):

| Variable | Description |
|----------|-------------|
| `TG_BOT_TOKEN` | BotFather token |
| `DEEPSEEK_API_KEY` | Reasonix provider key |
| `ALLOWED_USERS` | Optional comma-separated Telegram user IDs |
| `STATE_DIR` | Default `/var/lib/reasonix-telegram` |
| `CHAT_RULES_FILE` | Optional path to `AGENTS.md` / `REASONIX.md` symlinked into chat workdir |

Project memory: place `REASONIX.md` or `AGENTS.md` in the chat workdir (`$STATE_DIR/chat-wd/`), not only under `/root`.

## systemd

See `deploy/reasonix-telegram.service`. Install binary to `/usr/local/bin/reasonix-telegram`, enable unit `reasonix-telegram.service`.

## Relation to Reasonix

This is **not** a fork of Reasonix. It is a thin Telegram frontend that drives `reasonix serve` over HTTP (SSE). Upstream agent: [esengine/DeepSeek-Reasonix](https://github.com/esengine/DeepSeek-Reasonix); optional fork patches (e.g. `tools.enabled = ["none"]`) live in your `reasonix` build.

## License

Same spirit as the deployment repo — add a license file if you publish publicly.