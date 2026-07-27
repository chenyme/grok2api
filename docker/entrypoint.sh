#!/bin/sh
set -eu
umask 077
mkdir -p /run/grok2api /app/data

CONFIG_DEST=/app/config.yaml
PORT_NUM="${PORT:-8080}"
LISTEN_ADDR="0.0.0.0:${PORT_NUM}"

write_config_from_file() {
  src="$1"
  cp "$src" "$CONFIG_DEST"
  if id grok2api >/dev/null 2>&1; then
    chown grok2api:grok2api "$CONFIG_DEST" 2>/dev/null || true
  fi
  chmod 0600 "$CONFIG_DEST"
}

set +u
if [ -n "${GROK2API_CONFIG:-}" ]; then
  printf '%s\n' "$GROK2API_CONFIG" > /run/grok2api/config.yaml
  set -u
  write_config_from_file /run/grok2api/config.yaml
elif [ -f "${GROK2API_CONFIG_SOURCE:-/run/grok2api/config.yaml}" ]; then
  set -u
  write_config_from_file "${GROK2API_CONFIG_SOURCE}"
else
  echo "missing config: set GROK2API_CONFIG env or mount config.yaml" >&2
  exit 1
fi
set -u

echo "starting grok2api listen=${LISTEN_ADDR}" >&2
if command -v su-exec >/dev/null 2>&1 && id grok2api >/dev/null 2>&1; then
  exec su-exec grok2api:grok2api /app/grok2api --config "$CONFIG_DEST" --listen "$LISTEN_ADDR"
fi
exec /app/grok2api --config "$CONFIG_DEST" --listen "$LISTEN_ADDR"
