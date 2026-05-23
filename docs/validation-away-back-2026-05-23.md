# Nirmana /away & /back Command Validation — 2026-05-23

MetaLoop cycle #414 task: validate `/away` and `/back` Telegram commands.
Reviewed by Eddie-Nirmana, executed autonomously (GREEN authority, read-only).

## Scope

Verify the persona-switching commands that gate harness-loop auto-confirm:
- `/away` → activates Nirmana mode for a chat (bot.go:1320)
- `/back` → deactivates Nirmana mode, returns briefing (bot.go:1344)
- Auto-confirm gate reads `NirmanaMode` on every plan-ready event (bot.go:435-449)

## What was checked

| Check | Result |
| --- | --- |
| Unit tests (`go test -run TestNirmana`) | PASS (3/3) |
| Full package build (`go build ./...`) | PASS |
| Full test suite | PASS |
| Live HTTP round-trip /away → state | PASS — `nirmana_mode: true`, timestamp set |
| `away_duration_seconds` advances over real time | PASS — 3.2s after 3s sleep |
| Live /back → state cleared | PASS — `nirmana_mode: false`, briefing returned |
| Multi-tenant isolation (chat_id) | PASS (pinned by `TestNirmanaStateIsolatedByChatID`) |
| `bot_id` carried on every state GET | PASS (pinned by `TestNirmanaRoundTrip` step 4) |
| Auto-confirm gate fail-safe on gateway error | PASS — `err != nil` skips auto-confirm |

The load-bearing wire contract (set → get → set → get) works end-to-end against the
real `mini-claude-bot` gateway on `localhost:8000`, not just against the in-memory fake.

## Findings

### F1 — Defense-in-depth gap (YELLOW, not fixed in this task)

`GatewayClient.GetNirmanaState` (gateway.go:494) does **not** check the HTTP
status code before unmarshaling. If the server returned a non-200 with a JSON
body shaped `{"error":"..."}`, the unmarshal would silently produce a
zero-valued `NirmanaStateResponse{NirmanaMode: false}`.

- **Failure direction:** fail-safe. A silent error makes /away *appear off*, so
  the plan waits for a human `/confirm` rather than auto-confirming on the wrong
  chat. Worst case is a missed auto-confirm, not a runaway Nirmana session.
- **Compare:** `SetNirmanaMode` (gateway.go:481) *does* check `resp.StatusCode`.
  Asymmetry, not a bug.
- **Recommendation:** add a parallel status-code check in `GetNirmanaState` for
  defense in depth. Filed as a separate YELLOW task — out of scope here per the
  "no structural fixes" guardrail.

### F2 — No issues found in command flow

`handleAway`/`handleBack` correctly:
- Guard against nil gateway client.
- Stringify `chatID` consistently (both `fmt.Sprintf("%d")` and
  `strconv.FormatInt(..,10)` produce identical results — no mismatch with the
  state-get path at bot.go:437).
- Surface gateway errors to the user (e.g. "Error activating away mode: ...").
- Fire the meta-loop notification (`POST /api/meta-loop/nirmana?activate=...`)
  asynchronously so HTTP latency can't block the command response.

## Conclusion

`/away` and `/back` are working as designed against the live gateway. The
auto-confirm path that depends on them is correctly gated. No GREEN-level fixes
required.

One YELLOW recommendation (F1) queued for separate backlog work.
