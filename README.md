# telegram-claude-hero

A Go CLI tool that bridges a Telegram bot with [Claude Code](https://docs.anthropic.com/en/docs/claude-code). Send messages to your Telegram bot, get responses from Claude — using your Claude subscription, no API key needed.

## How it works

- Spawns `claude -p --output-format text --dangerously-skip-permissions` for each message
- Uses `--continue` to maintain conversation context across messages
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

```bash
./telegram-claude-hero
```

On first run, it will prompt for your Telegram bot token (saved to `~/.telegram-claude-hero.json`).

Then just send a message to your bot in Telegram.

### Commands

- `/start` — Reset the conversation
- `/stop` — End the session

## Requirements

- Go 1.21+
- [Claude Code CLI](https://docs.anthropic.com/en/docs/claude-code) installed and authenticated
- A Telegram bot token

## License

MIT
