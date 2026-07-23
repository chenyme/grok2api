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
<td valign="middle">Thanks to Krill AI for sponsoring this project! Krill provides fast and stable official API access to GPT, Claude, Gemini, and a wide range of Chinese models, with enterprise customization, invoicing, and dedicated 7×16-hour technical support. Its specially adapted WebSocket connection delivers fast time to first token. Register through <a href="https://www.krill-ai.com/register?invite=KJ2VGIRVAE">this link</a>, enter the coupon code “grok2api” when ordering, and receive 23% off your first Codex package.</td>
</tr>
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
| Egress | HTTP/SOCKS/Resin, subscriptions, probes, proxy pools, allocation, fallback, and FlareSolverr |
| Operations | Dashboard, model routes, client keys, audits, runtime settings, and media libraries |

### Provider boundaries

| Provider | Authentication | Models | Main capabilities |
| :-- | :-- | :-- | :-- |
| Grok Build | OAuth / Device OAuth | Discovered per account | Responses, Chat, Messages, compact, stored responses, video |
| Grok Web | SSO | Built-in, filtered by tier | Responses, Chat, Messages, images, image editing, video |
| Grok Console | SSO | Built-in | Stateless Responses, Chat, Messages |

Each Provider keeps its own credentials, quota, health, cooldown, concurrency, and model capabilities. Failover stays within the selected Provider.

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

Build models are discovered from account capabilities. Web and Console use built-in catalogs. Use the model page or `GET /v1/models` as the source of truth; the README does not maintain a static model list.

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
| `GET` | `/v1/media/images/{asset_id}`, `/v1/media/videos/{asset_id}` | Read archived media |

Stored responses and compact depend on the selected Provider. The signed-in admin console provides live examples at `/docs`; Swagger is available only when `server.swaggerEnabled: true`.

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

- HTTP, HTTPS, SOCKS4/4A, SOCKS5/5H, and Resin
- Subscription and text/Base64 import
- Batch probes, filtering, deletion, assignment, and balancing
- Fallback per scope: none, direct, or a fixed node
- Proxy-pool mode without global cooldown after one connection failure

Resin usernames can contain `{account}`:

```text
socks5h://Default.{account}:RESIN_PROXY_TOKEN@resin:2260
```

The placeholder becomes a stable anonymous identity. Linked Web, Build, and Console accounts can share it; raw tokens and email addresses are not used.

For managed Web/Console Cloudflare Clearance:

```bash
docker compose --profile flaresolverr up -d
```

Then select `FlareSolverr` under **Runtime Settings → Media & Network → Clearance** and use `http://flaresolverr:8191`.

The egress layer retries only connection failures known to occur before request submission. It does not replay submitted generation requests, authentication failures, exhausted quotas, or upstream rate limits.

## Configuration and deployment

`config.yaml` contains startup settings; Provider and operational settings are managed in the admin console and hot-reload unless marked otherwise.

| Deployment | Database | Runtime store | Media |
| :-- | :-- | :-- | :-- |
| Single instance | SQLite | Memory | Local directory |
| Multiple instances | PostgreSQL | Redis | Shared read/write directory |

Multi-instance deployments require a unique `deployment.instanceID` per replica, one shared `clusterID`, and `sharedMedia: true` only after the media directory is shared correctly.

Important optional settings:

- `audit.ledgerMode`: `observe` reports ledger faults; `enforce` can pause new inference to protect billing integrity.
- `routing.segmentedSelectorEnabled`: optimizes large account pools while retaining full-planner fallback and atomic guards.
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
