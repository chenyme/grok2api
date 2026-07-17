# Grok2API 推理回放缓存改造文档

> 状态：已落地（核心路径）  
> 日期：2026-07-17  
> 参考实现：[router-for-me/CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)  
> 目标工程：`E:\Grok2api\grok2api`  
> **测试手册（重点）**：[reasoning-replay-cache-testing.md](./reasoning-replay-cache-testing.md)

---

## 0. 一句话结论

CLIProxyAPI 的「缓存」**不是**通用 HTTP 响应缓存，而是 **xAI 推理回放缓存（Reasoning Replay Cache）**：

- 上一轮响应里的 `encrypted_content` / tool call 由**服务端保存**
- 下一轮客户端若不带回，服务端**自动注入**到 `input`
- 与官方 `prompt_cache_key`（计费前缀缓存 + 账号粘滞）是**两套机制**

Grok2API **已有** prompt cache 粘滞与 token 统计，**缺少**服务端推理回放。本文描述如何补齐。

---

## 1. 背景与问题

### 1.1 现象

多轮对话（尤其 Claude Code / Anthropic Messages）时：

1. 上游返回 `reasoning.encrypted_content`（或映射为 `thinking.signature`）
2. 部分客户端下一轮**不会**把 signature / encrypted_content 完整带回
3. 上游要求历史中带可校验的 encrypted reasoning，否则多轮失败或推理断裂

### 1.2 CLIProxyAPI 的解法

在 `internal/cache/xai_reasoning_replay_cache.go` + `internal/runtime/executor/xai_reasoning_replay.go`：

| 时机 | 动作 |
|------|------|
| 本轮 `response.completed` | 抽取 output 中可回放 items → Store |
| 下一轮出站前 | Get → 过滤重复 → 插入 input |
| Compact 成功 / 无 reasoning 的成功完成 | Delete |
| 存储写失败 | **保留**旧条目 |

### 1.3 Grok2API 现状

| 能力 | 状态 | 位置 |
|------|------|------|
| 提取会话种子 | ✅ | `transport/http/inference/prompt_cache.go` |
| 跨租户哈希 `prompt_cache_key` | ✅ | `application/gateway/prompt_cache.go` |
| 注入上游 body | ✅ | `infra/provider/cli/adapter.go` → `injectPromptCacheKey` |
| 账号粘滞 | ✅ | `StickySessionRepository` + memory/redis |
| 客户端自带 signature 映射 | ✅ | `conversation/messages_request.go` / `messages_response.go` |
| **服务端代持并注入 encrypted reasoning** | ❌ | **本文改造目标** |

---

## 2. 参考实现要点（CLIProxyAPI）

### 2.1 源文件

| 路径 | 职责 |
|------|------|
| `internal/cache/xai_reasoning_replay_cache.go` | 存储：Get / Store / Delete，内存 + Home KV |
| `internal/runtime/executor/xai_reasoning_replay.go` | 应用：读注入、写回、清理 |
| `internal/runtime/executor/xai_executor.go` | 请求准备调用 apply；完成事件调用 cache |
| `internal/runtime/executor/codex_executor.go` | session key 解析、insert 位置 |

### 2.2 默认参数

| 项 | 值 |
|----|-----|
| TTL | 1 小时（滑动续期） |
| 内存最大条目 | 10240 |
| 淘汰批量 | 128（最旧优先） |
| 缓存键 | `modelName + sessionKey`（**不绑账号**，便于 failover） |
| 租户隔离 | 客户端 session 前加 API Key 哈希前缀 |

### 2.3 可缓存 item 类型

仅以下类型，且需规范化为上游接受的最小形态：

- `reasoning`：必须有合法 `encrypted_content`
- `message`：assistant 的 `output_text` / `refusal`
- `function_call` / `custom_tool_call`

必须存在至少一个 **replay anchor**（`reasoning` | `function_call` | `custom_tool_call`），纯 message 不入库。

### 2.4 注入规则摘要

1. input 中已有相同 `encrypted_content` → 跳过该 reasoning
2. 末条 assistant 文本与缓存 assistant **不一致** → **整批不注入**（防错绑会话）
3. tool call 仅当 input 中已有对应 `function_call_output` / `custom_tool_call_output` 时注入
4. 插入位置：对应 tool output 之前，或最后一个 assistant 消息之后

### 2.5 Session 边界来源（CPA 优先级）

1. 内部 execution session  
2. body `prompt_cache_key`  
3. Codex window / turn metadata  
4. Headers：`session_id` / `conversation_id` / `X-Codex-*`  
5. Claude Code session  
6. 无 key → **不缓存**

Grok2API 应对齐为：**使用已 `resolvePromptCacheIdentity` 后的 `PromptCacheKey`**（天然含 clientKey / provider / model / operation 隔离）。

