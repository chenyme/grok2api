# 推理回放缓存 · 测试手册（重点）

> 配套：`reasoning-replay-cache-modification.md`  
> 核心问题：**怎么证明「有缓存」？怎么区分两种缓存？**

---

## 0. 先分清：你可能在测错东西

| 名称 | 是什么 | 成功信号 | 失败/未命中信号 |
|------|--------|----------|-----------------|
| **A. 官方 Prompt Cache** | 上游 Grok 对相同前缀的 token 计费优惠；Grok2API 只传 `prompt_cache_key` + 粘滞账号 | 响应 `usage` 里 **cached tokens > 0**；管理端审计「缓存输入」上升 | `cached_tokens = 0`（短对话、首轮、key 变了都正常） |
| **B. 推理回放缓存（本文）** | 服务端保存上一轮 `encrypted_content`，下一轮客户端没带时注入 | **出站请求 body 的 input 里出现客户端没发的 encrypted_content** | Turn2 出站 input 仍无 reasoning；或多轮直接 4xx |

**不要用「答案是否相同」判断 B。**  
B 不是结果缓存；同一问题两轮答案可以不同，只要 encrypted 被续上即可。

**当前 Grok2API 只有 A，没有 B。**  
落地 B 之前：用 §1 测 A；落地 B 之后：用 §2–§5 测 B。

---

## 1. 测「官方 Prompt Cache」（现成可测）

### 1.1 前置

- Build 账号在线、有额度  
- 模型已启用  
- 客户端密钥 `g2a_...`  
- **同一** `prompt_cache_key`（或同一 Claude session 头）连打多轮  

### 1.2 curl 模板

```bash
# 变量
BASE=http://127.0.0.1:8000
KEY=g2a_你的密钥
MODEL=你的对外模型名   # 管理端「模型管理」里的 public id
CACHE=test-session-001

# Turn 1
curl -sS "$BASE/v1/responses" \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d "{
    \"model\": \"$MODEL\",
    \"prompt_cache_key\": \"$CACHE\",
    \"input\": \"请用三句话介绍 Go 的 goroutine\",
    \"stream\": false
  }" | tee turn1.json

# Turn 2（同一 CACHE，带上上一轮对话上下文更好触发前缀缓存）
curl -sS "$BASE/v1/responses" \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d "{
    \"model\": \"$MODEL\",
    \"prompt_cache_key\": \"$CACHE\",
    \"input\": [
      {\"role\": \"user\", \"content\": \"请用三句话介绍 Go 的 goroutine\"},
      {\"role\": \"assistant\", \"content\": \"（这里可粘贴 turn1 的 assistant 文本）\"},
      {\"role\": \"user\", \"content\": \"再举一个 channel 的例子\"}
    ],
    \"stream\": false
  }" | tee turn2.json
```

### 1.3 看哪里 = 命中

在 `turn2.json` 里找（字段名因上游版本可能略有差异）：

```text
usage.input_tokens
usage.input_tokens_details.cached_tokens   # 或 cached_input_tokens
```

**判断：**

| 结果 | 含义 |
|------|------|
| Turn2 `cached_tokens > 0` | 官方 prompt cache **命中**（至少部分前缀复用） |
| 一直是 0 | 不一定坏：内容太短、模型不支持、key 每轮变了、账号切换导致缓存冷 |

### 1.4 管理端交叉验证

1. 打开 **请求审计**  
2. 同一会话两轮请求  
3. 看「缓存输入 / Cached」列或 token 明细  
4. 缓存命中率 = `cachedInputTokens / inputTokens`

### 1.5 用 session 头代替 body key（Claude Code 风格）

```bash
curl -sS "$BASE/v1/messages" \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -H "X-Claude-Code-Session-Id: claude-sess-001" \
  -d "{
    \"model\": \"$MODEL\",
    \"max_tokens\": 256,
    \"messages\": [{\"role\": \"user\", \"content\": \"hi\"}]
  }"
```

Grok2API 会把该头变成隔离后的 `prompt_cache_key` 再粘滞账号。  
**同一 Session-Id 连打** → 审计里应尽量落在同一账号（粘滞）；cached tokens 仍看 usage。

---

## 2. 测「推理回放缓存」（改造后的重点）

### 2.1 唯一可靠判据

```text
客户端 Turn2 请求里没有 encrypted_content / thinking.signature
        ↓
但服务端发往上游的 body.input 里有 type=reasoning 且 encrypted_content 非空
        ↓
= 回放缓存 HIT
```

客户端响应里「像不像上一轮」**不能**当证据。

### 2.2 三种可观测手段（实现时至少做一种）

#### 手段 1：专用 Debug 日志（推荐必做）

实现时在 Apply / Store 打日志（默认 Debug，可环境变量打开）：

