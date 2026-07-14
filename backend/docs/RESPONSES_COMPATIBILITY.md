# Responses Compatibility

本项目向下游提供 OpenAI 风格接口，向上游连接 Grok Build OAuth inference proxy、Grok Web SSO 会话与 Grok Console SSO Responses。

## Supported Surface

公共 `/v1` API 开放：

| Endpoint | Status | Notes |
| --- | --- | --- |
| `POST /v1/responses` | Build / Web / Console | JSON 与 SSE，支持工具调用、结构化输出和 encrypted reasoning 回放；Console 仅支持无状态请求 |
| `POST /v1/responses/compact` | Build | 强制非流式；Web 与 Console 返回明确的不支持错误 |
| `GET /v1/responses/{response_id}` | Build / Web | 根据持久化归属回到创建该 Response 的账号，并透传 `include` 等查询参数 |
| `DELETE /v1/responses/{response_id}` | Build / Web | 上游删除成功后移除本地归属 |
| `GET /v1/models` | Supported | 返回管理端已启用且账号池具备服务能力的公开模型路由 |
| `POST /v1/chat/completions` | Build / Web / Console | xAI Chat Completions JSON 与 SSE；支持函数工具，Web Lite 图片模型支持 `image_config` |
| `POST /v1/messages` | Build / Web / Console | Anthropic Messages JSON 与 SSE；支持客户端工具、`tool_use` 和 `tool_result` |
| `POST /v1/images/generations` | Grok Web | Lite Chat 生图与 Imagine WebSocket 生图，支持 `n`、URL 与 Base64 |
| `POST /v1/images/edits` | Grok Web | 官方 JSON `image.url`/`images[].url` 图片编辑 |
| `GET /v1/media/images/{id}` | Public asset | 读取生成后归档到本地媒体存储的不可变图片 |
| `POST /v1/videos/generations` | Grok Web | xAI 官方异步视频生成协议，返回 `request_id` |
| `GET /v1/videos/{request_id}` | Grok Web | xAI 官方 `pending/done/failed` 轮询响应 |

## Model IDs

下游公开模型名称不带 Provider 前缀，例如 `grok-4.5`。内部路由目标使用 `Build/<model>`、`Web/<model>` 或 `Console/<model>` 区分来源；同一公开名称存在多个可用来源时，网关会结合客户端密钥权限、协议能力和账号可用性选择渠道。显式使用带前缀名称仍可定向指定来源，选定后请求重试只发生在该 Provider 账号池内。

数据库升级会原位迁移内部路由，保留路由主键、客户端密钥授权，并将旧名称登记为兼容别名。新创建和同步的路由在库内使用带来源前缀的稳定 ID；`GET /v1/models` 会按不带前缀的公开名称去重。

## Conversation Modes

### Stateful

Build/Web 调用方可使用 `store` 与 `previous_response_id`。网关持久化 `response_id -> account_id`，后续创建、读取和删除必须回到原账号。绑定账号不可用时返回服务不可用，不会切换账号并制造无效状态链。上游对 retrieve 或 delete 明确返回 `404`/`410` 后，本地归属同步删除，避免失效映射长期滞留。

Grok Web 同时保存本地标准 Response JSON、上游 `conversationId` 和父响应游标。续轮仍绑定原账号；`/responses/compact` 对 Web 模型返回明确的不支持错误。

### Grok Web Chat Images

Grok Web 来源的 `grok-chat-fast`、`grok-chat-auto`、`grok-chat-expert`、`grok-chat-heavy` 支持 OpenAI Chat `image_url` 和 Responses `input_image.image_url`。输入可以是 Base64 图片 data URI，也可以是无用户信息的公网 HTTPS URL。网关会校验 DNS 和目标地址、防止访问内网或元数据地址，限制单张大小和总大小，然后在同一账号、同一出口节点、同一 User-Agent 与 Cloudflare 会话中上传到 `/rest/app-chat/upload-file`，将返回的 `fileMetadataId` 写入 `fileAttachments`。下载第三方图片时不会发送 SSO、Cloudflare Cookie 或 Authorization。

