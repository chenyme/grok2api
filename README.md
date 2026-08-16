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
<td width="200" align="center" valign="middle"><a href="https://www.krill-ai.com/register?invite=KJ2VGIRVAE"><img src="https://raw.githubusercontent.com/Krill-ai-org/krill-ai-static/refs/heads/main/krill-logo/Eng/250x150.png" alt="Krill AI" width="160"></a></td>
<td valign="middle">Krill AI provides fast, stable API access to GPT, Claude, Gemini, and leading Chinese models, with enterprise customization, invoicing, 7×16 support, and optimized WebSocket connections for faster first-token latency. Register through the <a href="https://www.krill-ai.com/register?invite=KJ2VGIRVAE">exclusive link</a> and use code “grok2api” for 23% off your first Codex package.</td>
</tr>
<tr>
<td width="200" align="center" valign="middle"><a href="https://github.com/DEEIX-AI/DEEIX-Chat"><img src="frontend/public/sponner/deeix-chat_deeix-ai.png" alt="DEEIX AI / DEEIX Chat" width="160"></a></td>
<td valign="middle">DEEIX-Chat is an open-source, self-hostable AI Chat platform for individuals, teams, and enterprises that need stable, long-term, unified access to multiple models. It brings models, conversations, files, tool calling, and administration together in one deployable and extensible system. Click <a href="https://github.com/DEEIX-AI/DEEIX-Chat">here</a> to start deploying.</td>
</tr>
<tr>
<td width="200" align="center" valign="middle"><a href="https://www.right.codes/register"><img src="frontend/public/sponner/rightcode.jpg" alt="RightCode" width="160"></a></td>
<td valign="middle">Right Code is an enterprise-grade AI Agent distribution platform that primarily provides stable access services for Claude Code, Codex, Gemini, and other models. It supports invoicing and dedicated one-to-one assistance for enterprises and teams. Thanks to Right Code for providing token support. Click <a href="https://www.right.codes/register">here</a> to register and get started.</td>
</tr>
<tr>
<td width="200" align="center" valign="middle"><a href="https://api.fenno.ai/s/xCBS"><img src="frontend/public/sponner/fenno-ai.jpg" alt="FennoAI" width="160"></a></td>
<td valign="middle">FennoAI provides enterprise-grade OpenAI/Anthropic-compatible APIs for Codex, Claude Code, and OpenCode, processing hundreds of billions of tokens daily with global business settlement and invoicing. Through the Grok2API <a href="https://api.fenno.ai/s/xCBS">exclusive offer</a>, USD 1.99 unlocks USD 50 in Coding Plan credits, plus referral commissions up to 20%.</td>
</tr>
<tr>
<td width="200" align="center" valign="middle"><a href="https://s.qiniu.com/RNNZFf"><img src="frontend/public/sponner/qiniu.jpg" alt="Qiniu Cloud AI" width="160"></a></td>
<td valign="middle">Qiniu Cloud AI, Qiniu Cloud’s (02567.HK) enterprise MaaS platform, offers protocol-compatible access to 150+ global models for text, image, audio, video, and files, serving 1.69+ million users. Grok2API registrations through the <a href="https://s.qiniu.com/RNNZFf">exclusive link</a> receive 12 million free enterprise tokens or 3 million developer tokens.</td>
</tr>
</table>

<br>

## Overview

Grok2API is a Go gateway with a built-in React admin console. It manages independent Grok Build, Grok Web, and Grok Console account pools and exposes unified OpenAI- and Anthropic-compatible APIs.

### Architecture

```mermaid
flowchart LR
    %% Color definitions
    classDef access fill:#e1f5fe,stroke:#01579b
    classDef core fill:#fff3e0,stroke:#e65100
    classDef providers fill:#f3e5f5,stroke:#4a148c
    classDef infra fill:#e8f5e9,stroke:#1b5e20
    classDef upstream fill:#fce4ec,stroke:#880e4f

    subgraph Access["Access Domain"]
        direction LR
        Clients["API Clients"]
        Admin["React Admin"]
    end

    subgraph Core["Gateway Core Domain"]
        direction LR
        Management["Management Services<br/>Accounts · Models · Keys · Settings"]
        Sync["Account Sync<br/>Credentials · Quota · Models"]
        Gateway["Gateway Service<br/>Protocols · Routing · Selection · Retry"]
        Audit["Audit Service<br/>Usage · Client Billing"]
        Management --> Sync
        Gateway -.-> Audit
    end

    subgraph Providers["Provider Channel Domain"]
        direction LR
        Registry["Provider Registry"]
        Build["Grok Build<br/>OAuth · Dynamic Models · Billing"]
        Web["Grok Web<br/>SSO · Remote Quota · Media"]
        Console["Grok Console<br/>SSO · Local Window · Stateless"]
        Registry --> Build
        Registry --> Web
        Registry --> Console
    end

    subgraph Infra["Shared Infrastructure Domain"]
        direction LR
        Egress["Egress Manager<br/>Scopes · Proxy Pool · Fallback · Clearance"]
        Database[("SQLite / PostgreSQL")]
        Runtime[("Memory / Redis")]
    end

    Upstream["🌐 Grok Upstream"]

    %% Cross-domain calls
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

    %% Application styles
    class Clients,Admin access
    class Management,Sync,Gateway,Audit core
    class Registry,Build,Web,Console providers
    class Egress,Database,Runtime infra
    class Upstream upstream
```

