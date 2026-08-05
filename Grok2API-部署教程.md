---

## title: "Grok2API 部署指南"
date: 2026-08-04
description: "分别使用 SQLite、PostgreSQL 和 PostgreSQL + Redis 部署 Grok2API，并按需接入 WARP、FlareSolverr 与 Egress Quality Guard。"
tags:
  - Grok2API
  - Docker Compose
  - PostgreSQL
  - Redis

# Grok2API 部署指南

Grok2API 是面向 Grok Build、Grok Web 和 Grok Console 的多账号 API 网关，提供 OpenAI Responses、Chat Completions 与 Anthropic Messages 兼容接口，并集成账号管理、模型路由、额度与并发控制、故障转移、审计、媒体归档和出口管理。

## 架构选型

本文不把所有组件堆进同一份配置，而是提供三套边界清晰的部署方案：


| 方案  | 适用场景         | 数据库        | 运行态存储  | 应用副本 |
| --- | ------------ | ---------- | ------ | ---- |
| 方案一 | 个人、测试、普通单机   | SQLite     | Memory | 1    |
| 方案二 | 单实例生产、托管数据库  | PostgreSQL | Memory | 1    |
| 方案三 | 同主机双实例或多实例验证 | PostgreSQL | Redis  | 2    |


建议从最符合当前需求的方案中选择一套，不要混用三套主配置。WARP、FlareSolverr 和 Egress Quality Guard 位于文章后半部分，作为独立 Compose 扩展叠加到主方案上。

