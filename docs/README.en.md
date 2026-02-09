# Grok2API

[中文](../README.md) | **English**

> [!NOTE]
> This project is for learning and research only. You must comply with Grok's Terms of Use and applicable laws. Do not use it for illegal purposes.

Grok2API rebuilt with **FastAPI**, fully aligned with the latest web call format. Supports streaming and non-streaming chat, image generation/editing, video generation, deep thinking, token pool concurrency, and automatic load balancing.

## Acknowledgments

This project is based on the following excellent projects:

- [@Chenyme/grok2api](https://github.com/chenyme/grok2api) - Original project author
- [@Tomiya233/grok2api](https://github.com/Tomiya233/grok2api) - Enhanced version

<br>

## Highlights

- **OpenAI-Compatible API** — Chat, image, video all via `/v1/chat/completions`, compatible with all OpenAI SDK clients
- **Multi-Backend Storage** — Local / Redis / MySQL / PostgreSQL, Redis recommended for production
- **Redis Atomic Operations** — Lua scripts for quota consumption, stats update, and cooling transition in a single RTT, zero race conditions
- **Dual Token Pool** — ssoBasic (80 req/20h) + ssoSuper (140 req/2h) auto-switching with multi-strategy load balancing
- **Security Middleware** — IP rate limiting, request body size limits, CORS whitelist, configurable auth policies
- **Imagine Waterfall** — WebSocket + SSE dual-mode real-time image generation with auto-fallback
- **Stats Dashboard** — Request statistics, log audit, proxy status monitoring
- **Proxy Pool** — Configurable proxy URL or proxy pool API with auto-rotation
- **Cache Management** — Local image/video cache with threshold-based auto-cleanup

<br>

## Quick Start

### Docker Compose (Recommended)

> [!TIP]
> Docker Compose + Redis is the recommended deployment. Out-of-the-box multi-worker concurrency with atomic operation optimization.

```bash
git clone https://github.com/CPU-JIA/grok2api
cd grok2api

# Modify environment variables in docker-compose.yml as needed
docker compose up -d
```

Default: 8 workers + Redis storage. In production, change `app.app_password` in `data/config.toml`.

### Local Development

```bash
uv sync
uv run main.py
```

### Admin Panel

URL: `http://<host>:8000/admin`
Default credentials: `admin` / `CHANGE_ME_NOW` (config keys `app.app_username` and `app.app_password`, change immediately in production).

**Modules**:

- **Token Management**: Import/add/delete tokens, view status and quota, filter by status or NSFW
- **Key Management**: Manage API keys, multi-key distribution
- **Stats Dashboard**: Request statistics, log audit, proxy status monitoring
- **Cache Preview**: View and clean local media cache (images/videos)
- **Configuration**: Online config editing, instant effect
- **Imagine Waterfall**: WebSocket/SSE real-time image generation with batch download
- **Voice Live**: LiveKit voice session, connect to Grok Voice

**Batch Operations**: Batch refresh, export, delete tokens; one-click NSFW (Unhinged mode)

<details>
<summary>Screenshots</summary>

<br>

**Login**

![Login](../Images/登录.png)

**Token Management**

![Token Management](../Images/Token管理.png)

**Imagine Waterfall**

![Imagine](../Images/Imagine.png)

**Voice Live**

![Voice](../Images/Voice.png)

**Key Management**

![Key Management](../Images/Key管理.png)

**Stats Dashboard — Request Statistics**

![Stats - Requests](../Images/统计监控-请求统计.png)

**Stats Dashboard — Log Audit**

![Stats - Logs](../Images/请求监控-日志统计.png)

**Cache Preview**

![Cache Preview](../Images/缓存预览.png)

**Configuration — Global & Grok Settings**

![Config 1](../Images/配置管理1.png)

**Configuration — Token Pool & Performance**

![Config 2](../Images/配置管理2.png)

</details>

### Environment Variables

> Configure via `.env` file or `docker-compose.yml` `environment` section

| Variable              | Description                                    | Default   | Example                |
| :-------------------- | :--------------------------------------------- | :-------- | :--------------------- |
| `LOG_LEVEL`           | Log level                                      | `INFO`    | `DEBUG`                |
| `SERVER_HOST`         | Bind address                                   | `0.0.0.0` | `0.0.0.0`              |
| `SERVER_PORT`         | Service port                                   | `8000`    | `8000`                 |
| `SERVER_WORKERS`      | Uvicorn worker count                           | `1`       | `4`                    |
| `SERVER_STORAGE_TYPE` | Storage type (`local`/`redis`/`mysql`/`pgsql`) | `local`   | `redis`                |
| `SERVER_STORAGE_URL`  | Storage URL (empty for local)                  | `""`      | `redis://redis:6379/0` |
| `DATA_DIR`            | Data directory path                            | `./data`  | `/app/data`            |

> **Storage URL examples**:
>
> - Redis: `redis://redis:6379/0`
> - MySQL: `mysql+aiomysql://user:password@host:3306/db` (if you set `mysql://`, it will be normalized to `mysql+aiomysql://`)
> - PostgreSQL: `postgresql+asyncpg://user:password@host:5432/db`

### Usage Limits

- Basic account: 80 requests / 20h
- Super account: 140 requests / 2h

### Models

| Model                    | Cost | Account     | Chat | Image | Video |
| :----------------------- | :--: | :---------- | :--: | :---: | :---: |
| `grok-3`                 |  1   | Basic/Super | Yes  |  Yes  |   -   |
| `grok-3-mini`            |  1   | Basic/Super | Yes  |  Yes  |   -   |
| `grok-3-thinking`        |  1   | Basic/Super | Yes  |  Yes  |   -   |
| `grok-4`                 |  1   | Basic/Super | Yes  |  Yes  |   -   |
| `grok-4-fast`            |  1   | Basic/Super | Yes  |  Yes  |   -   |
| `grok-4-mini`            |  1   | Basic/Super | Yes  |  Yes  |   -   |
| `grok-4-thinking`        |  1   | Basic/Super | Yes  |  Yes  |   -   |
| `grok-4-expert`          |  4   | Basic/Super | Yes  |  Yes  |   -   |
| `grok-4-heavy`           |  4   | Super       | Yes  |  Yes  |   -   |
| `grok-4.1`               |  1   | Basic/Super | Yes  |  Yes  |   -   |
| `grok-4.1-mini`          |  1   | Basic/Super | Yes  |  Yes  |   -   |
| `grok-4.1-fast`          |  1   | Basic/Super | Yes  |  Yes  |   -   |
| `grok-4.1-thinking`      |  4   | Basic/Super | Yes  |  Yes  |   -   |
| `grok-4.1-expert`        |  4   | Basic/Super | Yes  |  Yes  |   -   |
| `grok-imagine-1.0`       |  4   | Basic/Super |  -   |  Yes  |   -   |
| `grok-imagine-1.0-video` |  -   | Basic/Super |  -   |   -   |  Yes  |

<br>

## API

### `POST /v1/chat/completions`

> Generic endpoint: chat, image generation, image editing, video generation, video upscaling

```bash
curl http://localhost:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $GROK2API_API_KEY" \
  -d '{
    "model": "grok-4",
    "messages": [{"role":"user","content":"Hello"}]
  }'
```

<details>
<summary>Supported request parameters</summary>

<br>

| Field                | Type    | Description            | Allowed values                      |
| :------------------- | :------ | :--------------------- | :---------------------------------- |
| `model`              | string  | Model ID               | See model table above               |
| `messages`           | array   | Message list           | See message format below            |
| `stream`             | boolean | Enable streaming       | `true`, `false`                     |
| `thinking`           | string  | Thinking mode          | `enabled`, `disabled`, `null`       |
| `video_config`       | object  | **Video model only**   | -                                   |
| └─ `aspect_ratio`    | string  | Video aspect ratio     | `16:9`, `9:16`, `1:1`, `2:3`, `3:2` |
| └─ `video_length`    | integer | Video length (seconds) | `6`, `10`                           |
| └─ `resolution_name` | string  | Resolution             | `480p`, `720p`                      |
| └─ `preset`          | string  | Style preset           | `fun`, `normal`, `spicy`, `custom`  |

**Message format (messages)**:

| Field     | Type         | Description                                      |
| :-------- | :----------- | :----------------------------------------------- |
| `role`    | string       | Role: `developer`, `system`, `user`, `assistant` |
| `content` | string/array | Message content, plain text or multimodal array  |

**Multimodal content types (content array)**:

| type        | Description | Example                                                      |
| :---------- | :---------- | :----------------------------------------------------------- |
| `text`      | Text        | `{"type": "text", "text": "Describe this image"}`            |
| `image_url` | Image URL   | `{"type": "image_url", "image_url": {"url": "https://..."}}` |
| `file`      | File        | `{"type": "file", "file": {"url": "https://..."}}`           |

Note: any other parameters will be discarded and ignored.

<br>

</details>

### `POST /v1/images/generations`

> Image endpoint: image generation, image editing

```bash
curl http://localhost:8000/v1/images/generations \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $GROK2API_API_KEY" \
  -d '{
    "model": "grok-imagine-1.0",
    "prompt": "A cat floating in space",
    "n": 1
  }'
```

<details>
<summary>Supported request parameters</summary>

<br>

| Field             | Type    | Description      | Allowed values                          |
| :---------------- | :------ | :--------------- | :-------------------------------------- |
| `model`           | string  | Image model ID   | `grok-imagine-1.0`                      |
| `prompt`          | string  | Prompt           | -                                       |
| `n`               | integer | Number of images | `1` - `10` (streaming: `1` or `2` only) |
| `stream`          | boolean | Enable streaming | `true`, `false`                         |
| `size`            | string  | Image size       | `1024x1024` (not customizable yet)      |
| `quality`         | string  | Image quality    | `standard` (not customizable yet)       |
| `response_format` | string  | Response format  | `url`, `b64_json`                       |
| `style`           | string  | Style            | - (not supported yet)                   |

Note: `size`, `quality`, `style` are OpenAI-compatible placeholders, not customizable yet.

<br>

</details>

<br>

## Configuration

Config file: `data/config.toml` (auto-generated from `config.defaults.toml` on first start)

> [!NOTE]
> In production or behind a reverse proxy, make sure `app.app_url` is set to the public URL.
> Otherwise file links may be incorrect or return 403.

### `[app]` Application

| Field          | Description                                                   | Default                 |
| :------------- | :------------------------------------------------------------ | :---------------------- |
| `app_url`      | External access URL for file link generation                  | `http://127.0.0.1:8000` |
| `app_username` | Admin panel username                                          | `admin`                 |
| `app_password` | Admin panel password (required, change on first run)          | `CHANGE_ME_NOW`         |
| `api_key`      | API key (optional, empty = no auth)                           | `""`                    |
| `image_format` | Image output format                                           | `url`                   |
| `video_format` | Video output format (`html` = Markdown link, `url` = raw URL) | `html`                  |

### `[network]` Network

| Field             | Description                       | Default |
| :---------------- | :-------------------------------- | :------ |
| `timeout`         | Grok request timeout (seconds)    | `120`   |
| `base_proxy_url`  | Base proxy for Grok official site | `""`    |
| `asset_proxy_url` | Proxy for Grok static assets      | `""`    |

### `[security]` Security

| Field                   | Description                    | Default                          |
| :---------------------- | :----------------------------- | :------------------------------- |
| `cf_clearance`          | Cloudflare clearance cookie    | `""`                             |
| `browser`               | Browser fingerprint identifier | `chrome136`                      |
| `allow_anonymous_api`   | Allow anonymous API calls      | `false`                          |
| `allow_anonymous_admin` | Allow anonymous admin access   | `false`                          |
| `cors_allow_origins`    | CORS whitelist                 | `["http://127.0.0.1:8000", ...]` |
| `max_body_size_mb`      | Max request body size (MB)     | `50`                             |
| `rate_limit_enabled`    | Enable IP rate limiting        | `true`                           |
| `rate_limit_per_minute` | Requests per minute limit      | `120`                            |
| `rate_limit_burst`      | Burst request limit            | `60`                             |

### `[chat]` Chat

| Field             | Description                   | Default                                                 |
| :---------------- | :---------------------------- | :------------------------------------------------------ |
| `temporary`       | Enable temporary chat mode    | `true`                                                  |
| `disable_memory`  | Disable conversation memory   | `true`                                                  |
| `stream`          | Enable streaming by default   | `true`                                                  |
| `thinking`        | Enable thinking chain output  | `true`                                                  |
| `dynamic_statsig` | Enable dynamic Statsig values | `true`                                                  |
| `filter_tags`     | Auto-filter special Grok tags | `["xaiartifact", "xai:tool_usage_card", "grok:render"]` |

### `[retry]` Retry Strategy

| Field                  | Description                  | Default           |
| :--------------------- | :--------------------------- | :---------------- |
| `max_retry`            | Max retry count              | `3`               |
| `retry_status_codes`   | HTTP codes triggering retry  | `[401, 429, 403]` |
| `retry_backoff_base`   | Base retry delay (seconds)   | `0.5`             |
| `retry_backoff_factor` | Exponential backoff factor   | `2.0`             |
| `retry_backoff_max`    | Max wait per retry (seconds) | `30.0`            |
| `retry_budget`         | Total retry budget (seconds) | `90.0`            |

### `[timeout]` Timeouts

| Field                             | Description                          | Default |
| :-------------------------------- | :----------------------------------- | :------ |
| `stream_idle_timeout`             | Stream idle timeout (seconds)        | `120.0` |
| `video_idle_timeout`              | Video generation timeout (seconds)   | `90.0`  |
| `video_result_wait_timeout`       | Video result wait timeout (seconds)  | `90.0`  |
| `video_result_poll_interval`      | Video result poll interval (seconds) | `1.0`   |
| `video_result_scan_pages`         | Video result scan pages              | `12`    |
| `video_result_scan_assets`        | Video result scan assets             | `800`   |
| `video_result_candidate_attempts` | Video candidate attempts             | `4`     |

### `[image]` Image Generation

| Field                       | Description                       | Default  |
| :-------------------------- | :-------------------------------- | :------- |
| `image_ws`                  | Enable WebSocket image generation | `true`   |
| `image_ws_nsfw`             | Enable NSFW for WS generation     | `true`   |
| `image_ws_blocked_seconds`  | Wait time after block (seconds)   | `15`     |
| `image_ws_final_min_bytes`  | Min bytes for final image         | `100000` |
| `image_ws_medium_min_bytes` | Min bytes for medium image        | `30000`  |
| `image_ws_max_per_request`  | Max images per request            | `6`      |

### `[token]` Token Pool

| Field                          | Description                          | Default     |
| :----------------------------- | :----------------------------------- | :---------- |
| `auto_refresh`                 | Enable auto refresh                  | `true`      |
| `refresh_interval_hours`       | Basic token refresh interval (hours) | `8`         |
| `super_refresh_interval_hours` | Super token refresh interval (hours) | `2`         |
| `fail_threshold`               | Failures before token disabled       | `5`         |
| `save_delay_ms`                | Debounced save delay (ms)            | `2000`      |
| `reload_interval_sec`          | Multi-worker state refresh (seconds) | `120`       |
| `selection_strategy`           | Selection strategy                   | `max_quota` |

> Strategies: `max_quota` (highest quota first), `random`, `weighted`, `lru` (least recently used)

### `[cache]` Cache

| Field                  | Description                  | Default |
| :--------------------- | :--------------------------- | :------ |
| `enable_auto_clean`    | Enable auto cleanup          | `true`  |
| `image_limit_mb`       | Image cache threshold (MB)   | `2048`  |
| `video_limit_mb`       | Video cache threshold (MB)   | `4096`  |
| `cleanup_target_ratio` | Cleanup target ratio (0~1)   | `0.8`   |
| `cleanup_interval_sec` | Cleanup check interval (sec) | `30`    |
| `cleanup_max_delete`   | Max deletes per cleanup      | `1000`  |

### `[proxy]` Proxy Pool

| Field                 | Description                  | Default |
| :-------------------- | :--------------------------- | :------ |
| `proxy_url`           | Fixed proxy URL              | `""`    |
| `proxy_pool_url`      | Proxy pool API URL           | `""`    |
| `proxy_pool_interval` | Proxy pool refresh (seconds) | `300`   |

### `[stats]` Statistics

| Field                | Description                   | Default |
| :------------------- | :---------------------------- | :------ |
| `enabled`            | Enable statistics             | `true`  |
| `hourly_retention`   | Hourly data retention (hours) | `48`    |
| `daily_retention`    | Daily data retention (days)   | `30`    |
| `log_max_entries`    | Max log entries               | `1000`  |
| `flush_interval_sec` | Flush interval (seconds)      | `2`     |

### `[performance]` Concurrency

| Field                      | Description               | Default |
| :------------------------- | :------------------------ | :------ |
| `nsfw_max_concurrent`      | NSFW batch concurrency    | `50`    |
| `nsfw_batch_size`          | NSFW batch size           | `100`   |
| `nsfw_max_tokens`          | NSFW batch max tokens     | `5000`  |
| `usage_max_concurrent`     | Usage refresh concurrency | `100`   |
| `usage_batch_size`         | Usage refresh batch size  | `100`   |
| `usage_max_tokens`         | Usage refresh max tokens  | `5000`  |
| `assets_max_concurrent`    | Asset ops concurrency     | `100`   |
| `assets_batch_size`        | Asset ops batch size      | `50`    |
| `assets_max_tokens`        | Asset ops max tokens      | `5000`  |
| `assets_delete_batch_size` | Asset delete batch        | `50`    |
| `media_max_concurrent`     | Media generation cap      | `200`   |
| `sse_keepalive_sec`        | SSE keepalive (seconds)   | `15`    |

<br>

## Redis Storage Optimization

Redis is the recommended storage backend for production, offering these advantages over Local storage:

- **Atomic Quota Consumption** — Lua scripts perform "check quota -> deduct -> update stats -> cooling transition" in a single RTT, zero race conditions under high concurrency
- **Atomic Batch Updates** — Multi-token field updates via Lua scripts with transactional guarantees, preventing partial updates
- **Connection Pooling** — Built-in async connection pool with automatic connection reuse
- **Multi-Worker Sharing** — All workers share the same state in multi-process deployments, real-time quota consistency
- **Persistence** — Docker Compose enables AOF persistence by default, data survives container restarts

```yaml
# docker-compose.yml ships with Redis pre-configured
environment:
  SERVER_STORAGE_TYPE: redis
  SERVER_STORAGE_URL: redis://redis:6379/0
```

<br>

## Star History

[![Star History Chart](https://api.star-history.com/svg?repos=CPU-JIA/grok2api&type=Timeline)](https://star-history.com/#CPU-JIA/grok2api&Timeline)