The Gateway routes requests through the Provider Registry. Account Sync refreshes credentials, quota, and models. Each Provider keeps independent account state and uses an isolated egress scope; usage, audits, and client billing are finalized after the request.

### Core capabilities

| Area | Capabilities |
| :-- | :-- |
| APIs | Responses, Chat Completions, Anthropic Messages, Images, and asynchronous Videos |
| Clients | Codex, Claude Code, OpenAI-compatible SDKs, and Anthropic-compatible SDKs |
| Accounts | Bulk import/export, quota sync, credential renewal, conversion, tools, and cleanup |
| Routing | Model discovery, Provider pinning, sticky sessions, quota/concurrency guards, and bounded failover |
| Sessions | Stored responses, compact, prompt-cache affinity, and optional reasoning replay |
| Media | Image generation/editing, video jobs, local archiving, and URL/Base64/SSE output |
| Egress | HTTP/SOCKS/Resin and Trojan/VLESS/Shadowsocks/VMess tunnels, subscriptions, probes, proxy pools, allocation, fallback, and FlareSolverr |
| Operations | Dashboard, model routes, client keys, audits, runtime settings, and media libraries |

### Provider boundaries

| Provider | Authentication | Models | Main capabilities |
| :-- | :-- | :-- | :-- |
| Grok Build | OAuth / Device OAuth | Discovered per account | Responses, Chat, Messages, compact, stored responses, paid-account video |
| Grok Web | SSO | Built-in, filtered by tier | Responses, Chat, Messages, stored responses, images, image editing, video |
| Grok Console | SSO | Built-in | Stateless Responses, Chat, Messages, images, image editing, video, TTS, STT, Realtime |

Each Provider keeps its own credentials, quota, health, cooldown, concurrency, and model capabilities. Account retries stay within one route; when one public model ID intentionally aggregates multiple routes, the gateway may select another schedulable route without mixing Provider state.

## Quick start

Official images support `linux/amd64` and `linux/arm64`.

```bash
git clone https://github.com/chenyme/grok2api.git
cd grok2api
cp config.example.yaml config.yaml
```

Generate secrets and place them in `config.yaml`:

```bash
openssl rand -hex 32
openssl rand -base64 32
```

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

Open `http://127.0.0.1:8000`. The image already includes the frontend; SQLite data and local media are stored in the Compose volume.

### Run from source

```bash
cp config.example.yaml config.yaml
make run
```

For frontend development:

```bash
cd frontend
pnpm install
pnpm dev
```

## Set up the gateway

1. Sign in with the bootstrap administrator.
2. Connect a Build, Web, or Console account.
3. Wait for its quota and model capabilities to sync.
4. Review the public routes under **Model Routes**.
5. Create a client key under **Client Keys**.
6. Call a `/v1/*` endpoint with that key.

After first sign-in, change the administrator password and remove `bootstrapAdmin` from the configuration. Never rotate `credentialEncryptionKey` after credentials have been stored.

### Account operations

| Provider | Connect or import | Export |
| :-- | :-- | :-- |
| Build | Device OAuth, JSON/JSONL | Re-importable account file |
| Web | Pasted/TXT SSO, JSON/JSONL | Re-importable account file |
| Console | Pasted/TXT SSO, JSON/JSONL | Re-importable account file |

Imports accept UTF-8 BOM. Bulk quota sync, Build credential renewal, Web→Build/Console conversion, account tools, and cleanup report live progress.

Web account tools can accept the terms, set a random birthday corresponding to an age of 20–40, and enable NSFW. Completed steps are recorded and skipped on later runs.

