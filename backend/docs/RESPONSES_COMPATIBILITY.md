# Responses Compatibility

本项目向下游提供 OpenAI 风格接口，向上游连接 Grok Build OAuth inference proxy 与 Grok Web SSO 会话。

## Supported Surface

公共 `/v1` API 开放：

| Endpoint | Status | Notes |
| --- | --- | --- |
| `POST /v1/responses` | Supported | JSON 与 SSE，支持工具调用、结构化输出和 encrypted reasoning 回放 |
| `POST /v1/responses/compact` | Supported | 强制非流式，使用正常模型路由和账号选择 |
| `GET /v1/responses/{response_id}` | Supported | 根据持久化归属回到创建该 Response 的账号，并透传 `include` 等查询参数 |
| `DELETE /v1/responses/{response_id}` | Supported | 上游删除成功后移除本地归属 |
| `GET /v1/models` | Supported | 返回管理端已启用且账号池具备服务能力的公开模型路由 |
| `POST /v1/chat/completions` | Build / Web | xAI Chat Completions JSON 与 SSE；支持图片输入和函数工具，Web Lite 图片模型支持 `image_config` |
| `POST /v1/messages` | Build / Web | Anthropic Messages JSON 与 SSE；支持图片、客户端工具、`tool_use` 和 `tool_result` |
| `POST /v1/images/generations` | Grok Web | Lite Chat 生图与 Imagine WebSocket 生图，支持 `n`、URL 与 Base64 |
| `POST /v1/images/edits` | Grok Web | 官方 JSON `image.url`/`images[].url` 图片编辑 |
| `GET /v1/media/images/{id}` | Public asset | 读取生成后归档到本地媒体存储的不可变图片 |
| `POST /v1/videos/generations` | Grok Web | xAI 官方异步视频生成协议，返回 `request_id` |
| `GET /v1/videos/{request_id}` | Grok Web | xAI 官方 `pending/done/failed` 轮询响应 |

## Conversation Modes

### Stateful

调用方使用 `store` 与 `previous_response_id`。网关持久化 `response_id -> account_id`，后续创建、读取和删除必须回到原账号。绑定账号不可用时返回服务不可用，不会切换账号并制造无效状态链。上游对 retrieve 或 delete 明确返回 `404`/`410` 后，本地归属同步删除，避免失效映射长期滞留。

Grok Web 同时保存本地标准 Response JSON、上游 `conversationId` 和父响应游标。续轮仍绑定原账号；`/responses/compact` 对 Web 模型返回明确的不支持错误。

### Grok Web Chat Images

`grok-chat-fast`、`grok-chat-auto`、`grok-chat-expert`、`grok-chat-heavy` 支持 OpenAI Chat `image_url` 和 Responses `input_image.image_url`。输入可以是 Base64 图片 data URI，也可以是无用户信息的公网 HTTPS URL。网关会校验 DNS 和目标地址、防止访问内网或元数据地址，限制单张大小和总大小，然后在同一账号、同一出口节点、同一 User-Agent 与 Cloudflare 会话中上传到 `/rest/app-chat/upload-file`，将返回的 `fileMetadataId` 写入 `fileAttachments`。下载第三方图片时不会发送 SSO、Cloudflare Cookie 或 Authorization。

单次对话最多接受 `8` 张图片，图片总大小最多 `64 MiB`；单张上限跟随设置页“单张图片上限”。当前未实现 xAI Files API，因此 `input_image.file_id` 会返回明确错误。

`grok-imagine-image` 也可通过 `/v1/chat/completions` 调用。请求正文使用普通 `messages` 作为提示词，并可提供：

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

Grok Web 上游没有公开的 OpenAI function calling 协议。网关对 `grok-chat-*` 提供受控兼容层：接受 Chat Completions 的嵌套 `function` 定义和 Responses 的扁平函数定义，将最多 `128` 个函数及 `tool_choice` 注入结构化 XML 约定，再把模型结果转换为标准 `tool_calls` 或 Responses `function_call` item。支持 `auto`、`none`、`required` 和指定函数；未声明的函数名会被丢弃。

