#!/bin/bash
# Safe deployment script with watchdog auto-rollback.
# Designed for headless Mac Mini — no keyboard/mouse/SSH needed.
#
# Usage: ./deploy.sh
# Rollback: automatic if new binary crashes within 15 seconds

set -e

DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$DIR"

NEW_BIN="./telegram-claude-hero-new"
BAK_BIN="./telegram-claude-hero.bak"
CUR_BIN="./telegram-claude-hero"
GATEWAY_URL="${GATEWAY_URL:-http://localhost:8000}"
LOG="/tmp/tg-hero-deploy.log"

echo "=== Deploy started at $(date) ===" | tee -a "$LOG"

# 1. Verify new binary exists
if [ ! -f "$NEW_BIN" ]; then
    echo "ERROR: $NEW_BIN not found. Build first: go build -o telegram-claude-hero-new" | tee -a "$LOG"
    exit 1
fi

# 2. Verify gateway is reachable
if ! curl -sf http://localhost:8000/api/health > /dev/null 2>&1; then
    echo "ERROR: Gateway not reachable at http://localhost:8000. Start mini-claude-bot first." | tee -a "$LOG"
    exit 1
fi

# 3. Backup current binary
cp "$CUR_BIN" "$BAK_BIN"
echo "Backed up $CUR_BIN → $BAK_BIN" | tee -a "$LOG"

# 4. Find and kill current bot process
OLD_PID=$(pgrep -f './telegram-claude-hero$' || true)
if [ -n "$OLD_PID" ]; then
    echo "Killing current bot (PID $OLD_PID)..." | tee -a "$LOG"
    kill "$OLD_PID" 2>/dev/null || true
    sleep 2
    # Force kill if still alive
    kill -9 "$OLD_PID" 2>/dev/null || true
fi

# 5. Start new binary with gateway mode
echo "Starting new binary with GATEWAY_URL=$GATEWAY_URL..." | tee -a "$LOG"
GATEWAY_URL="$GATEWAY_URL" nohup "$NEW_BIN" >> /tmp/tg-hero-new.log 2>&1 &
NEW_PID=$!
echo "New binary started (PID $NEW_PID)" | tee -a "$LOG"

# 6. Watchdog: wait 15 seconds then check if alive
echo "Watchdog: waiting 15 seconds..." | tee -a "$LOG"
sleep 15

if kill -0 "$NEW_PID" 2>/dev/null; then
    echo "SUCCESS: New binary is running (PID $NEW_PID)" | tee -a "$LOG"
    # Replace current binary with new one
    cp "$NEW_BIN" "$CUR_BIN"
    echo "Updated $CUR_BIN with new binary" | tee -a "$LOG"
    echo "=== Deploy complete ===" | tee -a "$LOG"
else
    echo "FAILED: New binary crashed! Auto-rolling back..." | tee -a "$LOG"
    echo "Starting backup binary..." | tee -a "$LOG"
    nohup "$BAK_BIN" >> /tmp/tg-hero-backup.log 2>&1 &
    BAK_PID=$!
    sleep 3
    if kill -0 "$BAK_PID" 2>/dev/null; then
        echo "ROLLBACK SUCCESS: Backup binary running (PID $BAK_PID)" | tee -a "$LOG"
        # Restore backup as current
        cp "$BAK_BIN" "$CUR_BIN"
    else
        echo "CRITICAL: Backup binary also failed! Manual intervention needed." | tee -a "$LOG"
    fi
    echo "=== Deploy FAILED — rolled back ===" | tee -a "$LOG"
    exit 1
fi