Automatic deletion of old `reauthRequired` accounts is available but disabled by default. Active inference leases and video jobs are protected.

> [!TIP]
> To migrate from the Python version, export Grok Web SSO tokens as TXT and import them under **Grok Web**. Old pool metadata and databases are not compatible.

## Models and routing

Build models are discovered from each account's actual capabilities. Web and Console use built-in catalogs. The **Model Routes** page shows Provider-qualified routes, endpoint capabilities, and supporting-account counts; clients should treat the currently serviceable results from `GET /v1/models` as authoritative.

### Grok Build

Build does not use one global static model list. Account synchronization reads the upstream `/models` endpoint, and different accounts, subscription tiers, or staged rollouts may expose different models. Routing retains these per-account capabilities instead of replacing the global catalog with one account's response.

| Model | Type | Availability | Gateway surfaces |
| :-- | :-- | :-- | :-- |
| Conversation models returned by upstream `/models` (for example, `grok-4.5`) | Conversation | Returned by the selected account | Chat Completions, Responses, Messages, compact, stored responses |
| `grok-composer-2.5-fast` | Conversation | Grok Build OAuth accounts | Chat Completions, Responses, Messages; supplemented from the OAuth session contract when a sparse upstream catalog omits it |
| `grok-imagine-video-1.5` | Video | Super/paid Build accounts | Videos; not assigned to Free or unknown-entitlement accounts |

Conversation requests are translated to the Build Responses protocol while preserving the tool, reasoning, multi-turn, and prompt-cache compatibility required by Codex and Claude Code. Build currently exposes no image generation or image editing routes.

### Grok Web

Web uses a built-in catalog filtered by account tier; higher tiers inherit lower-tier models.

| Model | Type | Minimum tier | Gateway surfaces |
| :-- | :-- | :-- | :-- |
| `grok-chat-fast` | Conversation | Basic | Chat Completions, Responses, Messages |
| `grok-chat-auto` | Conversation | Super | Chat Completions, Responses, Messages |
| `grok-chat-expert` | Conversation | Super | Chat Completions, Responses, Messages |
| `grok-chat-heavy` | Conversation | Heavy | Chat Completions, Responses, Messages |
| `grok-imagine-image-lite` | Image | Basic | Images Generations |
| `grok-imagine-image-quality-lite` | Image | Basic | Images Generations |
| `grok-imagine-image-edit` | Image Edit | Super | Images Edits |
| `grok-imagine-video` | Video | Super | Videos |

### Grok Console

Console uses the catalog built into the current release. Conversation forwarding is stateless, while image, video, and voice use the standard xAI resource APIs.

| Model | Type | Gateway surfaces |
| :-- | :-- | :-- |
| `grok-4.20-0309-non-reasoning` | Conversation | Chat Completions, Responses, Messages |
| `grok-4.20-0309-reasoning` | Conversation | Chat Completions, Responses, Messages; the model reasons but the upstream rejects configurable `reasoningEffort` |
| `grok-4.20-multi-agent-0309` | Conversation | Chat Completions, Responses, Messages |
| `grok-4.5` | Conversation | Chat Completions, Responses, Messages |
| `grok-4.3` | Conversation | Chat Completions, Responses, Messages |
| `grok-build-0.1` | Conversation | Chat Completions, Responses, Messages |
| `grok-imagine-image` | Image, Image Edit | Images Generations, Images Edits |
| `grok-imagine-image-quality` | Image, Image Edit | Images Generations, Images Edits |
| `grok-imagine-image-2.0` | Image, Image Edit | Images Generations, Images Edits |
| `grok-imagine-video` | Video | Videos |
| `grok-imagine-video-1.5` | Video | Video generation, including Free Console accounts |
| `grok-voice-latest`, `grok-voice-think-fast-2.0`, `grok-voice-think-fast-1.0` | Voice | TTS and Realtime WebSocket proxy |
| `grok-stt` | Voice | STT and OpenAI-compatible audio transcriptions |

Generation and editing capabilities for the same Console image model are grouped into one logical model row; no separate `-edit` model copy is required.

Public names normally omit the Provider. Internally, routes use `Build/`, `Web/`, or `Console/`; qualified names can pin a request to one source.

Web can be weakly linked one-to-one with matching Build and Console accounts. Links share only an anonymous egress identity and provenance display. They never merge credentials, quota, health, cooldown, concurrency, capabilities, or billing.

### Codex, Claude Code, and prompt caching

