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
<td width="200" align="center" valign="middle"><a href="https://github.com/DEEIX-AI/DEEIX-Chat"><img src="frontend/public/sponner/deeix-chat_deeix-ai.png" alt="DEEIX AI / DEEIX Chat" width="160"></a></td>
<td valign="middle">DEEIX-Chat 是一款开源可部署的 AI Chat 平台，面向需要长期、稳定、统一使用多模型能力的个人、团队与企业，将模型、对话、文件、工具调用与后台管理整合为一套可部署、可扩展的系统。点击 <a href="https://github.com/DEEIX-AI/DEEIX-Chat">此处</a> 开始部署！</td>
</tr>
<tr>
<td width="200" align="center" valign="middle"><a href="https://www.right.codes/register"><img src="frontend/public/sponner/rightcode.jpg" alt="RightCode" width="160"></a></td>
<td valign="middle">Right Code 是一个企业级 AI Agent 分发平台，主要提供稳定的 Claude Code、Codex、Gemini 等模型的中转服务。充值即可开票，企业、团队用户一对一对接。感谢 Right Code 提供的 Tokens 支持，点击 <a href="https://www.right.codes/register">此处</a> 注册并开始使用！</td>
</tr>
</table>

<br>

Grok2API 是一个以 Go 为核心、内置 React 管理端的 Grok API 网关。它将 Grok Build OAuth、Grok Web SSO 与 Grok Console SSO 组织成相互独立的账号池，对外提供 OpenAI 与 Anthropic 风格接口，并统一管理模型路由、客户端密钥、额度、媒体、审计和出口代理。

## 功能概览

- **三 Provider**：Build、Web、Console 分别维护凭据、额度、健康、冷却、并发和模型能力
- **兼容接口**：Responses、Chat Completions、Anthropic Messages、Images 与异步 Videos
- **模型路由**：动态模型发现、静态目录、来源限定、客户端权限和账号能力过滤
- **多账号调度**：优先级、额度门控、会话粘滞、并发租约、冷却和有界故障切换
- **多轮兼容**：stored response 归属、compaction，以及可选的服务端 reasoning replay
- **媒体链路**：图片生成、图片编辑、视频任务、本地归档和 URL/Base64/SSE 输出
- **账号关联**：以 Web 为中心展示 Build/Console 弱关联，并可共享稳定出口身份；运行状态仍彼此独立
- **运行基础设施**：SQLite/PostgreSQL、Memory/Redis、HTTP/SOCKS5/Resin 出口
- **管理后台**：Dashboard、账号、模型、密钥、图库、视频库、请求审计、运行设置和版本检查
- **可选账号自动清理**（默认关闭）：运行设置可按间隔硬删除已标记 `reauthRequired` 且 `reauth_marked_at` 超过最短保留时长的账号。不会选中纯冷却账号，也不会打断仍可 drain 的永久 refresh 账号；仍有推理租约或排队中/进行中视频任务的账号会被跳过。共享运行态下使用分布式维护锁避免多实例重复执行，每次 tick 采用有限删除预算。启用后与进程启动后的首次扫描均等待一个间隔，且只有清理策略实际变化时才重排下一次扫描。

## 架构设计

```mermaid
flowchart TB
    Client["OpenAI / Anthropic Clients"] --> Compat["Compatibility API"]
    Admin["React Admin"] --> AdminAPI["Admin API"]

    Compat --> App["Application Services"]
    AdminAPI --> App

    App --> Router["Model Router"]
    Router --> Selector["Account Selector"]
    Selector --> Registry["Provider Registry"]

    Registry --> Build["Grok Build Adapter"]
    Registry --> Web["Grok Web Adapter"]
    Registry --> Console["Grok Console Adapter"]

    App --> DB["SQLite / PostgreSQL"]
    App --> Runtime["Memory / Redis"]
    Build --> Egress["Egress Manager"]
    Web --> Egress
    Console --> Egress
    App --> Media["Media Storage"]
```

请求不会在三个 Provider 之间混用账号状态：