单次对话最多接受 `8` 张图片，图片总大小最多 `64 MiB`；单张上限跟随设置页“单张图片上限”。当前未实现 xAI Files API，因此 `input_image.file_id` 会返回明确错误。

Grok Web 来源的 `grok-imagine-image` 也可通过 `/v1/chat/completions` 调用。请求正文使用普通 `messages` 作为提示词，并可提供：

```json
{
  "image_config": {
    "n": 2,
    "response_format": "url"
  }
}
```

Lite 上游每次 Fast Chat 查询固定生成两张候选，但旧协议只采用首张。因此 `n` 表示独立执行 `n` 次查询，范围为 `1..10`，每次按真实 Fast 额度扣减；流式 Chat 会在每张图片完成归档后输出一个 Markdown 图片增量。

### Grok Web Function Tools

Grok Web 上游没有公开的 OpenAI function calling 协议。网关对 Web 来源的 `grok-chat-*` 提供受控兼容层：接受 Chat Completions 的嵌套 `function` 定义和 Responses 的扁平函数定义，将最多 `128` 个函数及 `tool_choice` 注入结构化 XML 约定，再把模型结果转换为标准 `tool_calls` 或 Responses `function_call` item。支持 `auto`、`none`、`required` 和指定函数；未声明的函数名会被丢弃。

流式请求会在检测到工具 XML 起始标记后暂存该段内容，直到完整解析后才发送结构化工具事件，避免把内部 XML 泄露给客户端。Chat 多轮中的 assistant `tool_calls`、`role: tool`，以及 Responses 的 `function_call`、`function_call_output` 都会重建进下一轮上游上下文。

`web_search`/`web_search_preview` 声明使用 Grok Web 原生搜索，不伪装为客户端函数。上游搜索结果会去重写入响应根部 `search_sources`；正文中的 Grok 引用卡片会转换为 Markdown 引用，并输出标准 URL citation `annotations`。由于函数调用是兼容层而非上游原生 API，生产客户端仍应校验函数名和参数后再执行。

### Grok Build Stateless

调用方使用 `store: false`，并通过 `include: ["reasoning.encrypted_content"]` 获取 encrypted reasoning。后续请求可以回放 `reasoning`、assistant message、function call 与 function output。网关完整保留这些输入项。

### Grok Console Stateless

Console 请求固定使用 `store: false`。显式提交 `store: true` 或非空 `previous_response_id` 会在请求上游前返回 `400`，避免客户端误以为状态已被保存。Console 响应不会写入本地 response ownership，因此 retrieve/delete 返回 `404`，旧版本遗留的 Console ownership 会在首次访问时清理。

Chat Completions 与 Messages 同样采用无状态转换；客户端必须在下一轮回放所需消息、工具调用和工具结果。Console 上游的纯文本或 JSON 错误会按调用协议标准化，同时保留原始 HTTP 状态与 `Retry-After`，以便网关正确执行额度恢复和账号切换。

## Request Normalization

Grok Build Responses 默认保持原生转发，只执行以下改写：

- 将公开模型别名替换为上游模型 ID。
- 原样保留官方 `prompt_cache_key`，并用它执行账号粘滞与上游缓存路由。
- 将旧式 `response_format` 映射到 Responses `text.format`；`json_schema` 会展开旧的 `json_schema` 包装层。
- 保留未知字段、encrypted reasoning，以及除下述明确受限的新工具项外的标准 Responses input item。

Grok Build `0.2.99` 尚未原生接受 OpenAI 新版 `namespace` 与 `tool_search` 工具类型，因此网关只对这部分执行请求级双向兼容：