---

## 3. 目标行为（验收标准）

1. Build 路径下，同一 `PromptCacheKey` 的 Turn1 完成后，服务端缓存 encrypted reasoning。  
2. Turn2 客户端不带 signature / encrypted_content 时，出站 body 的 `input` 中自动出现规范化后的 reasoning（及必要的 tool call）。  
3. 不同 ClientKey 即使用相同原始 session 种子，也**不共享**回放缓存。  
4. TTL 过期后 Get miss，不注入。  
5. Compact 成功或无 reasoning 的成功完成 → 删除缓存。  
6. 已有 `previous_response_id` 且走 stored response 时 → **不注入**（避免双状态）。  
7. 开关关闭时读写均 no-op。

---

## 4. 架构设计

### 4.1 原则

- **不**照搬 CPA 包级全局 `map` + 单例  
- **对齐** Grok2API：`repository` 接口 + `memory` / `redis` 实现 + 构造注入  
- 与 `StickySessionRepository` **分离**（粘滞绑 `accountID`，回放绑 `[][]byte` items）

### 4.2 组件关系

```text
                    ┌─────────────────────────────┐
                    │ ReasoningReplayRepository   │
                    │  Get / Set / Delete         │
                    └─────────────┬───────────────┘
                                  │
              ┌───────────────────┼───────────────────┐
              │                   │                   │
       memory 实现            redis 实现         (未来扩展)
              │                   │
              └───────────────────┼───────────────────┘
                                  │
              ┌───────────────────┴───────────────────┐
              │                                       │
   gateway.Service（写/清）              cli.Adapter（读/注入）
   · 完成态 Store                       · normalize 后
   · Compact Delete                     · inject prompt_cache_key 后
                                        · doRequest 前 Apply
```

### 4.3 数据流

```text
Client
  │ session / prompt_cache_key / X-Claude-Code-Session-Id
  ▼
inference handler  → extractPromptCacheSeed
  ▼
gateway.createResponseAt
  │ resolvePromptCacheIdentity → PromptCacheKey（仅 Build）
  │ selector 账号粘滞（已有）
  ▼
cli.Adapter.ForwardResponse
  │ Convert / normalize
  │ injectPromptCacheKey
  │ ★ ApplyReplay(body, model, PromptCacheKey)   ← 读
  ▼
Upstream Grok Build / XAI
  ▼
gateway 成功完成 / stream completed
  │ ★ StoreFromCompleted(model, PromptCacheKey, output)  ← 写
  ▼
Client
```

---

## 5. 修改文件清单

### 5.1 新建文件

| 文件 | 作用 |
|------|------|
| `backend/internal/application/gateway/reasoning_replay.go` | normalize / filter / insert / store / apply 纯逻辑 |
| `backend/internal/application/gateway/reasoning_replay_test.go` | 单元测试 |
| `backend/internal/infra/runtime/memory/reasoning_replay.go` | Memory 实现 |
| `backend/internal/infra/runtime/redis/reasoning_replay.go` | Redis 实现 |
| `backend/internal/infra/runtime/memory/reasoning_replay_test.go` | Memory 测试 |
| （可选）`backend/internal/infra/runtime/redis/reasoning_replay_integration_test.go` | Redis 集成测 |

### 5.2 修改文件

| 文件 | 改动摘要 |
|------|----------|
| `backend/internal/repository/runtime.go` | 新增 `ReasoningReplayRepository` 接口 |
| `backend/internal/infra/config/config.go` | Routing 增加 TTL / maxEntries / enabled；默认值与校验 |
| `config.example.yaml` | 文档化新配置项 |
| `backend/internal/app/application.go` | 创建 memory/redis 实现；注入 Gateway 与 CLI Adapter |
| `backend/internal/application/gateway/service.go` | 构造函数加 replay 依赖；完成态写缓存；compact/失败清理 |
| `backend/internal/infra/provider/cli/adapter.go` | 构造函数加 replay；出站前 Apply |
| `backend/internal/infra/provider/cli/adapter_test.go` | 注入相关用例 |
| `backend/internal/application/gateway/service_test.go` | 端到端回放用例（可用 fake repo） |

### 5.3 可选（二期）

| 文件 | 说明 |
|------|------|
| `domain/settings` + `application/settings` + settings HTTP | 热更新开关/TTL |
| 前端设置页 | 管理端展示 |
| Console / Web Provider | 本期不做；Console 会丢 `prompt_cache_key` |

### 5.4 明确不改

| 文件 | 原因 |
|------|------|
| `pkg/resultcache` | 仪表盘/审计短缓存，无关 |
| `prompt_cache.go`（identity） | 已正确，直接复用 |
| CLIProxyAPI 的 `signature_cache` | Antigravity 专用，非 Grok2API 主路径 |

