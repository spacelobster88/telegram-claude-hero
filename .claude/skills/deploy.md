# Deploy telegram-claude-hero

Build and deploy the telegram-claude-hero bot with zero-downtime switching and automatic rollback.

## Steps

1. **Build the new binary**:
   ```bash
   cd /Users/spacelobster/Projects/telegram-claude-hero
   go build -o telegram-claude-hero-new
   ```

2. **Run the deploy script**:
   ```bash
   cd /Users/spacelobster/Projects/telegram-claude-hero
   ./deploy.sh
   ```

The deploy script will:
- Verify the new binary exists and the gateway is reachable
- Back up the current binary to `telegram-claude-hero.bak`
- Kill the running bot process
- Start the new binary with `GATEWAY_URL` (defaults to `http://localhost:8000`)
- Wait 15 seconds as a watchdog check
- If the new binary is still running: promote it as the current binary (success)
- If the new binary crashed: auto-rollback to the backup binary

## Rollback

Rollback is automatic. If the new binary crashes within 15 seconds of starting, the deploy script restores and starts the backup binary.

To manually rollback:
```bash
cd /Users/spacelobster/Projects/telegram-claude-hero
cp telegram-claude-hero.bak telegram-claude-hero
# Then kill the current process and start ./telegram-claude-hero
```

## Logs

- Deploy log: `/tmp/tg-hero-deploy.log`
- New binary output: `/tmp/tg-hero-new.log`
- Backup binary output: `/tmp/tg-hero-backup.log`