- `namespace` 中可立即调用的函数会转换为不超过 `128` 字符的唯一普通函数别名；JSON 与 SSE 中的函数调用会恢复原始 `name` 和 `namespace`，响应根部的 `tools` 也恢复为客户端声明。
- `execution: "client"` 的 `tool_search` 会转换为内部普通函数。带 `defer_loading: true` 的函数在首次请求中不暴露；客户端回传 `tool_search_output` 后，选中的函数才会进入下一轮上游工具集合。
- 客户端 Tool Search 会默认写入 `parallel_tool_calls: false`；显式要求并行会返回错误，避免搜索函数与普通函数在工具定义加载完成前同时执行。
- 单独声明 `defer_loading: true` 但没有客户端 `tool_search` 时返回参数错误，避免把原本延迟的工具静默改成立即可调用。
- 服务端托管 Tool Search 需要上游在同一次推理中搜索并注入工具，当前无法等价模拟，因此缺省或 `execution: "server"` 会返回 `400 unsupported_parameter`，不会静默展开全部延迟工具。
- `additional_tools` 中的定义会进入上游顶层工具集合，同时在原输入位置保留 developer 边界消息，提示这些工具从该位置开始可用。由于 0.2.99 没有位置化工具定义，响应会携带 `additional_tools_position_approximated` 兼容警告。

`0.2.99` OAuth 上游实测原生工具 discriminator 为 `function`、`web_search`、`x_search`、`image_generation`、`collections_search`、`file_search`、`code_execution`、`code_interpreter`、`mcp` 和 `shell`。其中 `collections_search` 需要 `vector_store_ids`，`shell` 需要 `environment`；网关保留这些原生工具参数交由上游校验。旧 Codex `local_shell` 会升级为 `shell + environment.type=local`，JSON/SSE 中的 `shell_call` 恢复为 `local_shell_call`，续轮输出升级为结构化 `shell_call_output`；同一请求不能混用新旧 shell 声明。

`apply_patch` 不在 0.2.99 上游枚举中，网关将其包装为具有严格 operation schema 的内部 function，并在 JSON、SSE 和续轮历史中双向恢复 `apply_patch_call`/`apply_patch_call_output`。流式 function 参数事件不会泄露给 Codex；网关等待 operation 完整后再发出标准 output item added/done。`computer_use_preview` 涉及截图与动作循环，无法安全等价模拟，继续返回明确的不支持错误。

`web_search_preview` 及其版本别名会转换为最小 `web_search` 声明；当前上游会以 `Argument not supported` 拒绝 Codex/OpenAI 新版的 `external_web_access`、`indexed_web_access`、`search_content_types`、`search_context_size`、`user_location` 和 `filters` 等控制字段，因此兼容层会将可安全放宽的已知字段降级为 0.2.99 的最小搜索声明。遇到 `external_web_access: false` 时会移除整个 Web Search 工具，并将显式选择该工具的 `tool_choice` 收敛为 `none`，确保不会把“禁止外网”静默扩大为可联网搜索；这会禁用包括索引搜索在内的全部 Web Search，并通过兼容警告公开该降级。非空 `filters` 或 `allowed_domains` 等无法用更严格子集等价执行的范围约束仍会明确拒绝；其他未知字段同样报错。

OpenAI `custom` 工具会转换为只接受 `input` 字符串的普通函数，并在 JSON/SSE 响应及续轮历史中恢复为 `custom_tool_call`、`custom_tool_call_output` 和 `response.custom_tool_call_input.*` 事件。纯文本 format 可兼容；grammar 无法等价表达，会返回 `unsupported_parameter`。

Codex 的可见 `agent_message` 与 `mcp_tool_call_output` 会保留为 developer 文本；`local_shell_call/output` 与 `apply_patch_call/output` 保持结构化续轮协议。不透明或加密的 agent 内容会替换为不含密文的边界消息，`compaction_trigger` 会替换为压缩边界消息，避免静默丢弃或让整轮失败。SSE 兼容层只向下游保留标准 `response.*`、`error` 与 `[DONE]`，过滤上游私有事件，同时保留标准 comment、`id` 和 `retry` 字段。