1. HTTP 层完成鉴权、输入上限和协议识别。
2. 模型路由将公开模型名解析为 Provider 限定的内部路由。
3. Provider Registry 根据声明式能力判断是否支持当前协议或媒体操作。
4. 账号选择器在目标 Provider 内按模型能力、额度、粘滞、冷却和并发选号。
5. 对应 Adapter 完成上游协议转换与转发。
6. 审计、额度、计费、响应归属和并发租约在请求结束时统一结算。

### Provider 能力边界

| Provider | 认证 | 模型目录 | 额度来源 | 对外能力 |
| :-- | :-- | :-- | :-- | :-- |
| Grok Build | OAuth / Device OAuth | 按账号从上游发现 | Billing | Responses、Chat、Messages、Compact、stored responses、Video |
| Grok Web | SSO | 内置目录并按账号等级过滤 | 上游额度窗口 | Responses、Chat、Messages、Images、Image Edit、Video |
| Grok Console | SSO | 内置目录 | 本地窗口 | 无状态 Responses、Chat、Messages |

Provider 通过小型能力接口接入，不在通用 Gateway 或 HTTP Handler 中拼装私有上游请求。依赖方向保持为：

```text
Transport → Application → Domain
                 ↑
       Infrastructure adapters
```

### 技术栈

| 层 | 主要技术 |
| :-- | :-- |
| Backend | Go 1.26、Gin、GORM |
| Frontend | React 19、TypeScript、Vite、Tailwind CSS、shadcn/ui |
| Database | SQLite / PostgreSQL |
| Runtime | Memory / Redis |

### 工程结构

```text
backend/
  cmd/grok2api/          进程入口
  internal/domain/      领域模型与稳定规则
  internal/application/ 用例、调度与结算
  internal/infra/       Provider、数据库、运行态、出口与安全实现
  internal/transport/   HTTP 路由、鉴权和 DTO
frontend/
  src/app/              路由、布局和全局 Provider
  src/features/         按业务能力组织的页面与交互
  src/entities/         跨功能领域对象
  src/shared/           API、鉴权、组件与通用工具
```

## 快速部署

### Docker Compose（推荐）

官方 GHCR 镜像同时发布 `linux/amd64` 与 `linux/arm64`。

```bash
git clone https://github.com/chenyme/grok2api.git
cd grok2api
cp config.example.yaml config.yaml
```

生成安全密钥：

```bash
openssl rand -hex 32
openssl rand -base64 32
```

将结果写入 `config.yaml`，并修改首次管理员密码：

```yaml
secrets:
  jwtSecret: "替换为 hex 随机值"
  credentialEncryptionKey: "替换为 Base64 随机密钥"

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

管理端默认地址：`http://127.0.0.1:8000`。

Compose 会将 `config.yaml` 只读挂载到容器，并使用 `grok2api-data` 保存 SQLite 数据库和本地媒体。镜像已经包含前端，无需单独部署 Web 服务。

常用维护命令：

```bash
docker compose restart grok2api
docker compose down
```

### 源码运行

```bash
cp config.example.yaml config.yaml
make run
```

单独启动前端开发服务器：

```bash
cd frontend
pnpm install
pnpm dev
```

前端默认运行在 `http://127.0.0.1:5173`，并将 API 请求代理到 `http://127.0.0.1:8000`。

## 首次使用

1. 使用 `bootstrapAdmin` 创建的管理员登录。
2. 在“上游账号”中接入 Build、Web 或 Console 账号。
3. 等待账号额度和模型能力完成首次同步。
4. 在“模型路由”中确认公开模型名、来源和启用状态。
5. 在“客户端密钥”中创建 `g2a_` API Key。
6. 使用该密钥调用 `/v1/*`。

管理员创建成功后，建议修改密码并从配置中删除 `bootstrapAdmin`。`credentialEncryptionKey` 必须长期保留，更换后已有凭据将无法解密。

## 模型与路由

公开模型名默认不带来源前缀。内部使用 `Build/`、`Web/`、`Console/` 作为稳定路由 ID；带前缀名称仍可用于显式指定来源，但不会作为普通模型名展示。

Build 模型按账号真实能力动态发现，因此不维护容易过期的固定列表。管理端会保存每个账号最后一次成功同步的能力快照，公开目录使用可用账号能力的并集。请始终以管理端模型页或以下接口为准：