```text
reasoning_replay_store  model=... session=... items=2 bytes=...
reasoning_replay_hit    model=... session=... injected=1 skipped=...
reasoning_replay_miss   model=... session=... reason=empty_key|not_found|filtered
reasoning_replay_delete model=... session=... reason=compact|no_anchor
```

测法：

```bash
# 按你们实际日志方式 tail；示例
# docker compose logs -f grok2api | findstr reasoning_replay
```

Turn1 后应见 `store`；Turn2（故意不带 signature）应见 `hit`。

#### 手段 2：Mock 上游（单元/集成最稳）

不接真 Grok，用 `httptest.Server` 假装上游：

1. Turn1 返回带 `output: [{type:reasoning, encrypted_content:"SIG_A"}, {type:message,...}]`  
2. 捕获 **第二次** 出站 POST body  
3. 断言 `input` 中含 `encrypted_content == "SIG_A"`  
4. 而客户端第二次请求 JSON **故意不带** SIG_A  

这是 CI 里最硬的验收。

#### 手段 3：临时 Dump 出站 body（联调）

仅本地：在 `adapter.doResponseRequest` 前写文件：

```text
data/debug/outbound-{request_id}.json
```

对比：

- `client-turn2.json`（你发的）  
- `outbound-turn2.json`（网关实际发出的）  

diff：后者多了 `reasoning` 块 → HIT。

---

## 3. 手工黑盒剧本（改造完成后照着打）

### 剧本 R1：基础双轮（Responses）

**前提：** `reasoningReplayEnabled: true`，Build 模型，密钥可用。

#### Step 1 — Turn1（要求返回 encrypted reasoning）

```bash
BASE=http://127.0.0.1:8000
KEY=g2a_xxx
MODEL=your-model
CACHE=replay-test-r1

curl -sS "$BASE/v1/responses" \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d "{
    \"model\": \"$MODEL\",
    \"prompt_cache_key\": \"$CACHE\",
    \"stream\": false,
    \"include\": [\"reasoning.encrypted_content\"],
    \"input\": \"用一句话解释什么是互斥锁\"
  }" | tee r1-turn1.json
```

**检查 Turn1 响应：**

```text
output[] 里是否有 type=reasoning 且 encrypted_content 很长
```

没有 → 模型/路径未返回 encrypted，**无法测回放**（先换高推理模型或确认 include 是否被 normalize 保留）。

从响应抽出：

- `ENC` = reasoning.encrypted_content  
- `ASST` = assistant 文本  

日志应出现：`reasoning_replay_store`（若已实现）。

#### Step 2 — Turn2（故意不带 encrypted）

客户端 **只** 发用户续话 + 可选明文历史，**不要**贴 `encrypted_content`：

```bash
curl -sS "$BASE/v1/responses" \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d "{
    \"model\": \"$MODEL\",
    \"prompt_cache_key\": \"$CACHE\",
    \"stream\": false,
    \"include\": [\"reasoning.encrypted_content\"],
    \"input\": [
      {\"role\": \"user\", \"content\": \"用一句话解释什么是互斥锁\"},
      {\"role\": \"assistant\", \"content\": \"$ASST\"},
      {\"role\": \"user\", \"content\": \"那死锁呢？一句话\"}
    ]
  }" | tee r1-turn2.json
```

**HIT 判定（满足任一即可）：**

1. 日志：`reasoning_replay_hit`  
2. outbound dump 的 input 含与 Turn1 相同的 `ENC`  
3. Turn2 **成功 200** 且继续返回新的 reasoning；  
   对比实验：关掉 `reasoningReplayEnabled` 后同样请求若失败或行为明显退化 → 侧面证明依赖回放  

**MISS 判定：**

- 日志 `reasoning_replay_miss`  
- outbound 无 reasoning  
- 或 `prompt_cache_key` Turn1/Turn2 不一致  

### 剧本 R2：Claude Messages 风格（更贴近 Claude Code）

```bash
# Turn1
curl -sS "$BASE/v1/messages" \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -H "X-Claude-Code-Session-Id: replay-claude-001" \
  -d "{
    \"model\": \"$MODEL\",
    \"max_tokens\": 512,
    \"messages\": [{\"role\": \"user\", \"content\": \"1+1等于几，简短回答\"}]
  }" | tee m1.json
```

从 `m1.json` 看 content 是否有 `thinking` + `signature` 或 `redacted_thinking`。

```bash
# Turn2：只回传文本，不回传 thinking/signature
curl -sS "$BASE/v1/messages" \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -H "X-Claude-Code-Session-Id: replay-claude-001" \
  -d "{
    \"model\": \"$MODEL\",
    \"max_tokens\": 512,
    \"messages\": [
      {\"role\": \"user\", \"content\": \"1+1等于几，简短回答\"},
      {\"role\": \"assistant\", \"content\": [{\"type\": \"text\", \"text\": \"2\"}]},
      {\"role\": \"user\", \"content\": \"那 2+2 呢\"}
    ]
  }" | tee m2.json
```