Responses and Messages support streaming, tools, reasoning, multi-turn sessions, and compaction. Stable client session signals are preserved for Grok Build prompt-cache affinity. Cache hits still require a compatible upstream account and an unchanged prompt prefix.

Responses and Chat Completions report OpenAI-style total input. Messages reports Anthropic-style uncached input and cache reads separately. Audits retain total and cached input for billing reconciliation.

## API

Inference endpoints use a client key:

```http
Authorization: Bearer g2a_xxx_xxx
```

| Method | Path | Purpose |
| :-- | :-- | :-- |
| `GET` | `/healthz`, `/readyz` | Liveness and readiness |
| `GET` | `/v1/models` | Serviceable models |
| `POST` | `/v1/responses` | Responses JSON/SSE |
| `POST` | `/v1/responses/compact` | Compact a supported Response session |
| `GET`, `DELETE` | `/v1/responses/{id}` | Read or delete a stored response |
| `POST` | `/v1/chat/completions` | Chat Completions JSON/SSE |
| `POST` | `/v1/messages` | Anthropic Messages JSON/SSE |
| `POST` | `/v1/images/generations`, `/v1/images/edits` | Generate or edit images |
| `POST`, `GET` | `/v1/videos/*` | Create and inspect video jobs |
| `POST` | `/v1/tts`, `/v1/audio/speech`, `/v1/audio/tasks` | Synthesize speech |
| `POST` | `/v1/stt`, `/v1/audio/transcriptions` | Transcribe audio |
| `GET` | `/v1/stt`, `/v1/realtime` | Proxy voice WebSocket sessions |
| `GET` | `/v1/media/images/{asset_id}`, `/v1/media/videos/{asset_id}` | Read archived media |

Stored responses and compact depend on the selected Provider. The signed-in admin console provides live examples at `/docs`; Swagger is available only when `server.swaggerEnabled: true`.

`/v1/audio/transcriptions` supports `json` (default), `verbose_json`, and `text`. Video edit/extension routes must resolve to Console `grok-imagine-video`; custom public model names remain supported. Monetary billing is applied only when the gateway can reliably measure the official pricing unit: TTS is reserved and settled from its input character count, while REST and streaming STT are settled from the actual audio duration returned by a successful response. Because STT duration is known only after completion, concurrent requests may briefly take a billing-limited key beyond its spend limit. Realtime, video edits/extensions, and custom routes without a recognized official price are currently audited as unpriced; they remain callable and do not consume the spend limit.

Client keys support model allowlists and optional RPM, concurrency, spend, and expiry limits.

```bash
curl http://127.0.0.1:8000/v1/responses \
  -H "Authorization: Bearer g2a_xxx_xxx" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "your-model",
    "input": "Explain quantum tunneling in three sentences.",
    "stream": true
  }'
```

## Egress and Cloudflare

Egress nodes are scoped to Build, Web, Console, or Web assets. The admin console supports:

- HTTP, HTTPS, SOCKS4/4A, SOCKS5/5H, Resin, Trojan, VLESS, Shadowsocks, and VMess
- TCP, WebSocket, and TLS tunnel transports; unsupported variants are rejected during import
- Subscription and text/Base64 import
- Batch probes, filtering, deletion, assignment, and balancing
- Fallback per scope: none, direct, or a fixed node
- Proxy-pool mode without global cooldown after one connection failure
- Immediate recovery probes after fixed-proxy transport failures, with per-node coalescing and bounded waiting for fast retry
- Optional [Egress Quality Guard](./tools/egress-quality-guard/README.md) for active per-node model probes, guarded quarantine, and recovery; enable it with the built-in `quality-guard` Compose profile

Hysteria and TUIC are not supported yet. FlareSolverr accepts only HTTP/SOCKS proxy URLs, so automatic clearance refresh cannot use a tunnel share URL directly.

To enable the guard, add a `qualityGuard` section to `config.yaml`, then start
the profile. The main service creates and reuses a non-exportable system probe
identity automatically:

```yaml
qualityGuard:
  enabled: true
  model: "grok-4.5"
```

```bash
docker compose --profile quality-guard up -d --build
```

Existing preview deployments that still contain `clientKeyID` can upgrade
directly. The field is accepted for compatibility but ignored and can be
removed; any manually created probe key is intentionally left untouched.

After changing this configuration, run `docker compose --profile quality-guard restart grok2api egress-quality-guard` to reload the base settings; policy edits made in the admin page still hot-reload.

