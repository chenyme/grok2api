#!/usr/bin/env sh
set -eu

ROOT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
DATA_DIR="$ROOT_DIR/data"
LOG_DIR="$ROOT_DIR/logs"
TMP_DIR="$DATA_DIR/tmp"

mkdir -p "$DATA_DIR" "$LOG_DIR" "$TMP_DIR"

if [ ! -f "$DATA_DIR/config.toml" ]; then
  cat > "$DATA_DIR/config.toml" <<'TOML'
[grok]
temporary = true
stream = true
thinking = true
dynamic_statsig = true
filter_tags = ["xaiartifact","xai:tool_usage_card","grok:render"]
timeout = 120
base_proxy_url = ""
asset_proxy_url = ""
cf_clearance = ""
max_retry = 3
retry_status_codes = [401,429,403]

[app]
app_url = "http://127.0.0.1:8000"
app_key = "grok2api"
api_key = ""
image_format = "url"
video_format = "url"

[token]
auto_refresh = true
refresh_interval_hours = 8
fail_threshold = 5
save_delay_ms = 500
reload_interval_sec = 30

[cache]
enable_auto_clean = true
limit_mb = 1024

[performance]
assets_max_concurrent = 25 # 推荐 25
media_max_concurrent = 50 # 推荐 50
usage_max_concurrent = 25 # 推荐 25
assets_delete_batch_size = 10 # 推荐 10
admin_assets_batch_size = 10 # 推荐 10
TOML
fi

if [ ! -f "$DATA_DIR/token.json" ]; then
  echo "{}" > "$DATA_DIR/token.json"
fi

chmod 600 "$DATA_DIR/config.toml" "$DATA_DIR/token.json" || true