**HIT：** 同一 Session-Id → 日志 hit；outbound Responses body 被注入 reasoning。  
**对照：** 换一个 Session-Id → miss（无法注入上一轮 ENC）。

### 剧本 R3：负向 — 无 session 应不缓存

Turn1/Turn2 **都不** 传 `prompt_cache_key` / Session-Id。

期望：

- 无 `store` / 或 store 因 empty_key 跳过  
- Turn2 必 miss  

### 剧本 R4：租户隔离

| 请求 | Key | Session |
|------|-----|---------|
| A-Turn1 | KEY_A | same-seed |
| B-Turn2 | KEY_B | same-seed（相同原始种子） |

期望：B 不能 hit A 的缓存（identity 含 clientKeyID）。

### 剧本 R5：TTL

1. 把 `reasoningReplayTTL` 临时设为 `10s`  
2. Turn1 store  
3. 等 15s  
4. Turn2 → miss  

### 剧本 R6：开关

```yaml
routing:
  reasoningReplayEnabled: false
```

Turn1 后 Turn2 故意不带 signature → 应 **miss**；打开后同样剧本 → hit。

---

## 4. 自动化测试怎么写（落地时照抄结构）

### 4.1 纯逻辑（无网络）

```text
TestNormalize_KeepsValidGrokEncrypted
TestNormalize_RejectsGarbage
TestFilter_SkipsDuplicateEncrypted
TestFilter_AbortsOnAssistantMismatch
TestInsert_PlacesBeforeToolOutput
```

### 4.2 Memory 仓储

```text
TestReplayStore_SetGetDelete
TestReplayStore_TTLExpire
TestReplayStore_EvictOldest
```

### 4.3 Adapter + Mock 上游（最重要）

```go
// 伪结构
// 1. mockUpstream 记录每次 POST body
// 2. Turn1 响应带 encrypted_content = "SIG_TEST"
// 3. 客户端 Turn2 body 不含 SIG_TEST
// 4. assert mock 第二次 body 的 input 含 SIG_TEST
```

**通过条件一句话：**  
`mock 第二次收到的 input 含客户端没发的 SIG_TEST`。

### 4.4 跑测试命令

```bash
cd backend
go test ./internal/application/gateway/ -run ReasoningReplay -count=1 -v
go test ./internal/infra/runtime/memory/ -run ReasoningReplay -count=1 -v
go test ./internal/infra/provider/cli/ -run Replay -count=1 -v
```

---

## 5. 对照实验表（打印给自己勾）

| # | 操作 | 期望 | 实际 | 通过? |
|---|------|------|------|-------|
| 1 | Turn1 有 encrypted | store 日志 / 响应有 ENC | | |
| 2 | Turn2 同 CACHE、无 ENC | hit 日志 / outbound 有 ENC | | |
| 3 | Turn2 换 CACHE | miss | | |
| 4 | Turn2 换 API Key | miss | | |
| 5 | enabled=false | miss | | |
| 6 | TTL 过期 | miss | | |
| 7 | 官方 usage.cached_tokens | 仅说明 A，不证明 B | | |

---

## 6. 常见误判

| 误判 | 真相 |
|------|------|
| 「答案一样所以缓存了」 | 那是模型行为，不是回放缓存 |
| 「cached_tokens>0 所以回放好了」 | 那是 **官方 Prompt Cache (A)** |
| 「第二轮 200 了所以 hit」 | 可能客户端自己带了 signature |
| 「没有 dump 日志看 usage」 | usage 无法证明 B；必须看出站 body 或 store/hit 日志 |

**正确自检顺序：**

```text
1) Turn1 是否产生 ENC？
2) 服务端是否 store？
3) Turn2 客户端是否故意不带 ENC？
4) 出站是否重新出现同一 ENC？  → 只有 4 才是 HIT
```

---

## 7. 实现时必须加的可观测性（否则你测不了）

落地代码时请带上（写进改造验收）：

1. **结构化日志**（§2.2 手段 1）  
2. **可选** `GROK2API_DUMP_OUTBOUND=1` 写出站 body  
3. **单测 Mock 上游**（§4.3）作为 CI 门禁  
4. （可选）管理端审计字段 `replay_injected: bool` — 非必须  

没有 1 或 3，联调只能靠猜。

---

## 8. 一页速查

```text
测官方前缀缓存(A):  看 usage.cached_tokens / 审计「缓存输入」
测推理回放(B):      看「客户端没带 → 出站却有」encrypted_content
现在能测:           只有 A
改造后重点测:       B 的双轮剧本 R1/R2 + Mock 单测
```

### 最小成功定义（给验收用）

> 固定 `prompt_cache_key`，Turn1 拿到 `encrypted_content=E`；  
> Turn2 请求体不含 `E`；  
> 网关出站 body 的 `input` 含 `E`；  
> 日志 `reasoning_replay_hit`。  

满足以上四条 → **回放缓存工作正常**。
