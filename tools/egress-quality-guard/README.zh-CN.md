# 出口质量守护程序

出口质量守护程序同时支持“真实请求审计被动检测”和“固定 Prompt 主动探测”。被动检测命中
硬阈值会立即隔离节点；软阈值仍需固定 Prompt 主动复测确认。

它是启发式熔断器，不是模型智力鉴定器。上游或中间层缓冲也可能造成瞬时数千
Token/s，因此建议先观察 JSON 日志，再根据实际流量调整阈值。

## 适用范围与前置条件

- 仅支持已经接入 grok2api 出口节点与请求审计的 Grok Build 流式请求。
- 每个受管节点应绑定可用于目标模型的账号，否则逐节点探测无法保证走指定出口。
- 需要一个专用探测 Client Key，以及能够访问管理员 API 的内部网络。
- 质量判断是启发式信号，不能证明模型能力被上游调整，也不能代替真实业务回归测试。

## 工作流程

1. 被动检测每 5 秒读取普通成功流式请求的新增审计，并按 grok2api 面板同口径的 `输出 Token / (总耗时 - 首字耗时)` 计算速度；输出 Token 故意包含 Reasoning Token。
2. 主动检测调用仅管理员可访问的 `POST /api/admin/v1/egress-nodes/{id}/quality-test`。
3. grok2api 优先使用明确绑定到该节点的账号；如果这些账号不可调度，则借用任意健康账号，但仍强制实际请求走被测节点，再发送固定流式 Prompt。
4. 普通真实请求达到硬阈值时立即隔离；达到软阈值时触发一次固定 Prompt 主动复测。
5. 主动复测达到硬阈值会立即隔离；主动软异常必须达到配置的连续次数。
6. 隔离节点仍可接受管理员探测，但不会承载普通用户请求。
7. 冷却结束后记录一次通用连接探测用于诊断，再以真实模型质量探测作为恢复判据，账号绑定保持不变。

普通 `/v1/*` 请求不能指定出口节点，也不能绕过节点禁用状态。

## 运行模式

- `passive`：轮询普通请求审计本身不消耗模型 Token；硬异常立即隔离，软异常会额外执行一次主动确认探测，守护程序隔离的节点仍会执行恢复探测。
- `active`：只按固定间隔逐节点主动测试。
- `hybrid`：同时开启两套检测器，推荐用于生产环境。

被动检测会忽略非流式请求、失败请求、少于 32 个输出 Token 的短回答，以及守护程序自己产生的审计。首次启动只建立基线，不追溯历史异常；审计 ID 去重状态会持久化，重启后不会重复处理。

通用 IP/Cloudflare 探针不作为恢复硬门槛：部分住宅出口可能无法访问探针站点，但访问 Grok 完全正常。真实模型质量请求才是最终判据。

## 严格隔离与换 IP

设置 `QUALITY_GUARD_FAIL_CLOSED=true` 后，软阈值、硬阈值和无法形成有效生成窗口的
可疑样本都会先摘除节点，再进行确认；最低健康节点数不再阻止隔离。短生成窗口产生的
瞬时高 TPS 会先在原 IP 上主动复测，复测正常即立即恢复，避免因为流式缓冲误换 IP。

可通过 `QUALITY_GUARD_ROTATION_URL` 接入受信任的内部换 IP Webhook，并用
`QUALITY_GUARD_ROTATABLE_NODE_IDS` 限制允许轮换的节点。确认异常后，守护程序会先调用
Webhook，确认出口发生变化，再执行一次真实模型质量检测；检测正常立即恢复，否则保持
隔离。Webhook 请求不包含代理凭据。

仓库提供了可选的 `session_rotator.py`，用于用户名带 `sid-...-t-...` 的 1024Proxy
粘性会话。它应与 Mihomo 控制器运行在同一受信任主机，仅监听回环地址，并只挂载需要
更新的凭据列表和 Mihomo 配置文件。

主动探测连续失败达到 `QUALITY_GUARD_CONSECUTIVE_ERRORS` 后才会隔离并换 IP。如果失败发生在
账号调度阶段，例如整个 Grok Build 账号池当前都不可调度，后端会返回独立错误码；守护程序按
`QUALITY_GUARD_NO_ACCOUNT_BACKOFF_SECONDS` 延后复测并抑制重复日志，不累计代理故障，也不执行
无意义的换 IP。节点仍保持隔离，直到真实模型质量检测通过。

## 管理界面

新版管理端左侧提供“质量守护”页面，显示守护进程新鲜度、当前模式、各节点与 grok2api 面板同口径的输出 Token/s、首字延迟、打击计数、隔离状态和最近事件，也可以对单个节点立即执行一次真实模型质量检测。

页面还会显示自统计功能启用以来的自动检测次数、主动探测、被动审计、异常命中、隔离与恢复次数，以及主动探测产生的输出 Token（包含推理 Token）。手动检测不计入累计值。代理的真实上下行字节数无法从 HTTPS/SSE 请求审计中可靠获得，因此页面不会用 Token 数伪装成代理流量。

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
- 严格模式会覆盖最低健康节点保护：无法确认质量时宁可无可用节点，也不调度可疑出口。
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

- HTTPS/SSE 请求审计无法可靠给出代理上下行字节数，界面只展示主动探测的输出 Token，不把它称为网络流量。
- 中间层缓冲可能制造异常高的瞬时 Token/s，阈值需要根据自己的链路校准。
- 被动检测只处理完整、成功且可计算速度的流式请求；短回答和失败请求会被忽略。
- 真实请求可能在输出已有文件、长常量或缓存内容，因此被动硬阈值策略偏激进；可按业务情况调高 `hard_tps`，软阈值仍会主动复测后再决定是否隔离。
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