发生协议降级时，响应 Header `X-Grok2API-Compatibility-Warnings` 会返回稳定的逗号分隔代码，例如 `web_search_controls_downgraded`、`web_search_disabled_no_external_access`、`web_search_tool_choice_disabled`、`legacy_local_shell_upgraded`、`apply_patch_emulated`、`additional_tools_position_approximated`。客户端可记录这些代码进行审计，不包含提示词、工具参数或凭据。

Chat Completions 与 Messages 都先转换为标准 Responses 输入项。Build 与 Console 将其发送到各自的 Responses 上游，并把 JSON/SSE 转回调用方协议；Web 再把同一规范输入转换为 app-chat 对话载荷。不支持的音频、Files API、Anthropic server tools，以及 Web/Console `/responses/compact` 会返回明确错误，不会静默丢弃。

Anthropic Messages 使用 `POST /v1/messages`，要求 `anthropic-version`，客户端密钥可通过 `x-api-key` 或 `Authorization: Bearer` 提供。返回使用 Anthropic `message` 对象；流式事件依次为 `message_start`、`content_block_*`、`message_delta` 和 `message_stop`。由于上游不是 Claude，模型 ID 仍使用本平台公开 Grok 模型名称。

Messages 转换层兼容 Claude Code 常见载荷：标准顶层 `system` 与误放在 `messages` 数组中的 `system`/`developer` 都会按原顺序合并为 Responses `instructions`，不会再因 `role: "system"` 返回 400。请求固定使用 `store: false`；`metadata.user_id` 映射为 `safety_identifier`。`thinking.enabled`/`thinking.adaptive` 会依据 `budget_tokens` 和 effort 映射到 Grok reasoning，并请求 `reasoning.encrypted_content`；JSON 与 SSE 会恢复 `thinking_delta`、`signature_delta`、`thinking` 或 `redacted_thinking`，便于下一轮原样回放，不伪造签名。

客户端工具支持 `strict`、`tool_choice.disable_parallel_tool_use`、严格的 `tool_use`/`tool_result` 配对和重复 ID 校验；带 `is_error` 的结果会保留失败语义。`tool_result` 可包含文本、图片、文档和 Anthropic `tool_reference`；引用只允许指向本次请求已声明的工具，并转换为确定性的搜索命中结果，因为对应工具定义已随请求提供给 Build。Messages 普通输入也支持 URL/Base64 图片、文本 document，以及 Build 上游可处理的 URL/Base64 `input_file`。Build 可把 `mcp_servers` 转换为原生 MCP 工具；Grok Web 对无法等价执行的 MCP 或文件内容返回明确错误。`stop_sequences` 在 JSON/SSE 下由网关跨内容块、跨增量执行并返回真实 `stop_sequence`；无法等价表示的 `top_k` 会在请求上游前明确拒绝。

图片编辑严格使用 xAI 当前 JSON 结构：`image.url` 或 `images[].url` 可使用公网 HTTPS URL 或 Base64 data URI，数量参数使用 `n`，`resolution` 支持 `1k`、`2k` 且默认 `1k`。不接受 multipart 或 `image_count` 等非官方兼容字段。当前未实现 Files API，因此 `image.file_id` 会返回明确的不支持错误。

## Usage

审计记录保存：

- `input_tokens`
- `input_tokens_details.cached_tokens`
- `output_tokens`
- `output_tokens_details.reasoning_tokens`
- `total_tokens`
- `cost_in_usd_ticks`
- `num_sources_used`
- `num_server_side_tools_used`
- `context_details.input_tokens/output_tokens`
- `media_input_images`
- `media_output_images`
- `media_output_seconds`

不保存 prompt、response body、encrypted reasoning 内容或工具参数。

Grok Web 未提供与公开 API 等价的精确 Token 计量，因此聊天审计标记 `usageSource: estimated`；图片和视频标记 `usageSource: none`，不会伪造 Token，用量费用仅按已配置的官方媒体单价估算。

