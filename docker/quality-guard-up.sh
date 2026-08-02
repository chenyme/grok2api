#!/bin/sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo_root"

if ! command -v docker >/dev/null 2>&1; then
  echo "docker is required" >&2
  exit 1
fi
if [ ! -f config.yaml ]; then
  echo "config.yaml is missing; create it from config.example.yaml first" >&2
  exit 1
fi

if [ ! -f .env ]; then
  cp .env.example .env
  chmod 0600 .env
  echo "created .env from .env.example"
fi

if [ -n "${QUALITY_GUARD_ENV_FILE:-}" ]; then
  guard_env_file=$QUALITY_GUARD_ENV_FILE
else
  guard_env_file=tools/egress-quality-guard/egress-quality-guard.env
  configured_guard_env=$(sed -n 's/^QUALITY_GUARD_ENV_FILE[[:space:]]*=[[:space:]]*//p' .env | tail -n 1)
  if [ -n "$configured_guard_env" ]; then
    guard_env_file=$configured_guard_env
  fi
fi
case "$guard_env_file" in
  /*) ;;
  *) guard_env_file="$repo_root/$guard_env_file" ;;
esac

if [ ! -f "$guard_env_file" ]; then
  guard_env_dir=$(dirname "$guard_env_file")
  mkdir -p "$guard_env_dir"

  printf "Grok2API admin username [admin]: "
  read -r admin_username
  admin_username=${admin_username:-admin}

  while :; do
    printf "Grok2API admin password: "
    stty -echo
    read -r admin_password
    stty echo
    printf "\n"
    [ -n "$admin_password" ] && break
    echo "password must not be empty" >&2
  done

  printf "Dedicated probe Client Key ID [1]: "
  read -r client_key_id
  client_key_id=${client_key_id:-1}
  case "$client_key_id" in
    *[!0-9]*|0)
      echo "Client Key ID must be a positive integer" >&2
      exit 1
      ;;
  esac

  printf "Probe model [grok-4.5]: "
  read -r probe_model
  probe_model=${probe_model:-grok-4.5}

  old_umask=$(umask)
  umask 077
  {
    printf '%s\n' 'GROK2API_BASE_URL=http://grok2api:8000'
    printf '%s\n' "GROK2API_ADMIN_USERNAME=$admin_username"
    printf '%s\n' "GROK2API_ADMIN_PASSWORD=$admin_password"
    printf '%s\n' "QUALITY_GUARD_CLIENT_KEY_ID=$client_key_id"
    printf '%s\n' "QUALITY_GUARD_MODEL=$probe_model"
    printf '%s\n' 'QUALITY_GUARD_MODE=hybrid'
    printf '%s\n' 'QUALITY_GUARD_ACTIVE_INTERVAL_SECONDS=1800'
    printf '%s\n' 'QUALITY_GUARD_PASSIVE_POLL_SECONDS=5'
    printf '%s\n' 'QUALITY_GUARD_SOFT_TPS=500'
    printf '%s\n' 'QUALITY_GUARD_HARD_TPS=1000'
    printf '%s\n' 'QUALITY_GUARD_CONSECUTIVE_SOFT=2'
    printf '%s\n' 'QUALITY_GUARD_CONSECUTIVE_ERRORS=2'
    printf '%s\n' 'QUALITY_GUARD_QUARANTINE_SECONDS=300'
    printf '%s\n' 'QUALITY_GUARD_MIN_HEALTHY_NODES=3'
    printf '%s\n' 'QUALITY_GUARD_MAX_OUTPUT_TOKENS=384'
    printf '%s\n' 'QUALITY_GUARD_INSECURE_TLS=false'
  } > "$guard_env_file"
  umask "$old_umask"
  chmod 0600 "$guard_env_file"
  unset admin_password
  echo "created $guard_env_file"
else
  chmod 0600 "$guard_env_file"
  echo "using existing $guard_env_file"
fi

QUALITY_GUARD_ENV_FILE="$guard_env_file" docker compose --profile quality-guard config --quiet
QUALITY_GUARD_ENV_FILE="$guard_env_file" docker compose --profile quality-guard build egress-quality-guard
QUALITY_GUARD_ENV_FILE="$guard_env_file" docker compose --profile quality-guard run --rm --no-deps egress-quality-guard --check-config
QUALITY_GUARD_ENV_FILE="$guard_env_file" docker compose --profile quality-guard up -d --build
QUALITY_GUARD_ENV_FILE="$guard_env_file" docker compose --profile quality-guard ps

echo "Quality guard is configured. Open the Quality Guard page in the admin console."
