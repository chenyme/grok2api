#!/usr/bin/env bash
set -euo pipefail

# 复现点（PostgreSQL）：
# - 如果 grok_settings.data 里缺少 "grok" 段（例如旧版本/脏数据），服务端会返回 POST 200 OK，
#   但 "grok" 相关更新（含 bypass_server / bypass_baseurl）在保存时被静默丢弃。
#
# 修复后：保存时会自动补齐缺失段并写入。
#
# 使用方法（按需手动执行每一步）：
# 1) export STORAGE_MODE=postgres
# 2) export DATABASE_URL='postgresql://user:pass@host:5432/db'
# 3) 启动服务（例如：python -m uvicorn main:app --port 8000）
# 4) 获取管理员 token：
#    TOKEN=$(curl -sS http://127.0.0.1:8000/api/login -H 'content-type: application/json' \
#      -d '{"username":"admin","password":"admin"}' | python -c 'import sys,json; print(json.load(sys.stdin)["token"])')
# 5) 造“缺 grok 段”的脏数据（稳定复现的关键）：
#    psql "$DATABASE_URL" -c "UPDATE grok_settings SET data = data - 'grok' WHERE id=(SELECT id FROM grok_settings ORDER BY id DESC LIMIT 1);"
# 6) 用 API 保存 bypass 配置：
#    curl -sS http://127.0.0.1:8000/api/settings -H "authorization: Bearer $TOKEN" -H 'content-type: application/json' -d @- <<'JSON'
#    {"global_config":null,"grok_config":{"bypass_server":true,"bypass_baseurl":"http://127.0.0.1:8080"}}
#    JSON
# 7) 验证是否真的落库：
#    psql "$DATABASE_URL" -c "SELECT data->'grok'->>'bypass_server' AS bypass_server, data->'grok'->>'bypass_baseurl' AS bypass_baseurl FROM grok_settings ORDER BY id DESC LIMIT 1;"