```http
GET /v1/models
```

### Grok Web 内置模型

| 模型 | 能力 | 最低等级 |
| :-- | :-- | :-- |
| `grok-chat-fast` | Chat / Responses / Messages | Basic |
| `grok-chat-auto` | Chat / Responses / Messages | Super |
| `grok-chat-expert` | Chat / Responses / Messages | Super |
| `grok-chat-heavy` | Chat / Responses / Messages | Heavy |
| `grok-imagine-image` | 图片生成 | Basic |
| `grok-imagine-image-quality` | 高质量图片生成 | Super |
| `grok-imagine-image-edit` | 图片编辑 | Super |
| `grok-imagine-video` | 视频生成 | Super |

### Grok Console 内置模型

| 模型 | 说明 |
| :-- | :-- |
| `grok-4.3` | 支持 reasoning effort 与搜索工具 |
| `grok-4.20-0309` | 通用 Responses 模型 |
| `grok-4.20-0309-reasoning` | Reasoning 版本 |
| `grok-4.20-0309-non-reasoning` | Non-reasoning 版本 |
| `grok-4.20-multi-agent-0309` | Multi-agent 版本 |
| `grok-build-0.1` | Build 系列模型 |

Console 还提供兼容别名和 reasoning effort 别名，例如 `grok-4.3-low`、`grok-4.3-medium`、`grok-4.3-high` 以及 `grok-4.20-multi-agent-xhigh`。Console 保持无状态语义，不支持 `previous_response_id`、Response 查询/删除或 compact。

`grok-4.5` 等 Build 模型来自账号的动态目录，不属于 Console 的静态目录。

同一个公开模型可以由多个来源提供。路由会先选择一个满足权限和能力的来源，之后的账号切换只发生在该 Provider 的账号池内，不会把额度、冷却或多轮状态迁移到其它 Provider。

## API

客户端推理接口需要 API Key；健康检查、不可猜测 ID 的媒体读取和一次性上传票据是独立授权边界：

```http
Authorization: Bearer g2a_xxx_xxx
```

| 方法 | 路径 | 说明 |
| :-- | :-- | :-- |
| `GET` | `/healthz` | 存活检查 |
| `GET` | `/readyz` | 分层就绪状态 |
| `GET` | `/v1/models` | 当前可服务模型 |
| `POST` | `/v1/responses` | Responses JSON / SSE |
| `POST` | `/v1/responses/compact` | Responses compact |
| `GET` | `/v1/responses/{id}` | 查询 stored response |
| `DELETE` | `/v1/responses/{id}` | 删除 stored response |
| `POST` | `/v1/chat/completions` | Chat Completions JSON / SSE |
| `POST` | `/v1/messages` | Anthropic Messages JSON / SSE |
| `POST` | `/v1/images/generations` | 图片生成 |
| `POST` | `/v1/images/edits` | 图片编辑，支持 JSON 与 multipart |
| `POST` | `/v1/videos/generations` | 创建异步视频任务 |
| `GET` | `/v1/videos/{request_id}` | 查询视频任务 |
| `GET` | `/v1/videos/{request_id}/content` | 获取视频任务内容 |
| `GET` | `/v1/media/images/{asset_id}` | 读取归档图片 |
| `GET` | `/v1/media/videos/{asset_id}` | 读取归档视频 |
| `PUT` | `/v1/media/uploads/{token}` | 使用一次性票据接收视频上传 |

stored response 和 compact 的可用性取决于最终路由到的 Provider。管理端登录后可访问 `/docs` 查看当前 Base URL、实际模型和调用示例；Swagger 仅在 `server.swaggerEnabled: true` 时注册到 `/swagger/index.html`。

最小调用示例：

```bash
export GROK2API_API_KEY="g2a_xxx_xxx"

curl http://127.0.0.1:8000/v1/responses \
  -H "Authorization: Bearer $GROK2API_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "grok-chat-auto",
    "input": "用三句话解释量子隧穿",
    "stream": true
  }'
```

## 配置、运行态与多实例