Grok Web 付费账号使用 `GrokBuildBilling/GetGrokCreditsConfig` 返回的统一周额度池：保存真实使用百分比、产品枚举分解、周期起止和重置时间，并作为 Chat、Imagine、图片编辑和视频共享的路由总闸门。成功调用后异步刷新周池，不按未知权重进行本地伪扣减；耗尽后按真实周重置时间进入单次恢复队列。

Free 账号先探测 gRPC 周池；没有有效周池时固定使用 `/rest/rate-limits` 的 `fast` 窗口及其真实重置时间。明确导入为 Super/Heavy 的账号只请求 gRPC 周池；`auto` 账号发现有效周池后先归为 Super，Heavy 需由导入等级明确指定，直到获得可验证的官方等级字段。全量同步会替换旧额度快照，避免升降级后残留窗口误导路由和 UI。

Free 的 429 会耗尽 `fast` 并按模式重置时间恢复。付费周池的 429 不直接置零：网关立即重新请求 gRPC，只有上游确认使用率达到 100% 才进入待重置队列；若周池尚未耗尽或同步失败，则按普通短期限流冷却并尝试其他账号。产品分解的 protobuf 枚举在获得官方 schema 前按数字原样保存，不猜测映射名称。

周池产品枚举为：`0 = Third Party`、`1 = API`、`2 = Grok Build`、`3 = Grok Plugins`、`4 = Chat`、`5 = Imagine`、`6 = Voice`。其中样本响应的 `Imagine 10% + Chat 1% = 总使用 11%` 已与 Grok Usage 页面交叉验证。管理端按真实百分比分段绘制进度条，零使用产品不显示；未来出现的未知枚举仍保留原始编号并使用通用标签。

### Image Generation

图片生成遵循 xAI REST API 的 `POST /v1/images/generations`：`n` 范围为 `1..10`，`aspect_ratio` 支持官方 Grok Imagine 枚举，`resolution` 支持 `1k` 与 `2k`。内部 WebSocket 将 `1k` 映射为 Speed、`2k` 映射为 Quality，并按 `n` 选择最小原生批次：`1..4 -> 4`、`5..8 -> 8`、`9..10 -> 12`，最终只返回请求的 `n` 张。

`size` 继续作为 OpenAI 客户端兼容别名；同时提供 `aspect_ratio` 时以后者为准。当前项目没有实现 xAI Files API，因此 `storage_options` 会返回明确的不支持错误，不会静默忽略。

所有生成图片都会在账号和出口租约释放前统一归档。WebSocket `blob` 会解码为二进制；只有上游 URL 时会使用原账号的资产出口下载。文件写入 `media.local.path`，数据库仅保存资源元数据。`response_format: url` 返回后端 `/v1/media/images/{id}`，`b64_json` 从同一份已归档字节编码，二者不会分别下载或保存两份。资源 ID 使用不可猜测随机值，读取端点支持 `GET`、`HEAD`、ETag 和不可变缓存头。

本地媒体默认容量上限为 `1 GiB`，自动清理阈值为 `80%`。占用超过阈值后按创建时间从旧到新删除，直到回落到阈值以内；保存图片会触发容量检查，后台每 `10m` 兜底执行。Memory/Redis 运行态均通过清理锁避免同一共享目录被多个实例同时清理。单图上限、容量、自动清理阈值和检查间隔由设置页管理并立即热加载；YAML 只保留存储驱动和本地目录，修改这两项需要重启。

`stream: true` 是 grok2api 扩展，不属于 xAI 官方 Images REST 字段。启用后响应为 SSE：先发送 `image_generation.started`，每张图片完成后发送 `image_generation.image.completed`，最后发送 `image_generation.completed` 与 `[DONE]`。首个事件写出后账号、出口节点和 WebSocket 均保持固定，不会跨账号拼接。Lite 模型在 `/v1/images/generations` 不支持该扩展，但可通过 `/v1/chat/completions` 流式返回图片。

