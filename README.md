# telegram-claude-hero

A Go CLI tool that bridges a Telegram bot with [Claude Code](https://docs.anthropic.com/en/docs/claude-code). Send messages to your Telegram bot, get responses from Claude — using your Claude subscription, no API key needed.

## How it works

**Local mode** (default):
- Spawns `claude -p --output-format text --dangerously-skip-permissions` for each message
- Uses `--continue` to maintain conversation context across messages
- Single chat session at a time

**Gateway mode** (with [mini-claude-bot](https://github.com/spacelobster/mini-claude-bot)):
- Forwards messages to a mini-claude-bot gateway API
- Each Telegram chat gets its own isolated Claude session
- Supports multiple concurrent users

In both modes:
- Auto-starts a session on the first message — no `/start` required
- Kills the claude process group on shutdown for clean cleanup

## Setup

1. Create a Telegram bot via [@BotFather](https://t.me/BotFather) and copy the token
2. Make sure `claude` CLI is installed and authenticated with your subscription

```bash
go install github.com/spacelobster/telegram-claude-hero@latest
```

Or build from source:

```bash
git clone https://github.com/spacelobster/telegram-claude-hero.git
cd telegram-claude-hero
go build
```

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

- `/start` — Reset the conversation
- `/stop` — End the session

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
- (Gateway mode) [mini-claude-bot](https://github.com/spacelobster/mini-claude-bot) running

## License

MIT
