# 出口质量守护程序

出口质量守护程序同时支持“真实请求审计被动检测”和“固定 Prompt 主动探测”。它会测量可见
Token 速度、首字时间、分块数量和指令标记是否完整；异常节点会被临时隔离，复测恢复后才重新启用。

它是启发式熔断器，不是模型智力鉴定器。上游或中间层缓冲也可能造成瞬时数千
Token/s，因此建议先观察 JSON 日志，再根据实际流量调整阈值。

## 适用范围与前置条件

- 仅支持已经接入 grok2api 出口节点与请求审计的 Grok Build 流式请求。
- 每个受管节点应绑定可用于目标模型的账号，否则逐节点探测无法保证走指定出口。
- 需要一个专用探测 Client Key，以及能够访问管理员 API 的内部网络。
- 质量判断是启发式信号，不能证明模型能力被上游调整，也不能代替真实业务回归测试。

## 工作流程

1. 被动检测每 5 秒读取普通成功流式请求的新增审计，并按 `(输出 Token - Reasoning Token) / (总耗时 - 首字耗时)` 重算可见速度。
2. 主动检测调用仅管理员可访问的 `POST /api/admin/v1/egress-nodes/{id}/quality-test`。
3. grok2api 只从明确绑定到该节点的账号中选号，并发送固定流式 Prompt。
4. 硬阈值一次触发隔离；软阈值或探测错误需要连续命中。
5. 隔离节点仍可接受管理员探测，但不会承载普通用户请求。
6. 冷却结束后记录一次通用连接探测用于诊断，再以真实模型质量探测作为恢复判据，账号绑定保持不变。

普通 `/v1/*` 请求不能指定出口节点，也不能绕过节点禁用状态。

## 运行模式

- `passive`：只检测普通请求审计，不额外消耗模型 Token；守护程序隔离的节点仍会执行恢复探测。
- `active`：只按固定间隔逐节点主动测试。
- `hybrid`：同时开启两套检测器，推荐用于生产环境。

被动检测会忽略非流式请求、失败请求、少于 32 个可见 Token 的短回答，以及守护程序自己产生的审计。首次启动只建立基线，不追溯历史异常；审计 ID 去重状态会持久化，重启后不会重复处理。

通用 IP/Cloudflare 探针不作为恢复硬门槛：部分住宅出口可能无法访问探针站点，但访问 Grok 完全正常。真实模型质量请求才是最终判据。

## 管理界面

新版管理端左侧提供“质量守护”页面，显示守护进程新鲜度、当前模式、各节点可见 Token/s、首字延迟、打击计数、隔离状态和最近事件，也可以对单个节点立即执行一次真实模型质量检测。

页面还会显示自统计功能启用以来的自动检测次数、主动探测、被动审计、异常命中、隔离与恢复次数，以及主动探测产生的可见输出 Token。手动检测不计入累计值。代理的真实上下行字节数无法从 HTTPS/SSE 请求审计中可靠获得，因此页面不会用 Token 数伪装成代理流量。

主服务只公开脱敏后的状态，并仅向独立的运行配置文件写入可编辑策略。Docker 部署时，将同一个状态卷挂载到主服务，并设置：

```yaml
services:
  grok2api:
    environment:
      QUALITY_GUARD_STATE_FILE: /var/lib/grok2api-quality-guard/state.json
      QUALITY_GUARD_RUNTIME_CONFIG_FILE: /var/lib/grok2api-quality-guard/runtime-config.json
    volumes:
      - quality_guard_state:/var/lib/grok2api-quality-guard

  egress-quality-guard:
    volumes:
      - quality_guard_state:/var/lib/grok2api-quality-guard
```

未配置状态路径时页面会显示“尚未连接”；未配置运行配置路径时策略保持只读，均不会影响网关和 sidecar 的原有功能。策略保存后约 1 秒内热加载，不需要重启容器。状态与策略接口位于管理员鉴权边界内，且不会返回管理员密码、Client Key 密钥、代理地址、探针 Prompt 或模型回答正文。

## 防误杀设计

- 不删除节点，不修改账号绑定。
- 不会恢复管理员手动禁用的节点。
- 启用节点数低于 `QUALITY_GUARD_MIN_HEALTHY_NODES` 时拒绝继续隔离。
- 使用进程锁防止重复运行。
- 状态文件原子写入且权限为 `0600`。
- 日志不记录管理员令牌、代理地址或模型回答正文。
- 管理员访问令牌只保存在内存中。

## 配置与成本

将 `egress-quality-guard.env.example` 复制到部署目录并设置为 `0600`。建议创建一个
专用探测 Client Key，只开放目标 Build 模型，并设置足够的 RPM、并发和本地计费额度。

默认混合策略为：

- 每 5 秒检查一次真实请求审计；
- 每 1,800 秒主动测试五个节点，附加最多 30 秒抖动；
- 可见速度达到 1000 Token/s 立即隔离；
- 达到 500 Token/s 连续两次才隔离；
- 连续两次探测错误才隔离；
- 隔离 300 秒后复测；
- 始终至少保留 3 个可用出口。

五个节点每 30 分钟测试一次，每天产生 240 次模型请求。被动模式只增加少量数据库读取，不消耗额外模型 Token 或住宅推理流量。

## Docker Compose 快速接入

从仓库根目录执行：

```sh
sudo install -m 0600 \
  tools/egress-quality-guard/egress-quality-guard.env.example \
  /etc/grok2api-egress-quality-guard.env
sudo editor /etc/grok2api-egress-quality-guard.env

docker compose \
  -f docker-compose.yml \
  -f tools/egress-quality-guard/compose.override.example.yml \
  config --quiet
docker compose \
  -f docker-compose.yml \
  -f tools/egress-quality-guard/compose.override.example.yml \
  up -d --build grok2api egress-quality-guard
```

先确认受管节点、专用 Client Key、模型和最低健康节点数正确，再允许 sidecar 长期运行。不要把 `/etc/grok2api-egress-quality-guard.env`、状态卷或生产日志提交到仓库。

## 已知限制

- HTTPS/SSE 请求审计无法可靠给出代理上下行字节数，界面只展示主动探测的可见输出 Token，不把它称为网络流量。
- 中间层缓冲可能制造异常高的瞬时 Token/s，阈值需要根据自己的链路校准。
- 被动检测只处理完整、成功且可计算速度的流式请求；短回答和失败请求会被忽略。
- 首次启动只建立被动审计基线，累计统计也从该版本首次写入状态时开始。
- 手动质量检测用于诊断，不计入自动检测累计统计，也不会直接改变隔离状态。

## 运行

```sh
set -a
. /etc/grok2api-egress-quality-guard.env
set +a
python3 quality_guard.py --check-config
python3 quality_guard.py --once
```

可使用仓库内的 systemd 单元，也可以用 `Dockerfile` 构建独立 sidecar。完整环境变量和容器示例见英文 README。

安全部署要求见 [`SECURITY.md`](./SECURITY.md)。

运行测试：

```sh
python3 -m unittest -v tools/egress-quality-guard/quality_guard_test.py
```