项目地址：[https://github.com/chenyme/grok2api](https://github.com/chenyme/grok2api)

> 请仅接入已获授权使用的账号与网络资源，并遵守相关服务条款、当地法律及组织安全规范。



## 准备工作

以下三套方案均要求：

- Linux `amd64` 或 `arm64` 主机；
- Docker Engine；
- Docker Compose v2，即使用 `docker compose`；
- 可以访问 Grok2API 镜像仓库及所需上游服务的网络。

确认运行环境：

```bash
docker version
docker compose version
```

下载项目：

```bash
git clone https://github.com/chenyme/grok2api.git
cd grok2api
```

生成应用密钥：

```bash
openssl rand -hex 32
openssl rand -base64 32
```

第一条命令的输出用于 `secrets.jwtSecret`，第二条命令的输出用于 `secrets.credentialEncryptionKey`。这两个值必须妥善保存，其中 `credentialEncryptionKey` 一旦用于写入账号凭据，就不能随意更换，否则已有凭据将无法解密。

---



## SQLite 单实例

这是依赖最少的基线方案，适合个人使用、功能验证以及普通单实例部署。数据库和本地媒体都保存在 Docker 数据卷中。

### 文件布局

```text
grok2api/
├── docker-compose.yml
└── config.yaml
```



### Compose 配置

```yaml
name: grok2api

services:
  grok2api:
    image: "${GROK2API_IMAGE:-ghcr.io/chenyme/grok2api:latest}"
    ports:
      - "${GROK2API_PORT:-8000}:8000"
    environment:
      TZ: "${TZ:-Asia/Shanghai}"
    volumes:
      - "./config.yaml:/run/grok2api/config.yaml:ro"
      - grok2api-data:/app/data
    restart: unless-stopped
    init: true
    stop_grace_period: 30s
    security_opt:
      - no-new-privileges:true

volumes:
  grok2api-data:
```



### 应用配置

```yaml
server:
  swaggerEnabled: false

auth:
  # 直接使用 HTTP 测试时保持 false；启用 HTTPS 后改为 true。
  secureCookies: false

secrets:
  jwtSecret: "替换为 openssl rand -hex 32 的输出"
  credentialEncryptionKey: "替换为 openssl rand -base64 32 的输出"

bootstrapAdmin:
  username: "admin"
  password: "替换为高强度初始密码"

database:
  driver: sqlite
  sqlite:
    path: "./data/backend.db"

runtimeStore:
  driver: memory

deployment:
  replicas: 1
  clusterID: "grok2api"

media:
  driver: local
  local:
    path: "./data/media"
```

未在该文件中列出的启动项使用项目内置默认值。需要进一步调整时，以仓库中的 `config.example.yaml` 为准。

### 启动验证

```bash
chmod 600 config.yaml
docker compose pull
docker compose up -d
docker compose ps
docker compose logs --tail=200 grok2api
```

健康检查：

```bash
curl -fsS http://127.0.0.1:8000/healthz
curl -fsS http://127.0.0.1:8000/readyz
```

默认访问地址为 `http://服务器地址:8000`。如需更换宿主机端口，可在 `.env` 中设置：

```dotenv
GROK2API_PORT=18000
```

方案一的数据位置：

- SQLite：卷内 `/app/data/backend.db`；
- 媒体：卷内 `/app/data/media`；
- 宿主机配置：`./config.yaml`。

---



## PostgreSQL 单实例

该方案仍运行一个 Grok2API 副本，但把业务数据迁移到 PostgreSQL。适合托管数据库、PaaS 或希望独立管理数据库备份的部署。

示例 Compose 同时启动 PostgreSQL；如果使用云数据库，可以删除 `postgres` 服务与 `postgres-data` 卷，并把 `GROK2API_DATABASE_URL` 改为云数据库地址。

### 文件布局

```text
grok2api/
├── .env
├── docker-compose.yml
└── config.yaml
```



### 环境变量

```dotenv
POSTGRES_DB=grok2api
POSTGRES_USER=grok2api
POSTGRES_PASSWORD=替换为高强度数据库密码

# 密码包含特殊字符时，这里的用户名和密码必须进行 URL 编码。
GROK2API_DATABASE_URL=postgresql://grok2api:替换为URL编码后的密码@postgres:5432/grok2api?sslmode=disable
```

`POSTGRES_PASSWORD` 是 PostgreSQL 容器使用的原始密码；URL 中的密码是同一个值经过 URL 编码后的结果。不要为两者配置不同密码。

### Compose 配置

```yaml
name: grok2api

services:
  postgres:
    image: "${POSTGRES_IMAGE:-postgres:16-alpine}"
    environment:
      POSTGRES_DB: "${POSTGRES_DB:-grok2api}"
      POSTGRES_USER: "${POSTGRES_USER:-grok2api}"
      POSTGRES_PASSWORD: "${POSTGRES_PASSWORD:?set POSTGRES_PASSWORD in .env}"
    volumes:
      - postgres-data:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U \"$${POSTGRES_USER}\" -d \"$${POSTGRES_DB}\""]
      interval: 10s
      timeout: 5s
      retries: 10
    restart: unless-stopped
    security_opt:
      - no-new-privileges:true

  grok2api:
    image: "${GROK2API_IMAGE:-ghcr.io/chenyme/grok2api:latest}"
    depends_on:
      postgres:
        condition: service_healthy
    ports:
      - "${GROK2API_PORT:-8000}:8000"
    environment:
      TZ: "${TZ:-Asia/Shanghai}"
      GROK2API_DATABASE_URL: "${GROK2API_DATABASE_URL:?set GROK2API_DATABASE_URL in .env}"
    volumes:
      - "./config.yaml:/run/grok2api/config.yaml:ro"
      - app-data:/app/data
    restart: unless-stopped
    init: true
    stop_grace_period: 30s
    security_opt:
      - no-new-privileges:true

volumes:
  postgres-data:
  app-data:
```

PostgreSQL 未映射宿主机端口，只允许 Compose 内网中的服务访问。如果使用外部管理工具，应通过受控网络、SSH Tunnel 或数据库平台提供的安全入口连接，而不是直接开放 `5432` 到公网。

### 应用配置

```yaml
server:
  swaggerEnabled: false

auth:
  secureCookies: false

secrets:
  jwtSecret: "替换为 openssl rand -hex 32 的输出"
  credentialEncryptionKey: "替换为 openssl rand -base64 32 的输出"

bootstrapAdmin:
  username: "admin"
  password: "替换为高强度初始密码"

database:
  driver: postgres
  postgres:
    # 实际运行时由 GROK2API_DATABASE_URL 覆盖。
    dsn: "postgresql://grok2api@postgres:5432/grok2api?sslmode=disable"
    maxOpenConns: 50
    maxIdleConns: 10

runtimeStore:
  driver: memory

deployment:
  replicas: 1
  clusterID: "grok2api"

media:
  driver: local
  local:
    path: "./data/media"
```

数据库配置优先级为：

```text
内置默认值 < config.yaml < GROK2API_DATABASE_URL < CLI 覆盖
```

当前 CLI 没有数据库参数，因此非空的 `GROK2API_DATABASE_URL` 是数据库配置的最高优先级。它会覆盖 `database.postgres.dsn`，并自动选择 PostgreSQL 驱动。应用不会隐式读取通用的 `DATABASE_URL`；PaaS 只提供该变量时，需要显式映射为 `GROK2API_DATABASE_URL`。

### 启动验证

```bash
chmod 600 .env config.yaml
docker compose config --quiet
docker compose pull
docker compose up -d
docker compose ps
docker compose logs --tail=200 postgres grok2api
```

```bash
curl -fsS http://127.0.0.1:8000/healthz
curl -fsS http://127.0.0.1:8000/readyz
```

方案二中，PostgreSQL 数据位于 `postgres-data` 卷，本地媒体位于 `app-data` 卷。两者必须分别备份。

---



## PostgreSQL + Redis 双实例

该方案用于验证 Grok2API 的多实例约束，或在同一台主机上运行两个应用副本。它不是跨主机高可用方案：PostgreSQL、Redis、媒体卷和 Docker 主机仍可能是单点。

真正的跨主机部署必须使用外部 PostgreSQL、外部 Redis、多个主机都能访问的共享媒体存储，以及独立负载均衡器。

### 部署约束

Grok2API 的每个副本必须满足：

- 使用同一个 PostgreSQL 数据库；
- 使用同一个 Redis；
- 使用相同的 `jwtSecret` 和 `credentialEncryptionKey`；
- 使用相同的 `deployment.clusterID`；
- 使用不同且稳定的 `deployment.instanceID`；
- `deployment.replicas` 设置为实际副本数；
- `deployment.sharedMedia` 设置为 `true`；
- 挂载同一个可读写媒体目录。

因此不能让两个副本共用完全相同的 `config.yaml`，也不能直接执行 `docker compose up --scale grok2api=2`。本方案使用 `config.node-1.yaml` 和 `config.node-2.yaml`，两者仅在实例身份和首次管理员配置上存在差异。

### 文件布局

```text
grok2api/
├── .env
├── docker-compose.yml
├── config.node-1.yaml
└── config.node-2.yaml
```



### 环境变量

```dotenv
POSTGRES_DB=grok2api
POSTGRES_USER=grok2api
POSTGRES_PASSWORD=替换为高强度数据库密码
GROK2API_DATABASE_URL=postgresql://grok2api:替换为URL编码后的密码@postgres:5432/grok2api?sslmode=disable

# 该值还需要原样写入两个节点配置的 runtimeStore.redis.password。
REDIS_PASSWORD=替换为高强度Redis密码
```



### Compose 配置

```yaml
name: grok2api

x-grok2api-common: &grok2api-common
  image: "${GROK2API_IMAGE:-ghcr.io/chenyme/grok2api:latest}"
  depends_on:
    postgres:
      condition: service_healthy
    redis:
      condition: service_healthy
  environment:
    TZ: "${TZ:-Asia/Shanghai}"
    GROK2API_DATABASE_URL: "${GROK2API_DATABASE_URL:?set GROK2API_DATABASE_URL in .env}"
  volumes:
    - app-data:/app/data
  restart: unless-stopped
  init: true
  stop_grace_period: 30s
  security_opt:
    - no-new-privileges:true

services:
  postgres:
    image: "${POSTGRES_IMAGE:-postgres:16-alpine}"
    environment:
      POSTGRES_DB: "${POSTGRES_DB:-grok2api}"
      POSTGRES_USER: "${POSTGRES_USER:-grok2api}"
      POSTGRES_PASSWORD: "${POSTGRES_PASSWORD:?set POSTGRES_PASSWORD in .env}"
    volumes:
      - postgres-data:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U \"$${POSTGRES_USER}\" -d \"$${POSTGRES_DB}\""]
      interval: 10s
      timeout: 5s
      retries: 10
    restart: unless-stopped
    security_opt:
      - no-new-privileges:true

  redis:
    image: "${REDIS_IMAGE:-redis:7-alpine}"
    environment:
      REDIS_PASSWORD: "${REDIS_PASSWORD:?set REDIS_PASSWORD in .env}"
    command:
      - sh
      - -c
      - 'exec redis-server --appendonly yes --requirepass "$${REDIS_PASSWORD}"'
    volumes:
      - redis-data:/data
    healthcheck:
      test: ["CMD-SHELL", "redis-cli -a \"$${REDIS_PASSWORD}\" ping | grep -q PONG"]
      interval: 10s
      timeout: 5s
      retries: 10
    restart: unless-stopped
    security_opt:
      - no-new-privileges:true

  grok2api:
    <<: *grok2api-common
    ports:
      - "127.0.0.1:8001:8000"
    volumes:
      - "./config.node-1.yaml:/run/grok2api/config.yaml:ro"
      - app-data:/app/data

  grok2api-2:
    <<: *grok2api-common
    ports:
      - "127.0.0.1:8002:8000"
    volumes:
      - "./config.node-2.yaml:/run/grok2api/config.yaml:ro"
      - app-data:/app/data

volumes:
  postgres-data:
  redis-data:
  app-data:
```



### 节点 1 配置

```yaml
server:
  swaggerEnabled: false

auth:
  secureCookies: false

secrets:
  jwtSecret: "两个节点完全相同的 JWT 密钥"
  credentialEncryptionKey: "两个节点完全相同的 Base64 加密密钥"

bootstrapAdmin:
  username: "admin"
  password: "替换为高强度初始密码"

database:
  driver: postgres
  postgres:
    dsn: "postgresql://grok2api@postgres:5432/grok2api?sslmode=disable"
    maxOpenConns: 25
    maxIdleConns: 5

runtimeStore:
  driver: redis
  redis:
    address: "redis:6379"
    username: ""
    password: "必须与 .env 中 REDIS_PASSWORD 完全相同"
    database: 0
    keyPrefix: "grok2api:"
    tls: false

deployment:
  replicas: 2
  instanceID: "grok2api-node-1"
  clusterID: "grok2api-production"
  sharedMedia: true

media:
  driver: local
  local:
    path: "./data/media"
```



### 节点 2 配置

```yaml
server:
  swaggerEnabled: false

auth:
  secureCookies: false

secrets:
  jwtSecret: "与 node-1 完全相同"
  credentialEncryptionKey: "与 node-1 完全相同"

# 第二个节点不负责首次创建管理员，因此不配置 bootstrapAdmin。

database:
  driver: postgres
  postgres:
    dsn: "postgresql://grok2api@postgres:5432/grok2api?sslmode=disable"
    maxOpenConns: 25
    maxIdleConns: 5

runtimeStore:
  driver: redis
  redis:
    address: "redis:6379"
    username: ""
    password: "必须与 node-1 和 .env 中 REDIS_PASSWORD 完全相同"
    database: 0
    keyPrefix: "grok2api:"
    tls: false

deployment:
  replicas: 2
  instanceID: "grok2api-node-2"
  clusterID: "grok2api-production"
  sharedMedia: true

media:
  driver: local
  local:
    path: "./data/media"
```

两个配置文件中的 `jwtSecret`、`credentialEncryptionKey`、Redis 密码和 `clusterID` 必须一致；只有 `instanceID` 必须不同。

### 启动验证

先启动基础设施和第一个节点，由第一个节点完成数据库迁移与管理员初始化：

```bash
chmod 600 .env config.node-1.yaml config.node-2.yaml
docker compose config --quiet
docker compose up -d postgres redis grok2api
curl -fsS http://127.0.0.1:8001/readyz
```

第一个节点就绪后再启动第二个节点：

```bash
docker compose up -d grok2api-2
curl -fsS http://127.0.0.1:8002/readyz
```

随后由外部 Nginx、Nginx Proxy Manager、HAProxy 或云负载均衡器把流量分发到：

```text
127.0.0.1:8001
127.0.0.1:8002
```

Redis 负责共享并发租约、Sticky Session、限流及失效通知等运行态；PostgreSQL 保存业务数据；`app-data` 卷作为同主机共享媒体目录。

---



## 应用初始化

主服务就绪后按以下顺序初始化：

1. 使用 `bootstrapAdmin` 登录管理端；
2. 修改管理员密码；
3. 删除首个节点配置中的 `bootstrapAdmin`，并重启对应服务；
4. 接入 Grok Build、Grok Web 或 Grok Console 账号；
5. 等待账号额度和模型能力同步；
6. 在“模型路由”中确认公开模型；
7. 在“客户端密钥”中创建调用密钥，并配置模型白名单、RPM、并发和用量限制。

列出模型：

```bash
curl -fsS http://127.0.0.1:8000/v1/models \
  -H "Authorization: Bearer g2a_xxx_xxx"
```

方案三应把端口改为 `8001`、`8002` 或负载均衡入口。

发送 Responses API 请求：

```bash
curl http://127.0.0.1:8000/v1/responses \
  -H "Authorization: Bearer g2a_xxx_xxx" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "替换为 /v1/models 返回的模型 ID",
    "input": "用三句话解释量子隧穿。",
    "stream": true
  }'
```

推理接口使用管理端创建的客户端密钥，不使用管理员登录令牌。

---



## 可选扩展



### WARP

WARP 适用于需要额外落地网络的场景。它需要 `NET_ADMIN` 能力，不应在没有明确网络需求时默认启用。

新建 `compose.warp.yml`：

```yaml
services:
  warp:
    image: caomingjun/warp:latest
    profiles: ["warp"]
    restart: unless-stopped
    environment:
      WARP_SLEEP: "2"
    cap_add:
      - NET_ADMIN
```

启动时同时加载主 Compose 与扩展文件：

```bash
docker compose \
  -f docker-compose.yml \
  -f compose.warp.yml \
  --profile warp up -d
```

然后在管理端“设置 → 出口代理”中新增：

```text
socks5://warp:1080
```

Compose 服务位于同一默认网络时，应使用服务名 `warp`，而不是 `127.0.0.1`。

---



### FlareSolverr

FlareSolverr 用于 Grok Web 的 Cloudflare Clearance 管理。没有 Grok Web Clearance 需求时无需部署。

新建 `compose.flaresolverr.yml`：

```yaml
services:
  flaresolverr:
    image: ghcr.io/flaresolverr/flaresolverr:latest
    profiles: ["flaresolverr"]
    environment:
      TZ: "${TZ:-Asia/Shanghai}"
      LOG_LEVEL: info
    restart: unless-stopped
```

启动：

```bash
docker compose \
  -f docker-compose.yml \
  -f compose.flaresolverr.yml \
  --profile flaresolverr up -d
```

在管理端将 Clearance 模式设置为 `FlareSolverr`，地址填写：

```text
http://flaresolverr:8191
```

不需要把 `8191` 映射到公网。

---



### Egress Quality Guard

Egress Quality Guard 是 Grok Build 出口质量守护 sidecar，支持主动/被动质量检测、隔离和健康恢复。普通 Grok2API 推理不依赖该服务。

#### 应用配置

方案一或方案二直接修改 `config.yaml`；方案三需在 `config.node-1.yaml` 和 `config.node-2.yaml` 中写入相同的配置：

```yaml
qualityGuard:
  enabled: true
  model: "grok-4.5"
  mode: hybrid
  activeInterval: 30m
  passivePollInterval: 5s
  softTPS: 500
  hardTPS: 1000
  consecutiveSoft: 2
  consecutiveErrors: 2
  quarantineDuration: 5m
  noAccountBackoff: 5m
  minimumHealthyNodes: 3
  failClosed: false
  nodeIDs: []
```

`nodeIDs` 为空时管理全部符合条件的 Grok Build 节点。阈值应先结合真实流量观察，再决定是否启用严格隔离。

#### Compose 配置

三套方案中的主服务都命名为 `grok2api`，因此可以共用以下扩展：

```yaml
services:
  grok2api:
    environment:
      GROK2API_QUALITY_GUARD_DIR: /var/lib/grok2api-quality-guard
    volumes:
      - quality_guard_state:/var/lib/grok2api-quality-guard

  egress-quality-guard:
    profiles: ["quality-guard"]
    build:
      context: .
      dockerfile: tools/egress-quality-guard/Dockerfile
    depends_on:
      grok2api:
        condition: service_healthy
    volumes:
      - quality_guard_state:/var/lib/grok2api-quality-guard
    restart: "on-failure:5"
    init: true
    stop_grace_period: 30s
    security_opt:
      - no-new-privileges:true

volumes:
  quality_guard_state:
```

方案三还应让第二个应用实例挂载同一个私有状态卷。在上述 `compose.quality-guard.yml` 的 `services` 中追加：

```yaml
  grok2api-2:
    environment:
      GROK2API_QUALITY_GUARD_DIR: /var/lib/grok2api-quality-guard
    volumes:
      - quality_guard_state:/var/lib/grok2api-quality-guard
```

只启动一个 `egress-quality-guard` sidecar 即可。它固定通过 Compose 内网访问 `grok2api:8000`；第二个应用实例挂载状态卷，是为了让管理请求经负载均衡命中任意实例时，都能读取相同的守护状态与热更新策略。

启动：

```bash
docker compose \
  -f docker-compose.yml \
  -f compose.quality-guard.yml \
  --profile quality-guard up -d --build
```

主程序会自动创建不可导出的内部探测身份，并把受限凭据写入私有共享卷；无需配置 Client Key ID。修改 `qualityGuard` 启动配置后，应重启主服务和 sidecar：

```bash
docker compose \
  -f docker-compose.yml \
  -f compose.quality-guard.yml \
  --profile quality-guard restart grok2api egress-quality-guard
```

方案三同时修改了两个节点的启动配置时，将上述命令的服务列表改为 `grok2api grok2api-2 egress-quality-guard`。管理界面保存的运行策略会通过共享状态卷热加载，不需要重启。

只停止守护程序不会影响主 API：

```bash
docker compose \
  -f docker-compose.yml \
  -f compose.quality-guard.yml \
  --profile quality-guard stop egress-quality-guard
```

---



### 组合启用

Compose 扩展可以叠加。例如同时启用 WARP、FlareSolverr 和 Quality Guard：

```bash
docker compose \
  -f docker-compose.yml \
  -f compose.warp.yml \
  -f compose.flaresolverr.yml \
  -f compose.quality-guard.yml \
  --profile warp \
  --profile flaresolverr \
  --profile quality-guard \
  up -d --build
```

扩展文件只增加对应服务，不改变主数据库方案。排查问题时可逐个移除扩展文件，以区分主程序故障和扩展组件故障。

---



## 反向代理

生产环境应通过 Nginx、Caddy、Traefik、Nginx Proxy Manager 或云负载均衡器终止 TLS。启用 HTTPS 后，将应用配置调整为：

```yaml
auth:
  secureCookies: true

frontend:
  publicApiBaseURL: "https://grok.example.com"
```

单实例 Nginx 核心配置：

```nginx
location / {
    proxy_pass http://127.0.0.1:8000;
    proxy_http_version 1.1;

    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;

    proxy_buffering off;
    proxy_cache off;
    proxy_read_timeout 3600s;
    proxy_send_timeout 3600s;
}
```

方案三可以定义双节点 upstream：

```nginx
upstream grok2api_backend {
    server 127.0.0.1:8001;
    server 127.0.0.1:8002;
    keepalive 32;
}

location / {
    proxy_pass http://grok2api_backend;
    proxy_http_version 1.1;

    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;

    proxy_buffering off;
    proxy_cache off;
    proxy_read_timeout 3600s;
    proxy_send_timeout 3600s;
}
```

关闭代理缓冲对于 Responses、Chat 和 Messages 的 SSE 流式输出非常重要。反向代理与 Grok2API 不在同一主机时，应使用受控内网地址或 Docker 网络服务名，不能使用远端主机的 `127.0.0.1`。

---



## 运维管理



### 升级

```bash
git pull --ff-only
docker compose pull
docker compose up -d
docker compose ps
docker compose logs --tail=200 grok2api
```

生产环境建议通过 `.env` 固定已验证镜像版本：

```dotenv
GROK2API_IMAGE=ghcr.io/chenyme/grok2api:替换为已验证版本
```

不要在升级时执行 `docker compose down -v`，其中 `-v` 会删除 Compose 管理的数据卷。

### SQLite 备份

方案一需要备份 `config.yaml` 和 `grok2api_grok2api-data` 卷。为获得一致的 SQLite 备份，可以短暂停止主服务：

```bash
mkdir -p backups
docker compose stop grok2api
docker run --rm \
  -v grok2api_grok2api-data:/data:ro \
  -v "$PWD/backups:/backup" \
  alpine:3.23 \
  tar -czf /backup/grok2api-data.tar.gz -C /data .
docker compose start grok2api
```



### PostgreSQL 与多实例备份

PostgreSQL 应使用数据库平台快照、PITR 或与服务器版本匹配的 `pg_dump`。本地媒体卷仍需单独备份。方案三还应保留两个节点配置及 Redis 配置；Redis 主要保存运行态，不能替代 PostgreSQL 数据备份。

任何回滚都应同时考虑应用镜像和数据库 Schema。旧镜像不代表数据库可以自动降级，重大升级前应在独立环境验证恢复流程。

---



## 故障排查



### 配置文件未挂载

```text
missing config: /run/grok2api/config.yaml
```

检查 Compose 挂载路径及文件是否存在：

```bash
ls -l config*.yaml
docker compose config
```



### 就绪检查失败

进程存活不代表依赖已经就绪。检查数据库、Redis 和启动迁移日志：

```bash
docker compose logs --tail=300 grok2api postgres redis
```

方案中没有对应服务时，从命令中移除其名称。

### PostgreSQL 连接失败

检查：

- URL 是否使用 `postgres://` 或 `postgresql://`；
- 是否误用了 SQLAlchemy 的 `postgresql+asyncpg://`；
- DSN 中的密码是否正确 URL 编码；
- 数据库域名、端口、安全组、IP 白名单及 `sslmode` 是否正确；
- “副本数 × maxOpenConns”是否超过数据库连接上限。

Grok2API 会对连接错误中的完整 DSN 和密码进行脱敏。

### 多实例校验失败

重点核对：

- `database.driver=postgres`；
- `runtimeStore.driver=redis`；
- 两个节点的 `instanceID` 不同；
- `clusterID` 和应用密钥相同；
- `sharedMedia=true`；
- 两个容器挂载同一个媒体卷。



### SSE 响应被缓冲

确认反向代理已关闭 `proxy_buffering` 与缓存，并设置足够长的读取超时。

### 模型列表为空

检查账号是否启用、凭据是否有效、模型能力同步是否完成，以及管理端是否存在可用模型路由。Build 模型按账号能力动态发现，不应依赖静态模型清单。

---



## 生产检查清单

- [ ] 所有示例密钥和密码均已替换；
- [ ] 首次登录后已删除 `bootstrapAdmin`；
- [ ] 已离线备份且固定 `credentialEncryptionKey`；
- [ ] 配置文件与 `.env` 权限为 `0600`；
- [ ] PostgreSQL、Redis 和代理端口未直接暴露到公网；
- [ ] 公网入口已启用 HTTPS 和 `auth.secureCookies`；
- [ ] 反向代理已关闭 SSE 缓冲；
- [ ] 客户端密钥已配置模型白名单、RPM、并发和用量限制；
- [ ] 数据库、媒体目录和配置文件均有可恢复备份；
- [ ] `/healthz` 与 `/readyz` 已纳入监控；
- [ ] 多实例的 `instanceID` 唯一，`clusterID` 与应用密钥一致；
- [ ] WARP、FlareSolverr 和 Quality Guard 仅在存在明确需求时启用。



## 结语

三套方案的核心区别不是组件数量，而是状态边界：SQLite 适合单实例基线，PostgreSQL 适合独立数据库管理，PostgreSQL + Redis + 共享媒体才满足多实例前提。出口扩展应作为独立 Compose 层按需叠加，不应与数据库架构绑定。

按照这种拆分方式，部署文件可以直接复用，故障定位也更清晰：先验证主方案，再逐项启用网络和质量治理扩展。