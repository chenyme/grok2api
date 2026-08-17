# 出口质量守护程序

出口质量守护程序同时支持“真实请求审计被动检测”和“固定 Prompt 主动探测”。被动检测命中
硬阈值会立即隔离节点；软阈值仍需固定 Prompt 主动复测确认。

它是启发式熔断器，不是模型智力鉴定器。上游或中间层缓冲也可能造成瞬时数千
Token/s，因此建议先观察 JSON 日志，再根据实际流量调整阈值。

## 适用范围与前置条件

- 仅支持已经接入 grok2api 出口节点与请求审计的 Grok Build 流式请求。
- 至少应有一个可调度的 Grok Build 账号能够使用目标模型；不要求每个受管节点都绑定账号。
- 主程序会自动创建不可导出的系统探测身份；sidecar 只通过 Compose 内网访问权限受限的内部 API。
- 质量判断是启发式信号，不能证明模型能力被上游调整，也不能代替真实业务回归测试。

## 工作流程

1. 被动检测每 5 秒读取普通成功流式请求的新增审计，并按 grok2api 面板同口径计算速度：输出 Token 包含 Reasoning Token，分母是生成窗口。默认是 `总耗时 - 首字耗时`；若这段尾巴既短于首字等待又不足 1 秒，则改用全程，避免加密思考被挤进最后几十毫秒。
2. 主动检测调用质量守护专用内部探测接口；该凭据不能导出账号、管理管理员或访问其他管理 API。
3. grok2api 优先使用明确绑定到该节点的账号；如果这些账号不可调度，则借用任意健康账号，但仍强制实际请求走被测节点，再发送固定流式 Prompt。即使其他 Provider 暴露同名模型，后端也会把探测路由固定为 Grok Build。
4. 普通真实请求达到硬阈值时立即隔离；达到软阈值时触发一次固定 Prompt 主动复测。
5. 主动复测达到硬阈值会立即隔离；主动软异常必须达到配置的连续次数。
6. 隔离节点仍可接受管理员探测，但不会承载普通用户请求。
7. 冷却结束后记录一次通用连接探测用于诊断，再以真实模型质量探测作为恢复判据，账号绑定保持不变。

普通 `/v1/*` 请求不能指定出口节点，也不能绕过节点禁用状态。
仅发生在质量探测中的模糊 403 不会冷却借用账号；明确的凭据失效、账号封禁和额度信号仍按原有规则处理。

## 运行模式

- `passive`：轮询普通请求审计本身不消耗模型 Token；硬异常立即隔离，软异常会额外执行一次主动确认探测，守护程序隔离的节点仍会执行恢复探测。
- `active`：只按固定间隔逐节点主动测试。
- `hybrid`：同时开启两套检测器，推荐用于生产环境。

被动检测会忽略非流式请求、失败请求、少于 32 个输出 Token 的短回答，以及守护程序自己产生的审计。首次启动只建立基线，不追溯历史异常；审计 ID 去重状态会持久化，重启后不会重复处理。

通用 IP/Cloudflare 探针不作为恢复硬门槛：部分住宅出口可能无法访问探针站点，但访问 Grok 完全正常。真实模型质量请求才是最终判据。

## 严格隔离与换 IP

设置 `qualityGuard.failClosed: true` 后，软阈值、硬阈值和无法形成有效生成窗口的
可疑样本都会先摘除节点，再进行确认；最低健康节点数不再阻止隔离。短生成窗口产生的
瞬时高 TPS 会先在原 IP 上主动复测，复测正常即立即恢复，避免因为流式缓冲误换 IP。

可通过 `qualityGuard.rotationURL` 接入受信任的内部换 IP Webhook，并用
`qualityGuard.rotatableNodeIDs` 限制允许轮换的节点。确认异常后，守护程序会先调用
Webhook，确认出口发生变化，再执行一次真实模型质量检测；检测正常立即恢复，否则保持
隔离。Webhook 请求不包含代理凭据。

仓库提供了可选的 `session_rotator.py`，用于用户名带 `sid-...-t-...` 的 1024Proxy
粘性会话。它应与 Mihomo 控制器运行在同一受信任主机，仅监听回环地址，并只挂载需要
更新的凭据列表和 Mihomo 配置文件。

主动探测连续失败达到 `qualityGuard.consecutiveErrors` 后才会隔离并换 IP。如果失败发生在
账号调度阶段，例如整个 Grok Build 账号池当前都不可调度，后端会返回独立错误码；守护程序按
`qualityGuard.noAccountBackoff` 延后复测并抑制重复日志，不累计代理故障，也不执行
无意义的换 IP。节点仍保持隔离，直到真实模型质量检测通过。

## 管理界面

