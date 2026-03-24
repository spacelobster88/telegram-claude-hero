# telegram-claude-hero

A Go CLI tool that bridges a Telegram bot with [Claude Code](https://docs.anthropic.com/en/docs/claude-code). Send messages to your Telegram bot, get responses from Claude — using your Claude subscription, no API key needed.

## Architecture

```
┌─────────┐       ┌──────────────────────┐       ┌──────────────────────────┐
│ Telegram │       │  telegram-claude-hero │       │  mini-claude-bot         │
│  Users   │       │  (Go)                │       │  (Python/FastAPI)        │
│          │       │                      │       │                          │
│ ┌──────┐ │  Bot  │ ┌──────────────────┐ │       │ ┌──────────────────────┐ │
│ │User A├─┼──API──┼►│                  │ │ HTTP  │ │ Session Manager      │ │
│ └──────┘ │       │ │  Telegram Bot    │ │       │ │                      │ │
│ ┌──────┐ │       │ │                  ├─┼───────┼►│ Chat A → claude -p   │ │
│ │User B├─┼──────►│ │  Gateway Client  │ │       │ │ Chat B → claude -p   │ │
│ └──────┘ │       │ │                  │ │       │ │ Chat C → claude -p   │ │
│ ┌──────┐ │       │ └──────────────────┘ │       │ └──────────────────────┘ │
│ │User C├─┼──────►│                      │       │                          │
│ └──────┘ │       │  Local mode:         │       │ + Cron, Memory, Chat DB  │
│          │       │  claude -p (single)  │       │ + Dashboard, MCP, Reports│
└─────────┘       └──────────────────────┘       └──────────────────────────┘
                         ▲                                ▲
                         │                                │
                    This repo                    github.com/spacelobster88/
                                                   mini-claude-bot
```

## How it works

telegram-claude-hero operates in two modes, selected by whether a gateway URL is configured.

### Local mode (default)

- Spawns `claude -p --output-format text --dangerously-skip-permissions` for each message
- Uses `--continue` to maintain conversation context across messages in a single session
- Single chat session at a time — a second chat is blocked until the first is stopped
- Process group cleanup on shutdown via `SIGKILL` to the process group

### Gateway mode (with [mini-claude-bot](https://github.com/spacelobster88/mini-claude-bot))

Gateway mode connects telegram-claude-hero to mini-claude-bot's HTTP gateway API, turning it into a multi-user frontend for Claude Code. Instead of spawning `claude` processes directly, it delegates all Claude interactions to the mini-claude-bot server.

**How the connection works:**

1. telegram-claude-hero sends HTTP requests to mini-claude-bot's gateway endpoints
2. mini-claude-bot manages isolated Claude CLI sessions per chat, keyed by `(bot_id, chat_id)`
3. Responses are streamed back via Server-Sent Events (SSE), displayed with live edit-in-place updates in Telegram

**Gateway API endpoints used:**

| Endpoint | Purpose |
|---|---|
| `POST /api/gateway/send` | Send a message and get a complete response |
| `POST /api/gateway/send-stream` | Send a message and stream the response via SSE |
| `POST /api/gateway/send-background` | Start a long-running background task |
| `GET /api/gateway/background-status/:id` | Check background task status |
| `GET /api/gateway/harness-status/:id` | Get harness loop progress (phases, tasks) |
| `POST /api/gateway/stop` | Stop a session |

**Multi-tenant isolation:** Each bot instance is identified by a `bot_id` (defaults to the Telegram bot username). This allows multiple telegram-claude-hero bots to share the same mini-claude-bot server without session conflicts.

**Retry logic:** Transient errors (EOF, connection refused, connection reset) are retried up to 5 times with exponential backoff, making the bot resilient to mini-claude-bot restarts.

### In both modes

- Auto-starts a session on the first message — no `/start` required
- Kills the claude process group on shutdown for clean cleanup

## Features

### Streaming responses

In gateway mode, responses stream in real time. The bot sends a "Thinking..." message, then edits it in place as text arrives (throttled to one edit per 3 seconds to stay within Telegram API rate limits). If streaming fails, it falls back to a non-streaming request automatically.

### Background tasks

Long-running tasks can be delegated to background execution. The bot sends the task to mini-claude-bot's background endpoint, which runs it asynchronously and sends results back to the Telegram chat when complete. Use `/status` to check progress.

### Harness loop detection

The bot detects `[HARNESS_EXEC_READY]` markers in Claude's responses. When a harness loop plan is ready:

1. The plan is displayed to the user
2. The user is prompted to `/confirm` or send feedback to revise
3. On confirmation, execution starts as a background task

This prevents accidental execution of large multi-step plans and gives the user a review gate.

### Media support

- **Documents** — Text files (code, JSON, CSV, etc.) are extracted and sent inline. PDFs are parsed for text content. Binary files are saved locally and referenced by path.
- **Photos** — Downloaded and passed to Claude for visual analysis. In local mode, uses Claude's `-f` flag; in gateway mode, references the local file path.
- **Voice messages** — Transcribed via OpenAI Whisper API, then sent to Claude as text. Requires `OPENAI_API_KEY`. Max 5 minutes duration.

### Message formatting

Claude's markdown output is converted to Telegram-compatible HTML (bold, italic, strikethrough, inline code, code blocks). Falls back to plain text if HTML parsing fails. Long responses are automatically split at newline boundaries to stay within Telegram's 4096-character limit.

### Reply context

When a user replies to a previous message, the quoted message text is prepended as context so Claude understands what is being referenced.

## Setup

1. Create a Telegram bot via [@BotFather](https://t.me/BotFather) and copy the token
2. Make sure `claude` CLI is installed and authenticated with your subscription

```bash
go install github.com/spacelobster88/telegram-claude-hero@latest
```

Or build from source:

```bash
git clone https://github.com/spacelobster88/telegram-claude-hero.git
cd telegram-claude-hero
go build
```

## Configuration

Configuration is stored in `~/.telegram-claude-hero.json`. Values can also be set via environment variables (env vars take precedence).

| Setting | Config key | Env var | Description |
|---|---|---|---|
| Bot token | `telegram_bot_token` | — | Telegram bot token (prompted on first run) |
| Gateway URL | `gateway_url` | `GATEWAY_URL` | mini-claude-bot server URL (enables gateway mode) |
| Bot ID | `bot_id` | `BOT_ID` | Multi-tenant identifier (defaults to bot username) |
| OpenAI key | `openai_api_key` | `OPENAI_API_KEY` | Required for voice message transcription |

## Usage

### Local mode

```bash
./telegram-claude-hero
```

### Gateway mode

```bash
GATEWAY_URL=http://localhost:8000 ./telegram-claude-hero
```

On first run, it will prompt for your Telegram bot token (saved to `~/.telegram-claude-hero.json`).

Then just send a message to your bot in Telegram.

### Commands

- `/start` — Reset the conversation (stops existing session, starts fresh)
- `/stop` — End the session
- `/status` — Show background task status with progress bars per phase
- `/purge` — Run system memory purge (gateway mode only)
- `/confirm` — Confirm a pending harness plan and start background execution

Natural-language confirmations also work when a plan is pending: "yes", "ok", "go ahead", etc.

## Deployment

A deploy script with automatic rollback is included for safe updates on headless machines:

```bash
# 1. Build the new binary
go build -o telegram-claude-hero-new

# 2. Deploy with watchdog (auto-rollback if crash within 15s)
./deploy.sh
```

The script backs up the current binary, swaps in the new one, and monitors it for 15 seconds. If it crashes, the previous version is automatically restored.

See [`.claude/skills/deploy.md`](.claude/skills/deploy.md) for full details.

## Requirements

- Go 1.21+
- [Claude Code CLI](https://docs.anthropic.com/en/docs/claude-code) installed and authenticated
- A Telegram bot token
- (Gateway mode) [mini-claude-bot](https://github.com/spacelobster88/mini-claude-bot) running
- (Voice messages) `OPENAI_API_KEY` for Whisper transcription

## License

Apache-2.0
