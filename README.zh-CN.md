<p align="center">
  <img alt="Grok2API" src="./frontend/public/grok2api.png" width="720" />
</p>

<p align="center">
  <strong>面向 Grok Build、Grok Web 与 Grok Console 的多账号 API 网关</strong>
</p>

<p align="center">
  <a href="./README.md">English</a> | 简体中文
</p>

<p align="center">
  <a href="./backend/go.mod"><img alt="Go" src="https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white" /></a>
  <a href="./frontend/package.json"><img alt="React" src="https://img.shields.io/badge/React-19-61DAFB?logo=react&logoColor=111827" /></a>
  <a href="https://github.com/chenyme/grok2api/pkgs/container/grok2api"><img alt="Docker" src="https://img.shields.io/badge/Docker-amd64%20%7C%20arm64-2496ED?logo=docker&logoColor=white" /></a>
</p>

<p align="center">
  <a href="https://trendshift.io/repositories/19868?utm_source=repository-badge&amp;utm_medium=badge&amp;utm_campaign=badge-repository-19868" target="_blank" rel="noopener noreferrer"><img src="https://trendshift.io/api/badge/repositories/19868" alt="chenyme%2Fgrok2api | Trendshift" width="250" height="55"/></a>
</p>

> [!TIP]
> 推荐个人新项目 [DEEIX-AI / DEEIX-Chat](https://github.com/DEEIX-AI/DEEIX-Chat)：面向多模型路由、对话、文件、工具、计费与运维的一体化轻量 AI 平台。

> [!NOTE]
> 本项目仅供技术研究与学习交流。使用时请务必遵循 Grok 官方的使用条款及当地法律法规，否则一切后果自负！

## 赞助商

> [希望赞助这个项目？](mailto:chenyme03@gmail.com)

<table>
<tr>
<td width="200" align="center" valign="middle"><a href="https://www.krill-ai.com/register?invite=KJ2VGIRVAE"><img src="https://raw.githubusercontent.com/Krill-ai-org/krill-ai-static/refs/heads/main/krill-logo/Eng/250x150.png" alt="Krill AI" width="160"></a></td>
<td valign="middle">感谢 Krill AI 赞助了本项目！Krill 提供 GPT / Claude / Gemini / 多款国产模型的官方稳定极速的 API 中转服务，支持企业级定制、报销开票、7×16h 专属技术支持。更有独家适配的 WebSocket 连接，畅享极速首字速度。Krill 为本项目提供了特别优惠，使用<a href="https://www.krill-ai.com/register?invite=KJ2VGIRVAE">此链接</a>注册并在下订单时填写「grok2api」优惠码，首购套餐可享 Codex 77 折优惠！</td>
</tr>
<tr>
<td width="200" align="center" valign="middle"><a href="https://github.com/DEEIX-AI/DEEIX-Chat"><img src="frontend/public/sponner/deeix-chat_deeix-ai.png" alt="DEEIX AI / DEEIX Chat" width="160"></a></td>
<td valign="middle">DEEIX-Chat 是一款开源可部署的 AI Chat 平台，面向需要长期、稳定、统一使用多模型能力的个人、团队与企业，将模型、对话、文件、工具调用与后台管理整合为一套可部署、可扩展的系统。点击 <a href="https://github.com/DEEIX-AI/DEEIX-Chat">此处</a> 开始部署！</td>
</tr>
<tr>
<td width="200" align="center" valign="middle"><a href="https://www.right.codes/register"><img src="frontend/public/sponner/rightcode.jpg" alt="RightCode" width="160"></a></td>
<td valign="middle">Right Code 是一个企业级 AI Agent 分发平台，主要提供稳定的 Claude Code、Codex、Gemini 等模型的中转服务。充值即可开票，企业、团队用户一对一对接。感谢 Right Code 提供的 Tokens 支持，点击 <a href="https://www.right.codes/register">此处</a> 注册并开始使用！</td>
</tr>
<tr>
<td width="200" align="center" valign="middle"><a href="https://api.fenno.ai/s/xCBS"><img src="frontend/public/sponner/fenno-ai.jpg" alt="FennoAI" width="160"></a></td>
<td valign="middle">FennoAI 面向企业研发团队和开发者提供企业级的高稳定、高性能 API 中转服务，兼容 OpenAI 与 Anthropic 协议，可接入 Codex、Claude Code、OpenCode 等主流 AI 编程工具。平台具备企业级稳定性，可支撑千亿 Token/日调用，以及境内外主体公对公结算与开票。Grok2API 用户通过<a href="https://api.fenno.ai/s/xCBS">专属链接</a>购买订阅，仅需 1.99 美元即可获得价值 50 美元的 Coding Plan 额度，邀请好友购买最高可获 20% 返佣。</td>
</tr>
<tr>
<td width="200" align="center" valign="middle"><a href="https://s.qiniu.com/RNNZFf"><img src="frontend/public/sponner/qiniu.jpg" alt="七牛云 AI" width="160"></a></td>
<td valign="middle">七牛云 AI 是七牛云（02567.HK）旗下企业级大模型 MaaS 平台，可一站式调用全球 150+ 主流模型，兼容主流模型厂商协议，覆盖文本、图像、音频、视频和文件处理等全模态能力，已服务超过 169 万企业及开发者用户。Grok2API 用户通过<a href="https://s.qiniu.com/RNNZFf">专属链接</a>注册，企业用户可免费领取 1200 万 Token，开发者可免费领取 300 万 Token。</td>
</tr>
</table>

<br>

## 项目简介

Grok2API 是一个内置 React 管理端的 Go 网关。它分别管理 Grok Build、Grok Web 和 Grok Console 账号池，并对外提供统一的 OpenAI 与 Anthropic 兼容接口。

### 项目架构

```mermaid
flowchart LR
    %% 颜色定义
    classDef access fill:#e1f5fe,stroke:#01579b
    classDef core fill:#fff3e0,stroke:#e65100
    classDef providers fill:#f3e5f5,stroke:#4a148c
    classDef infra fill:#e8f5e9,stroke:#1b5e20
    classDef upstream fill:#fce4ec,stroke:#880e4f

    subgraph Access["接入域"]
        direction LR
        Clients["API 客户端"]
        Admin["React 管理端"]
    end

    subgraph Core["网关核心域"]
        direction LR
        Management["管理服务<br/>账号 · 模型 · 密钥 · 设置"]
        Sync["账号同步<br/>凭据 · 额度 · 模型"]
        Gateway["网关服务<br/>协议 · 路由 · 选号 · 重试"]
        Audit["审计服务<br/>用量 · 客户端计费"]
        Management --> Sync
        Gateway -.-> Audit
    end

    subgraph Providers["Provider 渠道域"]
        direction LR
        Registry["Provider 注册表"]
        Build["Grok Build<br/>OAuth · 动态模型 · Billing"]
        Web["Grok Web<br/>SSO · 远端额度 · 媒体"]
        Console["Grok Console<br/>SSO · 本地窗口 · 无状态"]
        Registry --> Build
        Registry --> Web
        Registry --> Console
    end

    subgraph Infra["共享基础设施域"]
        direction LR
        Egress["出口管理器<br/>作用域 · 代理池 · 回退 · Clearance"]
        Database[("SQLite / PostgreSQL")]
        Runtime[("Memory / Redis")]
    end

    Upstream["🌐 Grok 上游"]

    %% 跨域调用
    Clients --> Gateway
    Admin --> Management
    Gateway --> Registry
    Sync --> Registry
    Build -->|grok_build| Egress
    Web -->|grok_web / asset| Egress
    Console -->|grok_console| Egress
    Egress --> Upstream
    Management --> Database
    Audit --> Database
    Gateway <--> Runtime

    %% 应用样式
    class Clients,Admin access
    class Management,Sync,Gateway,Audit core
    class Registry,Build,Web,Console providers
    class Egress,Database,Runtime infra
    class Upstream upstream
```

网关通过 Provider 注册表分发请求，账号同步负责刷新凭据、额度和模型。三个渠道独立维护账号状态并使用隔离的出口作用域；请求结束后统一结算用量、审计和客户端计费。

### 核心能力

| 模块 | 能力 |
| :-- | :-- |
| 接口 | Responses、Chat Completions、Anthropic Messages、Images 与异步 Videos |
| 客户端 | Codex、Claude Code，以及 OpenAI/Anthropic 兼容 SDK |
| 账号 | 批量导入导出、额度同步、凭据续期、转换、账号工具与清理 |
| 路由 | 模型发现、Provider 限定、会话粘滞、额度/并发门禁和有界切换 |
| 会话 | stored response、compact、Prompt Cache 亲和与可选 reasoning replay |
| 媒体 | 图片生成与编辑、视频任务、本地归档及 URL/Base64/SSE 输出 |
| 出口 | HTTP/SOCKS/Resin 与 Trojan/VLESS/Shadowsocks/VMess 隧道、订阅、探测、代理池、调配、回退与 FlareSolverr |
| 运维 | Dashboard、模型路由、客户端密钥、审计、运行设置和媒体库 |

### Provider 边界

| Provider | 认证 | 模型 | 主要能力 |
| :-- | :-- | :-- | :-- |
| Grok Build | OAuth / 设备授权 | 按账号动态发现 | Responses、Chat、Messages、compact、stored response、付费账号视频 |
| Grok Web | SSO | 内置并按等级过滤 | Responses、Chat、Messages、stored response、图片、图片编辑、视频 |
| Grok Console | SSO | 内置 | 无状态 Responses、Chat、Messages、图片、图片编辑、视频、TTS、STT、Realtime |

三个 Provider 独立维护凭据、额度、健康、冷却、并发与模型能力。单条路由的账号重试始终留在当前 Provider；当同一对外模型名主动聚合了多条路由时，网关可选择另一条可调度路由，但不会跨 Provider 混用账号状态。

## 快速部署

官方镜像支持 `linux/amd64` 和 `linux/arm64`。

```bash
git clone https://github.com/chenyme/grok2api.git
cd grok2api
cp config.example.yaml config.yaml
```

生成密钥并写入 `config.yaml`：

```bash
openssl rand -hex 32
openssl rand -base64 32
```

```yaml
secrets:
  jwtSecret: "替换为生成的 Hex 密钥"
  credentialEncryptionKey: "替换为生成的 Base64 密钥"

bootstrapAdmin:
  username: "admin"
  password: "替换为强密码"
```

启动服务：

```bash
docker compose pull
docker compose up -d
docker compose logs -f grok2api
```

访问 `http://127.0.0.1:8000`。镜像已包含前端，SQLite 数据库与本地媒体保存在 Compose 数据卷中。

### 源码运行

```bash
cp config.example.yaml config.yaml
make run
```

单独运行前端开发服务：

```bash
cd frontend
pnpm install
pnpm dev
```

## 初始化网关

1. 使用初始管理员登录。
2. 接入 Build、Web 或 Console 账号。
3. 等待额度和模型能力同步完成。
4. 在“模型路由”中确认公开模型。
5. 在“客户端密钥”中创建密钥。
6. 使用该密钥调用 `/v1/*`。

首次登录后请修改管理员密码，并从配置中删除 `bootstrapAdmin`。账号写入后不要更换 `credentialEncryptionKey`。

### 账号操作

| Provider | 接入或导入 | 导出 |
| :-- | :-- | :-- |
| Build | 设备授权、JSON/JSONL | 可重新导入的账号文件 |
| Web | 粘贴/TXT SSO、JSON/JSONL | 可重新导入的账号文件 |
| Console | 粘贴/TXT SSO、JSON/JSONL | 可重新导入的账号文件 |

导入兼容 UTF-8 BOM。批量额度同步、Build 凭据续期、Web→Build/Console 转换、账号工具和账号清理均显示实时进度。

Web 账号工具支持接受协议、设置对应 20–40 岁的随机生日和开启 NSFW；已完成步骤会记录并在后续执行时跳过。

系统支持自动删除长期处于 `reauthRequired` 的账号，默认关闭；存在活动推理租约或视频任务的账号不会被删除。

> [!TIP]
> 从 Python 版迁移时，请将 Grok Web SSO 导出为 TXT，再导入“Grok Web”。旧数据库和号池元数据不兼容。

## 模型与路由

Build 模型根据每个账号的实际能力动态发现；Web、Console 使用内置目录。管理端“模型路由”展示 Provider 前缀、接口能力和支持账号数；客户端应以 `GET /v1/models` 返回的当前可服务模型为准。

### Grok Build

Build 不使用全局固定模型清单。账号同步会读取上游 `/models`，不同账号、订阅等级或灰度批次可能返回不同模型，网关按账号能力参与调度，不会用单个账号覆盖全局目录。

| 模型 | 类型 | 可用条件 | 网关接口能力 |
| :-- | :-- | :-- | :-- |
| 上游 `/models` 返回的对话模型（例如 `grok-4.5`） | 对话 | 当前账号实际返回 | Chat Completions、Responses、Messages、compact、stored response |
| `grok-composer-2.5-fast` | 对话 | Grok Build OAuth 账号 | Chat Completions、Responses、Messages；即使上游稀疏目录暂未列出，网关也会按 OAuth 会话能力补齐 |
| `grok-imagine-video-1.5` | 视频 | Super/付费 Build 账号 | Videos；Free 或能力未知账号不会获得该路由 |

对话请求会转换到 Build Responses 协议，并保留 Codex、Claude Code 所需的工具、推理、多轮与 Prompt Cache 兼容逻辑。Build 当前不提供图片生成和图片编辑路由。

### Grok Web

Web 使用内置目录并按账号等级过滤；更高等级继承低等级模型。

| 模型 | 类型 | 最低等级 | 网关接口能力 |
| :-- | :-- | :-- | :-- |
| `grok-chat-fast` | 对话 | Basic | Chat Completions、Responses、Messages |
| `grok-chat-auto` | 对话 | Super | Chat Completions、Responses、Messages |
| `grok-chat-expert` | 对话 | Super | Chat Completions、Responses、Messages |
| `grok-chat-heavy` | 对话 | Heavy | Chat Completions、Responses、Messages |
| `grok-imagine-image-lite` | 图像 | Basic | Images Generations |
| `grok-imagine-image-quality-lite` | 图像 | Basic | Images Generations |
| `grok-imagine-image-edit` | 图像编辑 | Super | Images Edits |
| `grok-imagine-video` | 视频 | Super | Videos |

### Grok Console

Console 使用当前版本内置目录。对话为无状态转发；图片、视频和语音使用 xAI 标准资源接口。

| 模型 | 类型 | 网关接口能力 |
| :-- | :-- | :-- |
| `grok-4.20-0309-non-reasoning` | 对话 | Chat Completions、Responses、Messages |
| `grok-4.20-0309-reasoning` | 对话 | Chat Completions、Responses、Messages；模型会推理，但上游不接受可配置 `reasoningEffort` |
| `grok-4.20-multi-agent-0309` | 对话 | Chat Completions、Responses、Messages |
| `grok-4.5` | 对话 | Chat Completions、Responses、Messages |
| `grok-4.3` | 对话 | Chat Completions、Responses、Messages |
| `grok-build-0.1` | 对话 | Chat Completions、Responses、Messages |
| `grok-imagine-image` | 图像、图像编辑 | Images Generations、Images Edits |
| `grok-imagine-image-quality` | 图像、图像编辑 | Images Generations、Images Edits |
| `grok-imagine-image-2.0` | 图像、图像编辑 | Images Generations、Images Edits |
| `grok-imagine-video` | 视频 | Videos |
| `grok-imagine-video-1.5` | 视频 | 视频生成，包括 Free Console 账号 |
| `grok-voice-latest`、`grok-voice-think-fast-2.0`、`grok-voice-think-fast-1.0` | 语音 | TTS 和 Realtime WebSocket 代理 |
| `grok-stt` | 语音 | STT 和 OpenAI 兼容的音频转录 |

同一个 Console 图片模型的生成与编辑能力会聚合展示为一条逻辑模型，不需要创建 `-edit` 模型副本。

公开模型名通常不带 Provider。内部路由使用 `Build/`、`Web/` 或 `Console/` 前缀；带前缀名称可显式限定来源。

Web 可与对应的 Build、Console 建立一对一弱关联。关联只共享匿名出口身份和来源展示，不合并凭据、额度、健康、冷却、并发、模型能力或计费。

### Codex、Claude Code 与 Prompt Cache

Responses 与 Messages 支持流式、工具、推理、多轮会话和 compact。客户端会话信号会保持稳定，用于 Grok Build Prompt Cache 亲和；实际命中仍要求上游账号兼容且请求前缀未变化。

Responses 与 Chat Completions 按 OpenAI 语义报告输入总量；Messages 按 Anthropic 语义分开报告未缓存输入和缓存读取。审计保留输入总量与缓存部分，用于计费对账。

## API

推理接口使用客户端密钥：

```http
Authorization: Bearer g2a_xxx_xxx
```

| 方法 | 路径 | 用途 |
| :-- | :-- | :-- |
| `GET` | `/healthz`、`/readyz` | 存活与就绪检查 |
| `GET` | `/v1/models` | 当前可服务模型 |
| `POST` | `/v1/responses` | Responses JSON/SSE |
| `POST` | `/v1/responses/compact` | 压缩支持的 Response 会话 |
| `GET`、`DELETE` | `/v1/responses/{id}` | 查询或删除 stored response |
| `POST` | `/v1/chat/completions` | Chat Completions JSON/SSE |
| `POST` | `/v1/messages` | Anthropic Messages JSON/SSE |
| `POST` | `/v1/images/generations`、`/v1/images/edits` | 生成或编辑图片 |
| `POST`、`GET` | `/v1/videos/*` | 创建和查询视频任务 |
| `POST` | `/v1/tts`、`/v1/audio/speech`、`/v1/audio/tasks` | 语音合成 |
| `POST` | `/v1/stt`、`/v1/audio/transcriptions` | 音频转录 |
| `GET` | `/v1/stt`、`/v1/realtime` | 代理语音 WebSocket 会话 |
| `GET` | `/v1/media/images/{asset_id}`、`/v1/media/videos/{asset_id}` | 读取归档媒体 |

stored response 和 compact 取决于最终 Provider。登录管理端后可在 `/docs` 查看当前模型与调用示例；仅在 `server.swaggerEnabled: true` 时提供 Swagger。

`/v1/audio/transcriptions` 支持 `json`（默认）、`verbose_json` 和 `text`。视频编辑与延长按实际路由校验 Console `grok-imagine-video`，对外模型名仍可自定义。金额计费以网关能够可靠测量的官方计价单位为准：TTS 按输入字符数预留并结算，REST 与流式 STT 按成功响应返回的实际音频时长结算。STT 时长只能在请求完成后获得，因此并发中的请求可能使有限额 Key 短暂超过金额上限。Realtime、视频编辑与延长，以及未收录官方定价的自定义路由当前记录为“未计费”，保持可调用且不消耗金额额度。

客户端密钥支持模型白名单，以及可选的 RPM、并发、用量和截止日期限制。

```bash
curl http://127.0.0.1:8000/v1/responses \
  -H "Authorization: Bearer g2a_xxx_xxx" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "your-model",
    "input": "用三句话解释量子隧穿。",
    "stream": true
  }'
```

## 出口与 Cloudflare

出口节点按 Build、Web、Console 或 Web 资源隔离。管理端支持：

- HTTP、HTTPS、SOCKS4/4A、SOCKS5/5H、Resin、Trojan、VLESS、Shadowsocks 与 VMess
- 隧道协议支持 TCP、WebSocket 和 TLS，未实现的传输形态会在导入时拒绝
- 订阅和文本/Base64 导入
- 批量探测、筛选、删除、分配与均衡
- 按作用域配置无回退、直连或固定节点
- 代理池模式，单次连接失败不会触发全局冷却
- 固定代理传输失败后立即复测；同节点复测自动合并，后续绑定请求限时等待并在恢复后快速重试
- 可选的[出口质量守护程序](./tools/egress-quality-guard/README.zh-CN.md)，支持逐节点模型探测、防误杀隔离和自动恢复；通过内置的 `quality-guard` Compose profile 按需启用

Hysteria 与 TUIC 暂未支持。FlareSolverr 仅接受 HTTP/SOCKS 代理地址，因此自动刷新 Clearance 暂不能直接使用隧道分享链接。

首次启用时只需在 `config.yaml` 中增加 `qualityGuard` 并启动 profile。主程序会自动创建并稳定复用不可导出的系统探测身份：

```yaml
qualityGuard:
  enabled: true
  model: "grok-4.5"
```

```bash
docker compose --profile quality-guard up -d --build
```

曾使用预览版 `clientKeyID` 配置的现有部署可以直接升级：该字段会被兼容读取但不再使用，可安全删除；原来手工创建的探测 Key 不会被程序擅自删除。

后续修改该配置时，执行 `docker compose --profile quality-guard restart grok2api egress-quality-guard` 使基础配置重新加载；管理页面中的策略调整仍支持热加载。

普通的 `docker compose up -d` 不会启动守护程序，也不会产生主动探测流量。sidecar 只从主程序获得权限受限的内部凭据，不保存或使用管理员密码。启用自动隔离前请先阅读上面的详细说明。

Resin 用户名支持 `{account}`：

```text
socks5h://Default.{account}:RESIN_PROXY_TOKEN@resin:2260
```

占位符会替换为稳定的匿名身份。已关联的 Web、Build、Console 可共享该身份，不直接使用 Token 或 Email。

如需自动维护 Web/Console Cloudflare Clearance：

```bash
docker compose --profile flaresolverr up -d
```

随后在 **运行设置 → 媒体与网络 → Clearance** 填写 `http://flaresolverr:8191`，并选择一种托管模式：

- `FlareSolverr` 按配置周期主动刷新固定出口中过期的 Clearance。
- `按需刷新` 不按时间淘汰最后一次成功的 Clearance，只在上游明确拒绝并将其标记失效后重新求解；后台定时任务不会在该模式下启动浏览器。

`手动维护` 始终不会调用 FlareSolverr。按需模式允许首次请求不携带托管 Clearance；若被 Cloudflare 拒绝，下一次租约会执行一次经过并发去重的求解。

出口层只重试可以确认发生在请求提交前的连接故障，不会重放已经提交的生成请求、认证失败、额度耗尽或上游限流。

固定代理进入冷却后会立即触发一次独立连通性复测。同一节点的并发故障只启动一个探针；后续绑定请求最多等待 5 秒，复测健康后重新读取节点状态并继续，不健康则保持原冷却。代理池每次获取新隧道，单个旋转出口失败不会让整个池进入冷却。完整设计与安全边界见[即时故障复测与限时重试](./backend/internal/infra/egress/FAILURE_RETRY.md)。

## 配置与部署

`config.yaml` 保存启动配置；Provider 和运维参数由管理端维护，未标记“重启生效”的设置支持热加载。

| 场景 | 数据库 | 运行态 | 媒体 |
| :-- | :-- | :-- | :-- |
| 单实例 | SQLite | Memory | 本地目录 |
| 多实例 | PostgreSQL | Redis | 共享且可读写的目录 |

多实例需要为每个副本设置唯一的 `deployment.instanceID`，统一使用同一个 `clusterID`；只有媒体目录已正确共享时才设置 `sharedMedia: true`。

PostgreSQL 凭据可以通过环境变量注入，无需写入 `config.yaml`：

```bash
GROK2API_DATABASE_URL='postgresql://user:password@host:5432/grok2api?sslmode=require' docker compose up -d
```

非空的 `GROK2API_DATABASE_URL` 会覆盖 `database.postgres.dsn` 并自动选择 `postgres`；空值不会覆盖 YAML。支持 `postgres://` 和 `postgresql://`，SQLAlchemy 的 `postgresql+asyncpg://` 会返回格式迁移提示。程序不会隐式读取通用的 `DATABASE_URL`；平台只提供该变量时，可在部署清单中显式映射为 `GROK2API_DATABASE_URL: "${DATABASE_URL}"`。数据库配置优先级为：内置默认值 < `config.yaml` < `GROK2API_DATABASE_URL`。当前 CLI 没有数据库覆盖参数。

重要的可选设置：

- `audit.ledgerMode`：`observe` 仅报告账本故障；`enforce` 可暂停新推理以保护计费准确性。
- `routing.accountIsolatedConnections`：为外部 L4 或按连接哈希的负载均衡器按账号拆分出站 TCP/HTTP 连接池。默认关闭，因为会增加连接数、TLS 握手、内存和文件描述符占用。
- `routing.segmentedSelectorEnabled`：默认对至少 3000 个可用账号的大号池启用，限制动态并发读取规模，同时保留额度/等级优先级、会话粘性、完整选号回退与原子门禁。
- Build 响应头超时和精确匹配的 403 失效规则支持热加载。
- “同步最新版本”可应用已验证的 Grok Build 客户端版本和 User-Agent。

## 生产检查

- 使用 HTTPS，并启用 `auth.secureCookies`。
- 公网部署保持 Swagger 关闭。
- 使用强密钥并妥善备份；不要提交凭据、Cookie、账号导出或数据库。
- 备份 `config.yaml`、数据库和媒体目录。
- 多实例同时使用 PostgreSQL、Redis 与共享媒体。
- 公网服务前置反向代理与访问控制。

## 开发验证

```bash
cd backend
go test ./...
go test -race ./...
go vet ./...
go build ./cmd/grok2api
```

```bash
cd frontend
pnpm install --frozen-lockfile
pnpm lint
pnpm build
```

修改公开 API 注释后重新生成 Swagger：

```bash
make swagger
```

## 相关文档

- [English README](./README.md)
- [后端说明](./backend/README.md)
- [前端说明](./frontend/README.md)
