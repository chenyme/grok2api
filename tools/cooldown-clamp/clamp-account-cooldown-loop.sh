#!/bin/bash
set -eu
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
while true; do
  python3 "$SCRIPT_DIR/clamp_account_cooldown.py" "$@" || true
  sleep 15
done