### Video Generation

视频公共接口严格采用 xAI 官方异步协议。`POST /v1/videos/generations` 接受 `model`、`prompt`、`user`、`duration`、`aspect_ratio`、`resolution`、`image` 和 `reference_images`，成功仅返回 `request_id`。`duration` 按官方规则接受整数或整数字符串；不接受 `seconds`、`image_url`、`size`、`quality`、`input_reference` 等非原生字段。当前支持公网 HTTPS 或 Base64 data URI 图片；尚未实现 Files API，因此 `file_id` 会返回明确的不支持错误。`output.upload_url` 与 `storage_options` 同样不会被静默忽略。

`GET /v1/videos/{request_id}` 将内部任务状态映射为官方 `pending`、`done`、`failed`。完成响应在 `video` 中返回 `url`、`duration` 与 `respect_moderation`；失败响应只返回官方错误枚举和消息，不暴露账号、上游 Post ID、租约或数据库字段。当前不开放 `/videos/edits`、`/videos/extensions` 或独立内容代理端点。

## Grok Web SSO Import

标准导入格式：

```json
{
  "provider": "grok_web",
  "accounts": [
    { "name": "Web Account 01", "sso_token": "...", "tier": "auto" }
  ]
}
```

JSON 只接受当前 `accounts` 结构。SSO 不存在自动刷新：上游返回 401 后账号会标记为 `reauthRequired` 并退出号池。

也支持纯文本快速导入，每个非空行视为一个 SSO Token；可直接填写 Token 或 `sso=...` Cookie 形式。重复 Token 自动忽略，导入后仍会等待该批账号的首次额度与模型同步完成。

## Cloudflare And Egress

- Grok Build 使用标准 Go HTTP/TLS 传输并保持 CLI User-Agent；通过代理时仍沿用该传输，不切换成浏览器指纹。Grok Web HTTP 使用 Chrome TLS/HTTP2 指纹；Imagine 使用同一代理、User-Agent 和 Cookie Bundle 的 WebSocket 客户端。
- 每次上游 HTTP 请求生成独立 UUID v4 `x-xai-request-id`，并携带与浏览器同源 fetch 一致的最小稳定请求头；不伪造 Client Hints、Sentry、trace 数据或手工 HTTP/2 头顺序。
- 设置页支持两种 `x-statsig-id` 来源：手动模式直接使用管理员写入的固定值，不自动失效、刷新或替换；URL 模式会先使用同一账号、出口节点、User-Agent 与 Cookie 访问 `https://grok.com/index`，读取 `grok-site-verification` meta，再把请求 method、path 和 metaContent 发送到配置的签名服务。默认签名 URL 是 `https://grok.wodf.de/sign`。URL 签名按 method/path 缓存；Code 7 会立即强制刷新并替换旧值，刷新失败时保留上一个真实签名，绝不发送随机占位签名。
- URL 模式按 `method + path` 在当前实例内共享一份签名，跨账号复用并缓存 1 小时；不同路径不会混用。并发刷新使用 singleflight 合并，缓存最多保存 4096 个路径，过期项会及时清理。签名服务不会收到 SSO、Cloudflare Cookie、提示词或响应正文。公网签名服务必须使用无凭据、查询参数或片段的 HTTPS:443 地址；Docker 单标签服务名、localhost、`.local/.internal` 和私有地址可使用 HTTP/HTTPS 与自定义端口，例如 `http://grok-signer-go:8788/sign`。签名请求不跟随重定向。手动值仅写入，管理接口只返回是否已配置。
- HTTP 403 或流首包 Code 7 会在任何内容写给客户端前立即失效对应路径的缓存，重新获取 meta 并重签一次。首次失效不处罚代理节点；刷新后仍失败才反馈出口健康并返回反机器人错误。流已经开始后绝不重放或拼接第二条响应。
- 手动 Cookie 只保留 `cf_clearance`、`__cf_bm`、`_cfuvid` 和 `cf_chl_*`。
- 不配置、存储或发送 `grok_device_id`、`x-anonuserid`、`x-userid`、`x-challenge`、`x-signature`。
- `/index` 响应中的 `Set-Cookie` 不进入 Cookie Jar，也不会写库或转发；即使上游下发 `x-userid` 也会被丢弃。
- `sso`、`sso-rw` 始终从当前账号的加密 SSO Token 生成。
- Clearance 与出口节点绑定；403 或挑战只重建当前节点会话并降低健康分，下一次优先选择更健康节点，不会冷却或移除账号。
- User-Agent 必须与获取 Clearance 时一致。当前 TLS 客户端使用其最新 Chrome 146 profile；自定义为 Firefox、Safari 等非 Chromium UA 会造成明显指纹不一致。
- 出口节点按 `Grok Build`、`Grok Web`、`Grok Web（仅资源）` 三个作用域独立管理代理、健康度和冷却状态。Grok Build 始终沿用 Provider 的 CLI User-Agent，节点不能覆盖；Grok Web 和 Web 资源节点独立管理浏览器 User-Agent 与 Cloudflare Cookie。Web 资源未配置专用节点时仅回退到 Grok Web 节点。
- 代理地址支持 HTTP、HTTPS、SOCKS4、SOCKS4A、SOCKS5 与 SOCKS5H，可携带用户名和密码。Cloudflare Cookie 只适用于 Grok Web。
- 配置过对应作用域的出口节点后，如果全部节点不可用，服务不会静默退回直连；只有该作用域完全没有配置节点时才使用 direct。Grok Build 未配置节点时保持原有标准 HTTP 直连。
- 首版仅支持 `none/manual` Clearance，不接 FlareSolverr。

