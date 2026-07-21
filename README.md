<p align="center">
  <img alt="Grok2API" src="./frontend/public/grok2api.png" width="720" />
</p>

<p align="center">
  <strong>A multi-account API gateway for Grok Build, Grok Web, and Grok Console</strong>
</p>

<p align="center">
  English | <a href="./README.zh-CN.md">简体中文</a>
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
> Check out [DEEIX-AI / DEEIX-Chat](https://github.com/DEEIX-AI/DEEIX-Chat), a lightweight, integrated AI platform for model routing, chat, files, tools, billing, identity, and operations.

> [!NOTE]
> This project is for technical research and learning purposes only. Please comply with Grok's official terms of use and local laws when using it; otherwise, you will be solely responsible for all consequences!

## Sponsors
> [Want to sponsor this project?](mailto:chenyme03@gmail.com)

<table>
<tr>
<td width="200" align="center" valign="middle"><a href="https://github.com/DEEIX-AI/DEEIX-Chat"><img src="frontend/public/sponner/deeix-chat_deeix-ai.png" alt="DEEIX AI / DEEIX Chat" width="160"></a></td>
<td valign="middle">DEEIX-Chat is an open-source, self-hostable AI Chat platform for individuals, teams, and enterprises that need stable, long-term, unified access to multiple models. It brings models, conversations, files, tool calling, and administration together in one deployable and extensible system. Click <a href="https://github.com/DEEIX-AI/DEEIX-Chat">here</a> to start deploying.</td>
</tr>
<tr>
<td width="200" align="center" valign="middle"><a href="https://www.right.codes/register"><img src="frontend/public/sponner/rightcode.jpg" alt="RightCode" width="160"></a></td>
<td valign="middle">Right Code is an enterprise-grade AI Agent distribution platform that primarily provides stable access services for Claude Code, Codex, Gemini, and other models. It supports invoicing and dedicated one-to-one assistance for enterprises and teams. Thanks to Right Code for providing token support. Click <a href="https://www.right.codes/register">here</a> to register and get started.</td>
</tr>
</table>

<br>

Grok2API is a Go-based Grok API gateway with a built-in React admin console. It organizes Grok Build OAuth, Grok Web SSO, and Grok Console SSO credentials into independent account pools, exposes OpenAI- and Anthropic-style APIs, and provides one place to manage model routes, client keys, quotas, media, audits, and egress proxies.

## Highlights

- **Three Providers**: Build, Web, and Console keep credentials, quotas, health, cooldowns, concurrency, and model capabilities separate
- **Compatible APIs**: Responses, Chat Completions, Anthropic Messages, Images, and asynchronous Videos
- **Model routing**: remote discovery, static catalogs, source pinning, client permissions, and per-account capability filtering
- **Multi-account scheduling**: priorities, quota gates, sticky sessions, concurrency leases, cooldowns, and bounded failover
- **Multi-turn compatibility**: stored-response ownership, compaction, and optional server-side reasoning replay
- **Media pipeline**: image generation, image editing, video jobs, local archiving, and URL/Base64/SSE output
- **Account relationships**: Web-centered links to Build and Console can share a stable egress identity while runtime state stays independent
- **Runtime infrastructure**: SQLite/PostgreSQL, Memory/Redis, and HTTP/SOCKS5/Resin egress
- **Admin console**: dashboard, accounts, model routes, client keys, image gallery, video library, request audits, runtime settings, and update checks
- **Optional account auto-clean** (off by default): runtime settings can periodically hard-delete accounts already marked `reauthRequired` whose `reauth_marked_at` exceeds the configured minimum age. Cooldown-only and still-active permanent-refresh drain accounts are never selected. Accounts with active inference leases or queued/in-progress video jobs are skipped. A distributed maintenance lock prevents duplicate work across shared-runtime instances, and each tick has a bounded deletion budget. First scan waits one interval after enable and after process start; only actual policy changes reschedule the next tick.

## Architecture

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

Requests never mix account state across Providers:

1. The HTTP layer handles authentication, request limits, and protocol detection.
2. The model router resolves a public model name to a Provider-qualified internal route.
3. The Provider Registry verifies that the selected source supports the requested protocol or media operation.
4. The account selector chooses an eligible account from that Provider using capability, quota, stickiness, cooldown, and concurrency state.
5. The matching Adapter performs upstream protocol conversion and forwarding.
6. Audit, quota, billing, response ownership, and concurrency leases are finalized once at the end of the request.

### Provider boundaries

| Provider | Authentication | Model catalog | Quota authority | Exposed capabilities |
| :-- | :-- | :-- | :-- | :-- |
| Grok Build | OAuth / Device OAuth | Discovered per account | Billing | Responses, Chat, Messages, Compact, stored responses, Video |
| Grok Web | SSO | Built in and filtered by account tier | Upstream quota windows | Responses, Chat, Messages, Images, Image Edit, Video |
| Grok Console | SSO | Built in | Local window | Stateless Responses, Chat, Messages |

Providers are integrated through focused capability interfaces. Generic Gateway and HTTP Handler code does not construct private Provider requests. The dependency direction remains:

```text
Transport → Application → Domain
                 ↑
       Infrastructure adapters
```

### Technology stack

| Layer | Technology |
| :-- | :-- |
| Backend | Go 1.26, Gin, GORM |
| Frontend | React 19, TypeScript, Vite, Tailwind CSS, shadcn/ui |
| Database | SQLite / PostgreSQL |
| Runtime | Memory / Redis |

### Repository layout

```text
backend/
  cmd/grok2api/          Process entry point
  internal/domain/      Domain models and stable rules
  internal/application/ Use cases, scheduling, and finalization
  internal/infra/       Providers, persistence, runtime, egress, and security
  internal/transport/   HTTP routes, authentication, and DTOs
frontend/
  src/app/              Routing, application shell, and global providers
  src/features/         Feature-oriented pages and interactions
  src/entities/         Shared domain objects
  src/shared/           API client, auth, components, and utilities
```

## Quick start

### Docker Compose (recommended)

Official GHCR images are published for both `linux/amd64` and `linux/arm64`.

```bash
git clone https://github.com/chenyme/grok2api.git
cd grok2api
cp config.example.yaml config.yaml
```

Generate secure secrets:

```bash
openssl rand -hex 32
openssl rand -base64 32
```

Write the generated values to `config.yaml` and replace the bootstrap password:

```yaml
secrets:
  jwtSecret: "replace-with-the-generated-hex-value"
  credentialEncryptionKey: "replace-with-the-generated-base64-key"

bootstrapAdmin:
  username: "admin"
  password: "replace-with-a-strong-password"
```

Start the service:

```bash
docker compose pull
docker compose up -d
docker compose logs -f grok2api
```

The admin console is available at `http://127.0.0.1:8000` by default.

Compose mounts `config.yaml` read-only and stores the SQLite database and local media in the `grok2api-data` volume. The image already contains the frontend; no separate web deployment is required.

Common maintenance commands:

```bash
docker compose restart grok2api
docker compose down
```

### Run from source

```bash
cp config.example.yaml config.yaml
make run
```

To run the frontend development server separately:

```bash
cd frontend
pnpm install
pnpm dev
```

The frontend runs at `http://127.0.0.1:5173` by default and proxies API requests to `http://127.0.0.1:8000`.

## First-time setup

1. Sign in with the administrator created from `bootstrapAdmin`.
2. Add a Build, Web, or Console account under **Upstream Accounts**.
3. Wait for the initial quota and model-capability sync to complete.
4. Review public model names, sources, and enabled routes under **Model Routes**.
5. Create a `g2a_` API key under **Client Keys**.
6. Use that key to call `/v1/*`.

After the administrator has been created, change its password and remove `bootstrapAdmin` from the configuration. Keep `credentialEncryptionKey` permanently: changing it makes existing encrypted credentials unreadable.

## Models and routing

Public model names are unqualified by default. Internally, `Build/`, `Web/`, and `Console/` are used as stable route IDs. Qualified names remain available for explicitly selecting a source, but they are not shown as ordinary model names.

Build models are discovered from the real capabilities of each account, so the project does not maintain a fixed list that quickly becomes stale. The admin console stores the last successful capability snapshot for every account, and the public catalog is the union of currently serviceable account capabilities. Always use the model page or this endpoint as the source of truth:

```http
GET /v1/models
```

### Built-in Grok Web models

| Model | Capability | Minimum tier |
| :-- | :-- | :-- |
| `grok-chat-fast` | Chat / Responses / Messages | Basic |
| `grok-chat-auto` | Chat / Responses / Messages | Super |
| `grok-chat-expert` | Chat / Responses / Messages | Super |
| `grok-chat-heavy` | Chat / Responses / Messages | Heavy |
| `grok-imagine-image` | Image generation | Basic |
| `grok-imagine-image-quality` | High-quality image generation | Super |
| `grok-imagine-image-edit` | Image editing | Super |
| `grok-imagine-video` | Video generation | Super |

### Built-in Grok Console models

| Model | Description |
| :-- | :-- |
| `grok-4.3` | Supports reasoning effort and search tools |
| `grok-4.20-0309` | General Responses model |
| `grok-4.20-0309-reasoning` | Reasoning variant |
| `grok-4.20-0309-non-reasoning` | Non-reasoning variant |
| `grok-4.20-multi-agent-0309` | Multi-agent variant |
| `grok-build-0.1` | Build-family model |

Console also exposes compatibility and reasoning-effort aliases such as `grok-4.3-low`, `grok-4.3-medium`, `grok-4.3-high`, and `grok-4.20-multi-agent-xhigh`. Console is stateless and does not support `previous_response_id`, Response retrieval/deletion, or compact.

Build models such as `grok-4.5` come from the dynamic account catalog and are not part of the Console static catalog.

The same public model can be exposed by multiple sources. Routing first selects a source that satisfies client permissions and protocol capabilities; subsequent account failover stays within that Provider pool and never migrates quota, cooldown, or multi-turn state to another Provider.

## API

Client inference endpoints require an API key. Health checks, media reads with unguessable asset IDs, and one-time upload tickets use separate authorization boundaries:

```http
Authorization: Bearer g2a_xxx_xxx
```

| Method | Path | Description |
| :-- | :-- | :-- |
| `GET` | `/healthz` | Liveness check |
| `GET` | `/readyz` | Layered readiness status |
| `GET` | `/v1/models` | Currently serviceable models |
| `POST` | `/v1/responses` | Responses JSON / SSE |
| `POST` | `/v1/responses/compact` | Responses compact |
| `GET` | `/v1/responses/{id}` | Retrieve a stored response |
| `DELETE` | `/v1/responses/{id}` | Delete a stored response |
| `POST` | `/v1/chat/completions` | Chat Completions JSON / SSE |
| `POST` | `/v1/messages` | Anthropic Messages JSON / SSE |
| `POST` | `/v1/images/generations` | Image generation |
| `POST` | `/v1/images/edits` | Image editing with JSON or multipart input |
| `POST` | `/v1/videos/generations` | Create an asynchronous video job |
| `GET` | `/v1/videos/{request_id}` | Inspect a video job |
| `GET` | `/v1/videos/{request_id}/content` | Retrieve video job content |
| `GET` | `/v1/media/images/{asset_id}` | Read an archived image |
| `GET` | `/v1/media/videos/{asset_id}` | Read an archived video |
| `PUT` | `/v1/media/uploads/{token}` | Receive a video through a one-time upload ticket |

Stored responses and compact are available only when the selected Provider supports them. Signed-in administrators can open `/docs` for the active base URL, current models, and request examples. Swagger is registered at `/swagger/index.html` only when `server.swaggerEnabled: true`.

Minimal request example:

```bash
export GROK2API_API_KEY="g2a_xxx_xxx"

curl http://127.0.0.1:8000/v1/responses \
  -H "Authorization: Bearer $GROK2API_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "grok-chat-auto",
    "input": "Explain quantum tunneling in three sentences.",
    "stream": true
  }'
```

## Configuration, runtime state, and multi-instance deployments

`config.yaml` contains startup configuration only:

| Group | Description |
| :-- | :-- |
| `server` | Listen address, request limits, timeouts, and Swagger |
| `auth` | Admin token lifetime and secure cookies |
| `secrets` | JWT and credential-encryption keys |
| `frontend` | Static assets and the optional public address |
| `database` | SQLite or PostgreSQL |
| `runtimeStore` | Memory or Redis |
| `media` | Media storage driver and path |
| `routing` | Server-side multi-turn replay cache |

Provider settings, service capacity, batch concurrency, model routes, media, audits, and egress proxies are managed from the admin console. Settings that are not explicitly marked as restart-required are hot-reloaded.

| Deployment | Database | Runtime store | Media |
| :-- | :-- | :-- | :-- |
| Single instance | SQLite | Memory | Local directory |
| Multiple instances | PostgreSQL | Redis | Shared volume or instance affinity |

The relational database stores accounts, credentials, models, quotas, client keys, audits, and media metadata. Redis coordinates distributed rate limits, concurrency leases, sticky sessions, locks, quota recovery, and multi-instance setting notifications; it does not replace the relational database.

### Account scheduling and cross-Provider links

- A sticky-session hit prefers the account already bound to the conversation. If that account is temporarily full, the selector waits briefly before borrowing another eligible account according to policy.
- Without a valid binding, the selector combines priority, model capability, quota, concurrency, and last-selected time.
- Web accounts can form one-to-one weak links with corresponding Build and Console accounts.
- A link shares only an anonymous egress identity and management-page provenance. Credentials, quotas, availability, cooldowns, concurrency, model capabilities, and billing remain independent.
- Email addresses are used only for display and search, never as proxy identities.

### Managed FlareSolverr clearance

To automatically maintain Grok Web Cloudflare Clearance, start the optional FlareSolverr Compose service:

```bash
docker compose --profile flaresolverr up -d
# or
podman compose --profile flaresolverr up -d
```

Then open **Runtime Settings → Media & Network → Clearance**, select `FlareSolverr`, and use `http://flaresolverr:8191` as the solver URL. FlareSolverr is not published on the host; each Web or Console egress node uses its own proxy to obtain cookies and User-Agent.

### Resin sticky proxies

Proxy usernames support the `{account}` placeholder:

```text
socks5h://Default.{account}:RESIN_PROXY_TOKEN@resin:2260
```

At runtime, the placeholder is replaced with a stable anonymous account identity. Linked Web, Build, and Console accounts can reuse the same identity; unlinked accounts continue to use their own fallback identities. Token refreshes do not rotate a persisted identity.

The egress layer retries only connection errors that clearly occur before a request is submitted. Submitted generation requests, authentication failures, exhausted quotas, and upstream rate limits are never automatically replayed at the egress layer.

## Security and production guidance

- Serve the application over HTTPS and enable `auth.secureCookies` for an HTTPS admin address
- Generate strong random values for `jwtSecret` and `credentialEncryptionKey`
- Keep `server.swaggerEnabled: false` in production
- Never commit OAuth data, SSO tokens, cookies, account exports, or real databases
- Use PostgreSQL and Redis for multi-instance deployments, plus shared media storage or instance affinity
- Back up `config.yaml`, the relational database, and the media directory
- Place a reverse proxy, access controls, and basic network protections in front of public deployments

Credentials are encrypted at rest, while client keys, logs, remote-resource downloads, and request/response bodies have explicit security boundaries. Public documentation focuses on stable capabilities, deployment, and operational behavior.

## Development and verification

Backend:

```bash
cd backend
go test ./...
go test -race ./...
go vet ./...
go build ./cmd/grok2api
```

Frontend:

```bash
cd frontend
pnpm install --frozen-lockfile
pnpm lint
pnpm build
```

After changing public API annotations, regenerate Swagger from the repository root:

```bash
make swagger
```

## Further reading

- [简体中文 README](./README.zh-CN.md)
- [Backend guide](./backend/README.md)
- [Frontend guide](./frontend/README.md)
