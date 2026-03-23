#!/usr/bin/env sh
set -eu

# Hugging Face Spaces runtime defaults:
# - keep local storage unless the user explicitly overrides it
# - prefer /data when available (persistent volume), otherwise fall back to /tmp
# - disable file logging by default to avoid noisy ephemeral writes
# - honor injected PORT if SERVER_PORT is not set
if [ -n "${SPACE_ID:-}" ] || [ -n "${SPACE_HOST:-}" ]; then
  : "${SERVER_STORAGE_TYPE:=local}"
  : "${SERVER_STORAGE_URL:=}"
  : "${SERVER_WORKERS:=1}"
  : "${SERVER_HOST:=0.0.0.0}"

  if [ -n "${PORT:-}" ]; then
    SERVER_PORT="$PORT"
  fi
  : "${SERVER_PORT:=8000}"

  if [ -z "${DATA_DIR:-}" ]; then
    if mkdir -p /data 2>/dev/null; then
      DATA_DIR="/data"
    else
      DATA_DIR="/tmp/grok2api-data"
    fi
  fi

  : "${TMP_DIR:=$DATA_DIR/tmp}"
  : "${LOG_DIR:=$DATA_DIR/logs}"
  : "${LOG_FILE_ENABLED:=false}"

  export SERVER_STORAGE_TYPE SERVER_STORAGE_URL SERVER_WORKERS SERVER_HOST SERVER_PORT
  export DATA_DIR TMP_DIR LOG_DIR LOG_FILE_ENABLED
fi

/app/scripts/init_storage.sh

exec "$@"