新版管理端左侧提供“质量守护”页面，显示守护进程新鲜度、当前模式、各节点与 grok2api 面板同口径的输出 Token/s、首字延迟、打击计数、隔离状态和最近事件，也可以对单个节点立即执行一次真实模型质量检测。

被固定回退策略引用的节点不能在不破坏路由策略的情况下禁用。守护程序会自动识别并排除这类节点，界面显示为“固定回退（受保护）”。如需让它参与自动隔离，应先移除或调整对应的固定回退策略。

页面还会显示自统计功能启用以来的自动检测次数、主动探测、被动审计、异常命中、隔离与恢复次数，以及主动探测产生的输出 Token（包含推理 Token）。手动检测不计入累计值。代理的真实上下行字节数无法从 HTTPS/SSE 请求审计中可靠获得，因此页面不会用 Token 数伪装成代理流量。

主 Compose 统一管理私有共享卷。grok2api 会把 `config.yaml` 中的配置规范化后写入带版本的 bootstrap 文件，并从现有 `jwtSecret` 派生仅供质量守护内部接口使用的凭据。sidecar 不读取、保存或使用管理员密码。管理界面保存的策略约 1 秒内热加载；任何公开或管理接口都不会返回内部凭据、Client Key 密钥、代理地址、探针 Prompt 或模型回答正文。

## 防误杀设计

- 不删除节点，不修改账号绑定。
- 不会恢复管理员手动禁用的节点。
- 启用节点数低于 `qualityGuard.minimumHealthyNodes` 时拒绝继续隔离。
- 严格模式会覆盖最低健康节点保护：无法确认质量时宁可无可用节点，也不调度可疑出口。
- 使用进程锁防止重复运行。
- 状态文件原子写入且权限为 `0600`。
- 日志不记录管理员令牌、代理地址或模型回答正文。
- 内部凭据使用常量时间比较，并且只允许访问六个质量守护所需的出口/审计路由。

## 配置与成本

所有用户配置统一放在主 `config.yaml`。主程序会自动创建并稳定复用一个隐藏、仅限 Build 的系统身份，用于探测授权、计费和审计归属；管理员无需创建、复制、选择或填写任何 Client Key：

```yaml
qualityGuard:
  enabled: true
  model: "grok-4.5"
  mode: hybrid
  activeInterval: 30m
  passivePollInterval: 5s
  softTPS: 500
  hardTPS: 1000
  consecutiveSoft: 2
  consecutiveErrors: 2
  quarantineDuration: 5m
  noAccountBackoff: 5m
  minimumHealthyNodes: 3
  failClosed: false
  nodeIDs: []
```

旧预览版配置中的 `clientKeyID` 会被兼容读取但忽略，可直接删除。升级过程不会自动删除管理员以前手工创建的 Key，避免误删仍被其他业务使用的凭据。

`nodeIDs` 留空时会分页读取全部 Grok Build 节点；固定回退节点会被标记为受保护并排除，不会因为节点数超过单页上限而静默漏管。换 IP 等高级字段见 `config.example.yaml`。

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

仓库主 `docker-compose.yml` 已通过可选的 `quality-guard` profile 集成 sidecar。
普通执行 `docker compose up -d` 不会启动它，也不会产生主动探测流量。

从仓库根目录执行：

```sh
docker compose --profile quality-guard config --quiet
docker compose --profile quality-guard up -d --build
```

以后修改 `config.yaml` 中的 `qualityGuard` 基础配置时，执行
`docker compose --profile quality-guard restart grok2api egress-quality-guard` 让主程序重新生成 bootstrap。管理页面保存的运行策略仍会热加载，无需重启。

先确认受管节点、模型和最低健康节点数正确，再允许 sidecar 长期运行。不要提交状态卷或生产日志。只停止守护程序可执行
`docker compose --profile quality-guard stop egress-quality-guard`，不会影响主 API。

## 已知限制

- HTTPS/SSE 请求审计无法可靠给出代理上下行字节数，界面只展示主动探测的输出 Token，不把它称为网络流量。
- 中间层缓冲可能制造异常高的瞬时 Token/s，阈值需要根据自己的链路校准。
- 被动检测只处理完整、成功且可计算速度的流式请求；短回答和失败请求会被忽略。
- 真实请求可能在输出已有文件、长常量或缓存内容，因此被动硬阈值策略偏激进；可按业务情况调高 `hard_tps`，软阈值仍会主动复测后再决定是否隔离。
- 首次启动只建立被动审计基线，累计统计也从该版本首次写入状态时开始。
- 手动质量检测用于诊断，不计入自动检测累计统计，也不会直接改变隔离状态。

安全部署要求见 [`SECURITY.md`](./SECURITY.md)。

运行测试：

```sh
python3 -m unittest -v tools/egress-quality-guard/quality_guard_test.py tools/egress-quality-guard/session_rotator_test.py
```