`cost_in_usd_ticks` 是 xAI 公开 Responses 成本字段。Build OAuth 实测响应中 Free 请求返回 `0`，但字段仍按原值透传并进入审计。

`grok-code-fast*` 与 `grok-composer-2.5-fast` 按 `grok-build-0.1` 价格估算：标准输入 `$1.00 / 1M Tokens`、缓存输入 `$0.20`、输出 `$2.00`；Context 超过 200k 后分别为 `$2.00`、`$0.40`、`$4.00`。

文本计费模型会先移除系统已知的 `Build/`、`Web/`、`Console/` 来源前缀，再匹配官方精确名称和锚定的模型家族规则。该规则兼容 `latest`、日期、Build Free、reasoning、non-reasoning、multi-agent 等版本后缀，但不会用子串包含方式为未知模型套用价格。

Grok Web Chat 不返回官方 usage，输入、输出与推理 Token 由网关估算并标记为 `usageSource=estimated`。Web 来源的 `grok-chat-fast`、`grok-chat-auto`、`grok-chat-expert`、`grok-chat-heavy` 统一使用官方 `grok-4.5` Token 单价计算估算费用；图片、图片编辑和视频不伪造 Token。

Web 来源的 `grok-imagine-image-quality` 按客户端请求参数 `n` 计费：1K 为 `$0.05 × n`，2K 为 `$0.07 × n`。上游为了满足请求采用 4/8/12 原生批次时，不按原生批次数量多计费。`grok-imagine-image` 固定为 `$0.02 × n`，不区分 resolution。

Web 来源的 `grok-imagine-image-edit` 输出图片按 1K `$0.05 × n`、2K `$0.07 × n` 计费，并按输入图片数量额外计入 `$0.01 × input_images`。`grok-imagine-video` 按请求时长计费：480p 为 `$0.08 × seconds`，720p 为 `$0.14 × seconds`；没有已配置价格的分辨率保持未计费。媒体审计不写入伪造 Token，只保存价格、模型和价格版本。

## Response Headers