---

## 6. 接口与配置设计

### 6.1 Repository 接口

在 `backend/internal/repository/runtime.go` 追加：

```go
// ReasoningReplayRepository 保存无状态多轮所需的上一轮可回放 output items。
// key 边界为 model + sessionKey；sessionKey 应使用已隔离的 PromptCacheKey。
type ReasoningReplayRepository interface {
	Get(ctx context.Context, model, sessionKey string, now time.Time) (items [][]byte, ok bool, err error)
	Set(ctx context.Context, model, sessionKey string, items [][]byte, expiresAt time.Time) error
	Delete(ctx context.Context, model, sessionKey string) error
}
```

### 6.2 Redis Key

```text
{keyPrefix}reasoning-replay:{hash16(model)}:{hash32(sessionKey)}
```

- Value：JSON 数组，元素为各 item 的原始 JSON 字节（或 base64 字符串数组）  
- TTL：配置项，读命中时滑动 `EXPIRE`

### 6.3 配置项

`config.example.yaml` 建议：

```yaml
routing:
  stickyTTL: 1h
  # [按需] 服务端推理回放缓存（CLIProxyAPI 同款能力）
  reasoningReplayEnabled: true
  reasoningReplayTTL: 1h
  reasoningReplayMaxEntries: 10240   # 仅 memory 生效
```

`config.Routing` 对应字段：

| 字段 | 类型 | 默认 |
|------|------|------|
| `ReasoningReplayEnabled` | bool | true |
| `ReasoningReplayTTL` | Duration | 1h |
| `ReasoningReplayMaxEntries` | int | 10240 |

校验：`TTL > 0 && TTL <= 24h`；`MaxEntries >= 100`（或关闭时忽略）。

---

## 7. 核心逻辑规范

### 7.1 Session Key

```text
if PromptCacheKey == "" → 不读写
else sessionKey = PromptCacheKey
```

说明：

- `resolvePromptCacheIdentity` 仅在 **ProviderBuild** 时赋值（见 `service.go`）  
- 本期回放**仅启用 Build**  
- 不要用未哈希的原始客户端 session 当全局 key

### 7.2 StoreFromCompleted

输入：完整 Responses JSON（至少含 `response.output` 或顶层 `output`，实现时统一解析路径）。

步骤：

1. 无有效 session → return  
2. 遍历 output，收集 `reasoning` / `message` / `function_call` / `custom_tool_call`  
3. `normalizeReplayItems`  
4. 无 anchor → `Delete`  
5. 有合法 items → `Set`（expiresAt = now + TTL）  
6. Set 失败 → 记日志，**不** Delete 旧值

### 7.3 ApplyReplay

输入：已 normalize 的 Responses body、model、sessionKey。

步骤：

1. 开关关 / 无 session / Get miss → 原样返回  
2. `filterReplayItemsForInput`  
3. 过滤后为空 → 原样返回  
4. `insertReplayItems`  
5. 返回新 body

### 7.4 与 previous_response_id

当 Gateway 已解析到 `ResponseOwnership`（stored response 路径）时：

- **跳过** Apply  
- **跳过** Store（或仍 Store 但 Apply 跳过；推荐双跳过，状态以上游 store 为准）

### 7.5 Compact

Compact 请求成功返回后：对该 sessionKey `Delete`。

---

## 8. 挂载点（精确到函数）

### 8.1 读：`cli.Adapter.ForwardResponse`

文件：`backend/internal/infra/provider/cli/adapter.go`

推荐顺序：

```text
1. NormalizeBody / ConvertRequest
2. injectPromptCacheKey
3. ★ ApplyReplay          // 新增
4. doResponseRequest
```

构造：`NewAdapter(..., replay repository.ReasoningReplayRepository, opts ...)`  
`opts` 至少含 `enabled`、`ttl`（ttl 主要用于写侧，读侧只需 Get）。

### 8.2 写：`gateway.Service`

文件：`backend/internal/application/gateway/service.go`

| 位置 | 动作 |
|------|------|
| 非流式成功拿到完整 body | `StoreFromCompleted` |
| 流式聚合到 completed 等价 payload | `StoreFromCompleted` |
| Compact 成功 | `Delete` |
| 明确「无 reasoning 完成」 | `Delete` |

构造：`NewService` 增加 `replay` 依赖与配置。

### 8.3 装配：`app.New`

文件：`backend/internal/app/application.go`

与 sticky 并列：

```text
switch runtimeStore.driver
  memory → memory.NewReasoningReplayStore(maxEntries)
  redis  → 基于同一 redis client 的 ReasoningReplayStore
注入 gateway + cli adapter
```

---

## 9. 测试计划