The normal `docker compose up -d` command does not start the guard or generate
probe traffic. The sidecar receives a narrowly scoped internal credential from
the main service and never stores or uses the administrator password. See the
linked guide before enabling automatic quarantine.

Resin usernames can contain `{account}`:

```text
socks5h://Default.{account}:RESIN_PROXY_TOKEN@resin:2260
```

The placeholder becomes a stable anonymous identity. Linked Web, Build, and Console accounts can share it; raw tokens and email addresses are not used.

For managed Web/Console Cloudflare Clearance:

```bash
docker compose --profile flaresolverr up -d
```

Then use `http://flaresolverr:8191` under **Runtime Settings → Media & Network → Clearance** and select one of the managed modes:

- `FlareSolverr` proactively refreshes stale fixed-egress Clearance on the configured schedule.
- `On demand` keeps the last successful Clearance regardless of age and solves again only after an upstream rejection explicitly invalidates it. Scheduled refresh does not launch a browser in this mode.

`Manual` never invokes FlareSolverr. The on-demand mode can make the first request without a managed Clearance; if Cloudflare rejects it, the next lease performs one deduplicated solve.

The egress layer retries only connection failures known to occur before request submission. It does not replay submitted generation requests, authentication failures, exhausted quotas, or upstream rate limits.

When a fixed proxy enters cooldown after a transport failure, grok2api starts an independent connectivity probe immediately. Concurrent failures share one probe. A later request bound to that node waits for at most five seconds, reloads persisted node state after a healthy probe, and continues without waiting for the full cooldown. An unhealthy probe preserves the cooldown. Proxy-pool leases use fresh tunnels, so one rotating exit failure never cools the whole pool. See [Immediate egress failure probe and bounded retry](./backend/internal/infra/egress/FAILURE_RETRY.md) for the design and safety invariants.

## Configuration and deployment

`config.yaml` contains startup settings; Provider and operational settings are managed in the admin console and hot-reload unless marked otherwise.

| Deployment | Database | Runtime store | Media |
| :-- | :-- | :-- | :-- |
| Single instance | SQLite | Memory | Local directory |
| Multiple instances | PostgreSQL | Redis | Shared read/write directory |

Multi-instance deployments require a unique `deployment.instanceID` per replica, one shared `clusterID`, and `sharedMedia: true` only after the media directory is shared correctly.

PostgreSQL credentials can be injected without storing them in `config.yaml`:

```bash
GROK2API_DATABASE_URL='postgresql://user:password@host:5432/grok2api?sslmode=require' docker compose up -d
```

A non-empty `GROK2API_DATABASE_URL` overrides `database.postgres.dsn` and automatically selects the `postgres` driver. An empty value is ignored. Supported URL schemes are `postgres://` and `postgresql://`; SQLAlchemy's `postgresql+asyncpg://` form is rejected with a migration hint. The application does not implicitly read the generic `DATABASE_URL`; platforms that provide it can map it explicitly with `GROK2API_DATABASE_URL: "${DATABASE_URL}"`. Database configuration precedence is built-in defaults, `config.yaml`, then `GROK2API_DATABASE_URL`. The current CLI has no database override.

Important optional settings:

- `audit.ledgerMode`: `observe` reports ledger faults; `enforce` can pause new inference to protect billing integrity.
- `routing.accountIsolatedConnections`: partitions outbound TCP/HTTP pools by account for external L4 or connection-hash load balancers. It is off by default because it increases connections, TLS handshakes, memory, and file-descriptor usage.
- `routing.segmentedSelectorEnabled`: enabled by default for pools with at least 3,000 eligible accounts; bounds dynamic concurrency reads while retaining quota/tier priorities, sticky sessions, full-planner fallback, and atomic guards.
- Build response-header timeout and exact-match 403 invalidation rules are hot-reloadable.
- **Sync latest version** applies the validated Grok Build client version and User-Agent.

## Production checklist

- Use HTTPS and enable `auth.secureCookies`.
- Keep Swagger disabled on public deployments.
- Use strong, backed-up secrets; never commit credentials, cookies, exports, or databases.
- Back up `config.yaml`, the database, and media storage.
- Use PostgreSQL, Redis, and shared media for multiple instances.
- Put a reverse proxy and access controls in front of public deployments.

## Development

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

Regenerate Swagger after changing public API annotations:

```bash
make swagger
```

## Documentation

- [简体中文 README](./README.zh-CN.md)
- [Backend guide](./backend/README.md)
- [Frontend guide](./frontend/README.md)