`config.yaml` 只保存启动所需配置：

| 分组 | 说明 |
| :-- | :-- |
| `server` | 监听地址、请求体限制、超时和 Swagger |
| `auth` | 管理端 Token 与安全 Cookie |
| `secrets` | JWT 与凭据加密密钥 |
| `frontend` | 静态资源目录和可选公开地址 |
| `database` | SQLite 或 PostgreSQL |
| `runtimeStore` | Memory 或 Redis |
| `media` | 媒体存储驱动与路径 |
| `routing` | 服务端多轮回放缓存 |

Provider、服务容量、批量任务并发、模型路由、媒体、审计和出口代理等运行设置由管理端维护；页面未标记“重启生效”的字段会热加载。

| 场景 | 数据库 | 运行态 | 媒体 |
| :-- | :-- | :-- | :-- |
| 单实例 | SQLite | Memory | 本地目录 |
| 多实例 | PostgreSQL | Redis | 共享卷或实例亲和 |

关系型数据库保存账号、凭据、模型、额度、密钥、审计和媒体元数据。Redis 负责分布式限流、并发租约、粘滞会话、锁、额度恢复和多实例设置通知，不替代关系型数据库。

### 账号调度与跨 Provider 关联

- 会话粘滞命中时优先复用原账号；账号暂时满载时会短暂等待，再按规则借用其它可用账号。
- 无粘滞或绑定失效时，选择器综合优先级、模型能力、额度、并发和最近选择时间进行调度。
- Web 可以与对应的 Build、Console 建立一对一弱关联。
- 关联只共享匿名出口身份和管理端来源展示，不共享凭据、额度、可用性、冷却、并发、模型能力或计费。
- Email 仅用于展示和检索，不作为代理身份。

### FlareSolverr 自动维护 Clearance

如需自动维护 Grok Web / Console 的 Cloudflare Clearance，可启动可选的 FlareSolverr Compose 服务：

```bash
docker compose --profile flaresolverr up -d
# 或
podman compose --profile flaresolverr up -d
```

随后在管理端打开 **运行设置 → 媒体与网络 → Clearance**，选择 `FlareSolverr`，并将服务地址设为 `http://flaresolverr:8191`。FlareSolverr 不会暴露到宿主机；每个 Web 或 Console 出口节点均使用自身代理获取匹配的 Cookie 与 User-Agent。

### Resin 粘性代理

出口代理用户名支持 `{account}` 占位符：

```text
socks5h://Default.{account}:RESIN_PROXY_TOKEN@resin:2260
```

运行时会将占位符替换为稳定、匿名的账号身份。已关联的 Web、Build、Console 可以复用同一身份；未关联账号继续使用各自的回退身份。身份不会因为 Token 续期而变化。

出口层只对明确发生在请求提交前的连接错误执行有限重试。已经提交的生成请求、认证失败、额度耗尽和上游限流不会在出口层自动重放。

## 安全与生产建议

- 使用 HTTPS，并在 HTTPS 管理地址下启用 `auth.secureCookies`
- 使用强随机 `jwtSecret` 和 `credentialEncryptionKey`
- 生产环境保持 `server.swaggerEnabled: false`
- 不要将 OAuth、SSO、Cookie、账号导出或真实数据库提交到 Git
- 多实例使用 PostgreSQL 与 Redis，并为媒体配置共享卷或实例亲和
- 备份 `config.yaml`、关系型数据库和媒体目录
- 公网部署建议使用反向代理、访问控制和基础网络防护

服务端对凭据进行加密保存，并对客户端密钥、日志、远程资源下载和请求/响应体设置明确的安全边界。公开文档聚焦稳定能力、部署方式和运维边界。

## 开发与验证

后端：

```bash
cd backend
go test ./...
go test -race ./...
go vet ./...
go build ./cmd/grok2api
```

前端：

```bash
cd frontend
pnpm install --frozen-lockfile
pnpm lint
pnpm build
```

修改公开 API 注释后，在仓库根目录执行：

```bash
make swagger
```

## 进一步阅读

- [English README](./README.md)
- [后端说明](./backend/README.md)
- [前端说明](./frontend/README.md)