| 编号 | 用例 | 期望 |
|------|------|------|
| T1 | normalize 合法 reasoning | 保留 encrypted_content，去掉多余字段 |
| T2 | normalize 非法 encrypted | 丢弃，无 anchor 则不存 |
| T3 | filter 重复 encrypted | 不二次注入 |
| T4 | filter assistant 文本不一致 | 整批不注入 |
| T5 | insert 位置 | tool output 前或末 assistant 后 |
| T6 | Memory Get/Set/Delete + TTL | 过期 miss |
| T7 | 容量淘汰 | 超过 maxEntries 删最旧 |
| T8 | Gateway fake：Turn1 写 Turn2 读 | 出站 body 含 reasoning |
| T9 | 不同 ClientKey | 缓存隔离 |
| T10 | 无 PromptCacheKey | 不读写 |
| T11 | stored previous_response_id | 不注入 |
| T12 | enabled=false | no-op |

建议对照 CPA 测试：

- `internal/cache/xai_reasoning_replay_cache_test.go`  
- `xai_executor_test.go` 中 compact / shared-session 相关段  

---

## 10. 实施顺序

| 阶段 | 内容 | 风险 |
|------|------|------|
| P0 | 接口 + Memory + 纯逻辑 + 单测 | 低 |
| P1 | Adapter Apply + Gateway Store/Delete | 中（流式完成态取数） |
| P2 | application 装配 + config | 低 |
| P3 | Redis 实现 | 中 |
| P4 | 真机 Claude Code 多轮 / tool 多轮验收 | 验收 |
| P5 | （可选）设置热更、管理端 | 低 |

---

## 11. 风险与边界

| 风险 | 处理 |
|------|------|
| 误做成「相同 prompt 返回缓存答案」 | 文档与命名统一为 reasoning-replay，禁止全响应缓存 |
| 跨租户泄漏 | 强制使用 `resolvePromptCacheIdentity` 结果作 key |
| 内存膨胀 | maxEntries + TTL；encrypted blob 体积受条目上限约束 |
| 流式半包写入 | 仅 completed 全量 output 后写入 |
| 与 sticky 混淆 | 独立接口、独立 Redis 前缀 |
| Console/Web | 本期不启用 |

---

## 12. 工作量粗估

| 项 | 估时 |
|----|------|
| 存储 + 接口 | 0.5–1d |
| 纯逻辑 + 单测 | 1–1.5d |
| 挂载读写 + 流式 | 1d |
| Redis + 配置 | 0.5d |
| 联调验收 | 0.5–1d |
| **合计** | **约 3.5–5d** |

---

## 13. 最终总结：改什么

### 必须改（核心交付）

1. **新建**推理回放仓储接口  
   - `repository/runtime.go` → `ReasoningReplayRepository`

2. **新建** Memory / Redis 实现  
   - `infra/runtime/memory/reasoning_replay.go`  
   - `infra/runtime/redis/reasoning_replay.go`

3. **新建**回放业务逻辑  
   - `application/gateway/reasoning_replay.go`  
   - normalize / filter / insert / store / apply / clear

4. **改**出站注入  
   - `infra/provider/cli/adapter.go`  
   - normalize + inject prompt_cache_key **之后**、HTTP **之前**调用 Apply

5. **改**完成态写回与清理  
   - `application/gateway/service.go`  
   - 成功 completed → Store；compact / 无 reasoning → Delete

6. **改**进程装配  
   - `app/application.go`  
   - 创建 store 并注入 Gateway + CLI Adapter

7. **改**配置  
   - `config.example.yaml` + `infra/config/config.go`  
   - `reasoningReplayEnabled` / `reasoningReplayTTL` / `reasoningReplayMaxEntries`

8. **补**测试  
   - 纯逻辑、Memory、Gateway/Adapter 挂载用例

### 不要改

- `pkg/resultcache`（无关）  
- 现有 `prompt_cache` 身份哈希（直接复用）  
- Console/Web 主路径（本期）  
- 整包 HTTP 响应缓存（错误方向）

### 能力结果

| 改造前 | 改造后 |
|--------|--------|
| 仅官方 prompt_cache_key + 账号粘滞 | 额外具备服务端 encrypted reasoning 多轮续接 |
| 客户端不回传 signature 则多轮易挂 | 与 CLIProxyAPI 同款：服务端自动补洞 |

---

## 14. 参考链接

- 上游参考：https://github.com/router-for-me/CLIProxyAPI  
- 关键源文件（克隆后）：  
  - `internal/cache/xai_reasoning_replay_cache.go`  
  - `internal/runtime/executor/xai_reasoning_replay.go`  
  - `internal/runtime/executor/xai_executor.go`  
  - `internal/runtime/executor/codex_executor.go`（insert / session key）