网关保留 `Content-Type`、请求追踪和上游限流等端到端响应头。HTTP hop-by-hop headers、`Connection` 动态声明的逐跳头、`Content-Length` 以及上游 `Set-Cookie` 不会转发给下游，避免连接级状态和上游会话凭据穿透代理边界。

## Transfer Limits

请求体默认最大 `32 MiB`。非流式响应以 `128 MiB`、流式响应以 `256 MiB` 作为单请求传输安全上限，并使用固定小缓冲直接转发。JSON usage 与单条 SSE 事件的内存检查上限为 `8 MiB`；超出时正文仍继续转发，但该次请求可能无法提取 usage、模型或 Response ID。

## Build Billing Observation

Grok Build `0.2.93` 实测使用 `GET /billing` 与 `GET /billing?format=credits`。网关保存真实出现的 `monthlyLimit`、`used`、`onDemandCap`、`onDemandUsed`、`prepaidBalance`、`isUnifiedBillingUser`、`topUpMethod`、`currentPeriod.type` 和历史账期。

这些端点没有公开 xAI 字段契约，因此实现遵守以下限制：

- Billing 全零本身不证明 Free，`isUnifiedBillingUser: true` 也不证明付费；但完整匹配已捕获的 weekly/unified/top-up Free Profile 时可标记为 `estimated`。
- 估算态使用约 1M 作为管理端参考值，并通过 `confidence: estimated` 与 `limitKnown: false` 明确标识，不参与上游确认语义。
- 真实响应模型以 `-build-free` 结尾时标记为 `observed`；上游返回 Free 耗尽 `actual/limit` 后标记为 `confirmed` 并覆盖估算值。
- 升级 Grok Build 版本时使用脱敏 fixture 重新验证 Billing 和 Responses 字段。

## Model Capabilities

模型发现以账号为粒度执行。每个账号保存最后一次成功返回的模型集合，公开模型目录使用所有账号能力的并集。请求路由只使用已确认支持目标上游模型的账号；未完成首次能力同步的账号可以作为兼容回退，已确认不支持的账号不会参与该模型的负载均衡。

会员等级只使用上游 Billing 明确返回的 plan/tier 元数据展示，不通过额度大小反推套餐名称。路由决策以实际模型能力快照为准，因此即使两个付费账号的套餐名称不同，只要模型集合不同，也会被正确拆分处理。

同步失败不会删除最后一次成功快照。只有当全部活跃账号均已完成同步且都不支持某个模型时，该模型才会从 `GET /v1/models` 和新请求路由中移除。既有 `previous_response_id` 状态链仍遵守原账号归属，不会为切换会员等级而跨账号迁移。

## Version Contract

当前 Grok Build 基线为 `0.2.99`：

- `x-grok-client-version: 0.2.99`
- `x-grok-client-identifier: grok-shell`
- `User-Agent: grok-shell/0.2.99 (linux; x86_64)`

升级 CLI 版本时必须重新运行请求捕获和以下回归：首轮文本、stateless encrypted reasoning 续轮、stateful `previous_response_id`、函数调用、namespace、客户端 Tool Search、结构化输出、SSE usage、compact、retrieve 与 delete。

## Upstream Boundary

公开 xAI API 文档：

- https://docs.x.ai/build/overview
- https://docs.x.ai/developers/rest-api-reference/inference/chat
- https://docs.x.ai/developers/model-capabilities/text/generate-text

OpenAI Responses 参考：

- https://platform.openai.com/docs/api-reference/responses
- https://developers.openai.com/api/docs/guides/tools-shell
- https://developers.openai.com/api/docs/guides/tools-local-shell
- https://developers.openai.com/api/docs/guides/tools-apply-patch

`cli-chat-proxy.grok.com` 是 Grok Build 产品上游，不是公开、长期稳定的第三方 API 契约。项目通过版本锁定和协议回归降低变化风险，但不能承诺跨未知未来版本的零变更兼容。