流式请求会在检测到工具 XML 起始标记后暂存该段内容，直到完整解析后才发送结构化工具事件，避免把内部 XML 泄露给客户端。Chat 多轮中的 assistant `tool_calls`、`role: tool`，以及 Responses 的 `function_call`、`function_call_output` 都会重建进下一轮上游上下文。

`web_search`/`web_search_preview` 声明使用 Grok Web 原生搜索，不伪装为客户端函数。上游搜索结果会去重写入响应根部 `search_sources`；正文中的 Grok 引用卡片会转换为 Markdown 引用，并输出标准 URL citation `annotations`。由于函数调用是兼容层而非上游原生 API，生产客户端仍应校验函数名和参数后再执行。

### Grok Build Stateless

调用方使用 `store: false`，并通过 `include: ["reasoning.encrypted_content"]` 获取 encrypted reasoning。后续请求可以回放 `reasoning`、assistant message、function call 与 function output。网关完整保留这些输入项。

## Request Normalization

Grok Build Responses 保持原生转发，只执行以下改写：

- 将公开模型别名替换为上游模型 ID。
- 原样保留官方 `prompt_cache_key`，并用它执行账号粘滞与上游缓存路由。
- 将旧式 `response_format` 映射到 Responses `text.format`；`json_schema` 会展开旧的 `json_schema` 包装层。
- 保留未知字段、encrypted reasoning 和全部标准 Responses input item。

Chat Completions 与 Messages 都先转换为标准 Responses 输入项。Build 将其发送到原生 Responses 上游，并把 JSON/SSE 转回调用方协议；Web 再把同一规范输入转换为 app-chat 对话载荷。两种 Provider 都支持文本、URL/Base64 图片、客户端函数工具和工具结果。不支持的音频、Files API、Anthropic server tools 和 Web `/responses/compact` 会返回明确错误，不会静默丢弃。

Anthropic Messages 使用 `POST /v1/messages`，要求 `anthropic-version`，客户端密钥可通过 `x-api-key` 或 `Authorization: Bearer` 提供。返回使用 Anthropic `message` 对象；流式事件依次为 `message_start`、`content_block_*`、`message_delta` 和 `message_stop`。由于上游不是 Claude，模型 ID 仍使用本平台公开 Grok 模型名称；Anthropic 原生 thinking signature、容器和 server tools 不会伪造。

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

- HTTP 使用 Chrome TLS/HTTP2 指纹；Imagine 使用同一代理、User-Agent 和 Cookie Bundle 的 WebSocket 客户端。
- 每次上游 HTTP 请求生成独立 UUID v4 `x-xai-request-id`，并携带与浏览器同源 fetch 一致的最小稳定请求头；不伪造 Client Hints、Sentry、trace 数据或手工 HTTP/2 头顺序。
- 设置页支持两种 `x-statsig-id` 来源：手动模式直接使用管理员写入的固定值，不自动失效、刷新或替换；URL 模式会先使用同一账号、出口节点、User-Agent 与 Cookie 访问 `https://grok.com/index`，读取 `grok-site-verification` meta，再把请求 method、path 和 metaContent 发送到配置的签名服务。默认签名 URL 是 `https://grok.wodf.de/sign`。URL 签名按 method/path 缓存；Code 7 会立即强制刷新并替换旧值，刷新失败时保留上一个真实签名，绝不发送随机占位签名。
- URL 模式按 `method + path` 在当前实例内共享一份签名，跨账号复用并缓存 1 小时；不同路径不会混用。并发刷新使用 singleflight 合并，缓存最多保存 4096 个路径，过期项会及时清理。签名服务不会收到 SSO、Cloudflare Cookie、提示词或响应正文；签名 URL 必须是无凭据的公网 HTTPS 地址。手动值仅写入，管理接口只返回是否已配置。
- HTTP 403 或流首包 Code 7 会在任何内容写给客户端前立即失效对应路径的缓存，重新获取 meta 并重签一次。首次失效不处罚代理节点；刷新后仍失败才反馈出口健康并返回反机器人错误。流已经开始后绝不重放或拼接第二条响应。
- 手动 Cookie 只保留 `cf_clearance`、`__cf_bm`、`_cfuvid` 和 `cf_chl_*`。
- 不配置、存储或发送 `grok_device_id`、`x-anonuserid`、`x-userid`、`x-challenge`、`x-signature`。
- `/index` 响应中的 `Set-Cookie` 不进入 Cookie Jar，也不会写库或转发；即使上游下发 `x-userid` 也会被丢弃。
- `sso`、`sso-rw` 始终从当前账号的加密 SSO Token 生成。
- Clearance 与出口节点绑定；403 或挑战只重建当前节点会话并降低健康分，下一次优先选择更健康节点，不会冷却或移除账号。
- User-Agent 必须与获取 Clearance 时一致。当前 TLS 客户端使用其最新 Chrome 146 profile；自定义为 Firefox、Safari 等非 Chromium UA 会造成明显指纹不一致。
- 出口节点按 `所有域通用`、`Grok Build`、`Grok Web`、`Grok Web（仅资源）` 四个作用域管理。专用节点优先，Build 与 Web 可回退到通用节点，Web 资源依次回退到 Web、通用节点。Build 使用通用节点时仅复用代理地址，不发送 Web 的 User-Agent 或 Cloudflare Cookie。
- 代理地址支持 HTTP、HTTPS、SOCKS4、SOCKS4A、SOCKS5 与 SOCKS5H，可携带用户名和密码。节点 User-Agent 创建时自动填入当前 Provider 默认值；Cloudflare Cookie 只适用于 Grok Web。
- 配置过对应作用域的出口节点后，如果全部节点不可用，服务不会静默退回直连；只有该作用域完全没有配置节点时才使用 direct。Grok Build 未配置节点时保持原有标准 HTTP 直连。
- 首版仅支持 `none/manual` Clearance，不接 FlareSolverr。

