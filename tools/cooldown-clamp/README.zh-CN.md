# 账号冷却封顶

官方 grok2api 镜像在上游 504/429 带很长 `Retry-After` 时，会把 Build 账号冻数小时。
进程内的 `cooldownMax` 发生在套用 Retry-After 之前，所以本 sidecar 直接改
`provider_accounts.cooldown_until`，把剩余冷却压回运营设定的重试窗口。

脚本会截断 Go/SQLite 的纳秒时间戳。Python `fromisoformat` 无法解析 9 位小数，
之前会直接跳过该行，导致 24 小时冷却一直留在库里。

## 行为

1. 每 15 秒扫描 `cooldown_until` 超过 `--max-remaining`（默认 1 分钟）的账号。
2. 把 `cooldown_until` 改成现在 + 最大剩余时间，并清掉 `failure_count`。
3. 对该账号 `PATCH /api/admin/v1/accounts/:id` `{enabled: true}`，丢掉选号内存 overlay 里的旧冷却。

管理员密码只从 `config.yaml` 的 `bootstrapAdmin` 读取，不会打印。

## 运行

```bash
python3 tools/cooldown-clamp/clamp_account_cooldown_test.py
chmod 700 tools/cooldown-clamp/clamp-account-cooldown-loop.sh
sudo cp tools/cooldown-clamp/grok2api-cooldown-clamp.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now grok2api-cooldown-clamp.service
```

可用 `--db`、`--config`、`--api-base`、`--max-remaining` 覆盖路径。这是官方镜像合入 `markFailure` 的 Retry-After 封顶之前的宿主机兜底。
