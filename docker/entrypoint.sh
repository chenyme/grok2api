#!/bin/sh
set -eu

umask 077

quality_guard_dir=/var/lib/grok2api-quality-guard
mkdir -p "${quality_guard_dir}"
chown grok2api:grok2api "${quality_guard_dir}"
chmod 0700 "${quality_guard_dir}"

if [ ! -f "${GROK2API_CONFIG_SOURCE}" ]; then
  echo "missing config: ${GROK2API_CONFIG_SOURCE}" >&2
  echo "mount config.yaml to /run/grok2api/config.yaml" >&2
  exit 1
fi

cp "${GROK2API_CONFIG_SOURCE}" /app/config.yaml
chown grok2api:grok2api /app/config.yaml
chmod 0600 /app/config.yaml

exec su-exec grok2api:grok2api "$@"