`cost_in_usd_ticks` 是 xAI 公开 Responses 成本字段。Build OAuth 实测响应中 Free 请求返回 `0`，但字段仍按原值透传并进入审计。

`grok-composer-2.5-fast` 按 `grok-build-0.1` 的 256k Context 价格估算：输入 `$1.00 / 1M Tokens`、缓存输入 `$0.20 / 1M Tokens`、输出 `$2.00 / 1M Tokens`，不应用 200k 长上下文加价。

Grok Web Chat 不返回官方 usage，输入、输出与推理 Token 由网关估算并标记为 `usageSource=estimated`。`grok-chat-fast`、`grok-chat-auto`、`grok-chat-expert`、`grok-chat-heavy` 统一使用官方 `grok-4.5` Token 单价计算估算费用；图片、图片编辑和视频不伪造 Token。

`grok-imagine-image-quality` 按客户端请求参数 `n` 计费：1K 为 `$0.05 × n`，2K 为 `$0.07 × n`。上游为了满足请求采用 4/8/12 原生批次时，不按原生批次数量多计费。`grok-imagine-image` 固定为 `$0.02 × n`，不区分 resolution。

`grok-imagine-image-edit` 的输出图片按 1K `$0.05 × n`、2K `$0.07 × n` 计费，并按输入图片数量额外计入 `$0.01 × input_images`。`grok-imagine-video` 按请求时长计费：480p 为 `$0.08 × seconds`，720p 为 `$0.14 × seconds`；没有已配置价格的分辨率保持未计费。媒体审计不写入伪造 Token，只保存价格、模型和价格版本。

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

当前 Grok Build 基线为 `0.2.93`：

- `x-grok-client-version: 0.2.93`
- `x-grok-client-identifier: grok-shell`
- `User-Agent: grok-shell/0.2.93 (linux; x86_64)`

升级 CLI 版本时必须重新运行请求捕获和以下回归：首轮文本、stateless encrypted reasoning 续轮、stateful `previous_response_id`、函数调用、结构化输出、SSE usage、compact、retrieve 与 delete。

## Upstream Boundary

公开 xAI API 文档：

- https://docs.x.ai/build/overview
- https://docs.x.ai/developers/rest-api-reference/inference/chat
- https://docs.x.ai/developers/model-capabilities/text/generate-text

OpenAI Responses 参考：

- https://platform.openai.com/docs/api-reference/responses

`cli-chat-proxy.grok.com` 是 Grok Build 产品上游，不是公开、长期稳定的第三方 API 契约。项目通过版本锁定和协议回归降低变化风险，但不能承诺跨未知未来版本的零变更兼容。
