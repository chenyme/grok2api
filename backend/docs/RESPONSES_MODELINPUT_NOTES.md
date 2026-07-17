# Grok Build Responses / ModelInput 注意事项

> 适用：Codex / OpenAI Responses 客户端 → `grok2api` → Grok Build `POST /v1/responses`  
> 现象：首轮常成功，后续多轮历史回放时 422  
> 错误：
>
> ```text
> unexpected status 422 Unprocessable Entity:
> {"error":"Failed to deserialize the JSON body into the target type:
> data did not match any variant of untagged enum ModelInput"}
> ```

## 1. 结论（先看这个）

Grok Build 0.2.99 的 `input[]` 使用 **Rust 风格 untagged enum `ModelInput`**，对历史 item 形状很严格：

1. **不要原样透传 Codex 历史**
2. **必须白名单重建**每个 `input[]` item / content part
3. **禁止输出 `null` 字段**（尤其是 `reasoning`）
4. **禁止保留 Codex 私有字段**

相关实现主要在：

- `backend/internal/infra/provider/cli/responses_history.go`
- `backend/internal/infra/provider/cli/responses_input.go`
- `backend/internal/infra/provider/cli/responses_codex_tools.go`
- `backend/internal/infra/provider/cli/responses_tool_state.go`
- `backend/internal/infra/provider/cli/responses_tool_declarations.go`
- `backend/internal/infra/provider/cli/responses_tool_choice.go`

## 2. 已确认会 422 的形状

### 2.1 `reasoning` 带 null 字段（高频，会话已复现）

Codex 第二轮经常带回：

```json
{
  "type": "reasoning",
  "id": "rs_xxx",
  "summary": [{"type": "summary_text", "text": "..."}],
  "content": null,
  "encrypted_content": null,
  "internal_chat_message_metadata_passthrough": {"turn_id": "..."}
}
```

错误处理：

```json
{
  "type": "reasoning",
  "id": "rs_xxx",
  "summary": [...],
  "content": null,
  "encrypted_content": null
}
```

正确处理：

- 有可用 `encrypted_content`：只保留非 null 白名单字段回放
- `encrypted_content` 缺失 / null / 空：降级为 developer boundary，保留 summary 文本  
  **绝不要回放带 null 的 reasoning 对象**

复现会话：`019f64fa-984d-73e2-be0e-c732978d3321`  
（首轮 200，第二轮带 null reasoning 后 422）

### 2.2 Codex 私有字段

几乎每条 Codex history 都会带：

- `internal_chat_message_metadata_passthrough`
- `phase`

这些字段不能进入上游 `input[]`。

### 2.3 assistant 文本类型写错

| role | 正确 content part type |
|---|---|
| `assistant` / `model` | `output_text` |
| `user` / `developer` / `system` | `input_text` |

**错误做法：** 把 assistant 的 `output_text` 全改成 `input_text`  
**正确做法：** 按 role 选择类型（对齐 CLIProxyAPI 的 construction 路径）

### 2.4 function 历史字段过多

`function_call` 只允许：

```json
{"type":"function_call","call_id":"...","name":"...","arguments":"..."}
```

`function_call_output` 只允许：

```json
{"type":"function_call_output","call_id":"...","output":"..."}
```

不要带：

- `id`
- `status`
- `namespace`
- `internal_chat_message_metadata_passthrough`
- 其他私有/扩展字段

`output` 必须是 **字符串**；对象要先 `json.Marshal` 成字符串。

### 2.5 未知 / 外部工具历史原样透传

以下类型不能原样发给 Grok Build，应改写成 developer boundary 或兼容 function 协议：

- `web_search_call`
- 原始 `shell_call`（若上游不接受）
- 其他未知 `type`

`GROK2API_STRIP_EXTERNAL_TOOLS=true` 时，还应剥离工具声明：

- `web_search*`
- `computer_use_preview`

保留 function-backed 工具：`function` / `apply_patch` / `tool_search` / `shell|local_shell`。

## 3. 推荐白名单模型

只向 Grok Build 发送这些 `input[]` 形态：

### message

```json
{
  "type": "message",
  "role": "user|assistant|developer",
  "content": [
    {"type": "input_text|output_text", "text": "..."},
    {"type": "input_image", "image_url": "..."},
    {"type": "input_file", "file_data": "...", "filename": "..."}
  ]
}
```

注意：

- 不带 `id` / `status` / `phase` / metadata
- content part 不带 `annotations`
- 有 `role` 但缺 `type` 时，先补 `"type":"message"`

