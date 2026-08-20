# Account cooldown clamp

Official grok2api images can freeze a Build account for hours when upstream
returns 504/429 with a long `Retry-After`. The in-process `cooldownMax` clamp
is applied before Retry-After, so this sidecar enforces the operator retry
window against `provider_accounts.cooldown_until`.

It also truncates Go/SQLite nanosecond timestamps. Python's `fromisoformat`
rejects 9-digit fractions, which previously caused the clamp to skip the row.

## What it does

1. Every 15 seconds, find accounts whose `cooldown_until` is more than
   `--max-remaining` in the future (default 1 minute).
2. Rewrite `cooldown_until` to now + max remaining, and clear `failure_count`.
3. PATCH `/api/admin/v1/accounts/:id` with `{enabled: true}` so the selector
   overlay drops the stale 24h cooldown.

The script reads `bootstrapAdmin` from `config.yaml` and never logs the
password.

## Run

```bash
python3 tools/cooldown-clamp/clamp_account_cooldown_test.py
chmod 700 tools/cooldown-clamp/clamp-account-cooldown-loop.sh
sudo cp tools/cooldown-clamp/grok2api-cooldown-clamp.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now grok2api-cooldown-clamp.service
```

Override paths with `--db`, `--config`, `--api-base`, and `--max-remaining`.
This is a host-side workaround until a grok2api release caps Retry-After inside
`markFailure`.
