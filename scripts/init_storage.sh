#!/usr/bin/env sh
set -eu

ROOT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
DATA_DIR="${DATA_DIR:-$ROOT_DIR/data}"
LOG_DIR="${LOG_DIR:-$ROOT_DIR/logs}"
TMP_DIR="${TMP_DIR:-$DATA_DIR/tmp}"
DEFAULT_CONFIG="$ROOT_DIR/config.defaults.toml"

# Install custom CA certificates (useful for MITM proxies like Surge).
# Place PEM-encoded certs as *.crt under $DATA_DIR/certs/.
CERTS_DIR="$DATA_DIR/certs"

mkdir -p "$DATA_DIR" "$LOG_DIR" "$TMP_DIR"

if [ -d "$CERTS_DIR" ]; then
  mkdir -p /usr/local/share/ca-certificates
  installed=0
  for cert in "$CERTS_DIR"/*.crt; do
    [ -e "$cert" ] || continue
    cp -f "$cert" "/usr/local/share/ca-certificates/$(basename "$cert")"
    installed=1
  done
  if [ "$installed" -eq 1 ]; then
    update-ca-certificates >/dev/null 2>&1 || true
  fi
fi

if [ ! -f "$DATA_DIR/config.toml" ]; then
  cp "$DEFAULT_CONFIG" "$DATA_DIR/config.toml"
fi

if [ ! -f "$DATA_DIR/token.json" ]; then
  echo "{}" > "$DATA_DIR/token.json"
fi

chmod 600 "$DATA_DIR/config.toml" "$DATA_DIR/token.json" || true