### function_call

```json
{
  "type": "function_call",
  "call_id": "call_xxx",
  "name": "tool_name",
  "arguments": "{}"
}
```

### function_call_output

```json
{
  "type": "function_call_output",
  "call_id": "call_xxx",
  "output": "string only"
}
```

### reasoning

```json
{
  "type": "reasoning",
  "id": "rs_xxx",
  "summary": [{"type":"summary_text","text":"..."}],
  "encrypted_content": "non-empty cipher"
}
```

规则：

- 只保留非 null 字段
- 没有可用 `encrypted_content` 时，不要发 `type=reasoning`，改发 developer boundary

### shell_call_output（如需保留）

- `output` 必须是结构化数组块
- outcome 使用 `exit_code`（不是 `exitCode`）
- 去掉未知块字段（如 `command`）

### 其他未知类型

```json
{
  "type": "message",
  "role": "developer",
  "content": [
    {
      "type": "input_text",
      "text": "A prior Responses history item was omitted ..."
    }
  ]
}
```

## 4. 实现原则

1. **白名单重建，不要 clone 后删字段**  
   clone 容易把 `null` / 私有字段带回去。

2. **JSON 里不要出现 `null` 值**  
   Go 里 `map[string]any{"content": nil}` 会编码成 `"content":null`，对 untagged enum 很危险。

3. **对齐 CLIProxyAPI 的 construction，而不是它的 Responses passthrough**  
   CLIProxyAPI 没有 Grok 专用 adapter；它从 Chat/Claude/Gemini 构造 Codex input 时才会发出干净 item。  
   Responses→Codex 本身几乎是 top-level 清洗，历史深字段不会帮你修。

4. **多轮问题优先查第二轮历史**  
   典型模式：
   - 第 1 轮：只有 user/developer message → 200
   - 第 2 轮：附带 assistant + reasoning(+null) / tool history → 422

5. **修完后要替换正在运行的 Docker 二进制**  
   只改源码不重建容器，Codex 仍打旧逻辑。

## 5. 排查清单

遇到新的 ModelInput 422 时：

1. 从 Docker 日志拿 `request_id` 与时间点
2. 找到对应 Codex session rollout（`~/.codex/sessions/...jsonl`）
3. 统计第二轮 `response_item` 的 `type` 与额外字段
4. 特别检查：
   - `reasoning.content` / `reasoning.encrypted_content` 是否为 null
   - 是否残留 `internal_chat_message_metadata_passthrough` / `phase`
   - assistant content 是否不是 `output_text`
   - `function_call*` 是否多了 `id/status`
5. 用 normalize 单测 / 临时 fixture 验证清洗结果里：
   - 无 `null`
   - 无私有字段
   - item 类型都在白名单
6. `go test ./internal/infra/provider/cli/`
7. 编译 Linux 二进制并替换容器：

```bash
cd backend
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o ../out/grok2api ./cmd/grok2api
docker cp ../out/grok2api grok2api:/app/grok2api
docker restart grok2api
curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:8000/healthz
docker commit grok2api grok2api:local
```

8. **新开 Codex 任务验证**（旧会话可能已污染）

## 6. 相关测试

优先看 / 补充这些回归：

- `TestAssistantOutputMessageHistoryKeepsOutputText`
- `TestNullEncryptedReasoningBecomesBoundary`
- `TestCodexPrivateMetadataIsStrippedFromHistory`
- `TestFunctionCallOutputHistoryEncodesStructuredOutput`
- `TestStripExternalClientToolHistory`
- `TestRoleMissingTypeBecomesMessageAndFunctionCallIsAllowlisted`

## 7. 环境变量

| 变量 | 作用 |
|---|---|
| `GROK2API_STRIP_EXTERNAL_TOOLS=true` | 剥离 Grok Build 难托管的外部工具声明与对应历史，保留 function-backed 工具 |

Docker Compose override 示例见仓库根目录 `docker-compose.override.yml`。

## 8. 参考会话

| Session ID | 现象 |
|---|---|
| `019f5ed1-ebd0-7183-8229-ae0bdaf88c9e` | 多轮 Codex 历史复杂，首次系统性排查 ModelInput 422 |
| `019f64fa-984d-73e2-be0e-c732978d3321` | 首轮 200，第二轮因 null `reasoning.encrypted_content` 422 |

## 9. 一句话记住

> **Codex 历史不能透传；按白名单重建，role 决定 text 类型，reasoning 没有 cipher 就降级，任何 null / 私有字段都不要发给 Grok Build。**
