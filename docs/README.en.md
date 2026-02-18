# Grok2API

[中文](../readme.md) | **English**

> [!NOTE]
> This project is for learning and research only. You must comply with Grok **Terms of Use** and **local laws and regulations**. Do not use for illegal purposes.

> [!NOTE]
> Open source projects welcome everyone's support for secondary development and pull requests, but please retain the original author's and frontend's logos to respect the work of others!

Grok2API rebuilt with **FastAPI**, fully aligned with the latest web call format. Supports streaming/non-streaming chat, image generation/editing, video generation/upscale (text-to-video and image-to-video), deep reasoning, token pool concurrency, and automatic load balancing.

<img width="2618" height="1658" alt="image" src="https://github.com/user-attachments/assets/a8c406f8-4c28-483a-8099-c23df5df7605" />

<br>

## Quick Start

### Local

```bash
uv sync
uv run main.py
```

### Docker Compose

```bash
git clone https://github.com/chenyme/grok2api
cd grok2api

docker compose up -d
```

### Vercel

[![Deploy with Vercel](https://vercel.com/button)](https://vercel.com/new/clone?repository-url=https://github.com/chenyme/grok2api&env=LOG_LEVEL,LOG_FILE_ENABLED,DATA_DIR,SERVER_STORAGE_TYPE,SERVER_STORAGE_URL&envDefaults=%7B%22DATA_DIR%22%3A%22/tmp/data%22%2C%22LOG_FILE_ENABLED%22%3A%22false%22%2C%22LOG_LEVEL%22%3A%22INFO%22%2C%22SERVER_STORAGE_TYPE%22%3A%22local%22%2C%22SERVER_STORAGE_URL%22%3A%22%22%7D)

> Set `DATA_DIR=/tmp/data` and disable file logs with `LOG_FILE_ENABLED=false`.
>
> For persistence, use MySQL / Redis / PostgreSQL and set `SERVER_STORAGE_TYPE` and `SERVER_STORAGE_URL`.

### Render

[![Deploy to Render](https://render.com/images/deploy-to-render-button.svg)](https://render.com/deploy?repo=https://github.com/chenyme/grok2api)

> Render free instances sleep after 15 minutes of inactivity; redeploy/restart will lose data.
>
> For persistence, use MySQL / Redis / PostgreSQL and set `SERVER_STORAGE_TYPE` and `SERVER_STORAGE_URL`.

<br>

## Admin Panel

- Access: `http://<host>:8000/admin`
- Default password: `grok2api` (config `app.app_key`, recommended to change)

**Features**:

- **Token Management**: import/add/delete tokens, view status and quota
- **Status Filter**: filter by status (active/limited/expired) or NSFW status
- **Batch Ops**: batch refresh/export/delete/enable NSFW
- **NSFW Enable**: one-click Unhinged for tokens (proxy or `cf_clearance` required)
- **Config Management**: update system config online
- **Cache Management**: view and clear media cache

<br>

## Environment Variables

> Configure `.env`

| Name | Description | Default | Example |
| :-- | :-- | :-- | :-- |
| `LOG_LEVEL` | Log level | `INFO` | `DEBUG` |
| `LOG_FILE_ENABLED` | Enable file logging | `true` | `false` |
| `DATA_DIR` | Data dir (config/tokens/locks) | `./data` | `/data` |
| `SERVER_HOST` | Bind address | `0.0.0.0` | `0.0.0.0` |
| `SERVER_PORT` | Server port | `8000` | `8000` |
| `SERVER_WORKERS` | Uvicorn worker count | `1` | `2` |
| `SERVER_STORAGE_TYPE` | Storage type (`local`/`redis`/`mysql`/`pgsql`) | `local` | `pgsql` |
| `SERVER_STORAGE_URL` | Storage DSN (optional for local) | `""` | `postgresql+asyncpg://user:password@host:5432/db` |

> MySQL example: `mysql+aiomysql://user:password@host:3306/db` (if you provide `mysql://`, it will be converted to `mysql+aiomysql://`).

<br>

## Quotas

- Basic account: 80 requests / 20h
- Super account: 140 requests / 2h

<br>

## Models

| Model | Cost | Account | Chat | Image | Video |
| :-- | :--: | :-- | :--: | :--: | :--: |
| `grok-3` | 1 | Basic/Super | Yes | Yes | - |
| `grok-3-mini` | 1 | Basic/Super | Yes | Yes | - |
| `grok-3-thinking` | 1 | Basic/Super | Yes | Yes | - |
| `grok-4` | 1 | Basic/Super | Yes | Yes | - |
| `grok-4-mini` | 1 | Basic/Super | Yes | Yes | - |
| `grok-4-thinking` | 1 | Basic/Super | Yes | Yes | - |
| `grok-4-heavy` | 4 | Super | Yes | Yes | - |
| `grok-4.1-mini` | 1 | Basic/Super | Yes | Yes | - |
| `grok-4.1-fast` | 1 | Basic/Super | Yes | Yes | - |
| `grok-4.1-expert` | 4 | Basic/Super | Yes | Yes | - |
| `grok-4.1-thinking` | 4 | Basic/Super | Yes | Yes | - |
| `grok-4.20-beta` | 1 | Basic/Super | Yes | Yes | - |
| `grok-imagine-1.0` | - | Basic/Super | - | Yes | - |
| `grok-imagine-1.0-edit` | - | Basic/Super | - | Yes | - |
| `grok-imagine-1.0-video` | - | Basic/Super | - | - | Yes |

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

| Field | Type | Description | Allowed values |
| :-- | :-- | :-- | :-- |
| `model` | string | Model ID | See model list above |
| `messages` | array | Message list | See message format below |
| `stream` | boolean | Enable streaming | `true`, `false` |
| `reasoning_effort` | string | Reasoning effort | `none`, `minimal`, `low`, `medium`, `high`, `xhigh` |
| `temperature` | number | Sampling temperature | `0` ~ `2` |
| `top_p` | number | Nucleus sampling | `0` ~ `1` |
| `video_config` | object | **Video model only** | Supported: `grok-imagine-1.0-video` |
| └─ `aspect_ratio` | string | Video aspect ratio | `16:9`, `9:16`, `1:1`, `2:3`, `3:2`, `1280x720`, `720x1280`, `1792x1024`, `1024x1792`, `1024x1024` |
| └─ `video_length` | integer | Video length (seconds) | `6`, `10`, `15` |
| └─ `resolution_name` | string | Resolution | `480p`, `720p` |
| └─ `preset` | string | Style preset | `fun`, `normal`, `spicy`, `custom` |
| `image_config` | object | **Image models only** | Supported: `grok-imagine-1.0` / `grok-imagine-1.0-edit` |
| └─ `n` | integer | Number of images | `1` ~ `10` |
| └─ `size` | string | Image size | `1280x720`, `720x1280`, `1792x1024`, `1024x1792`, `1024x1024` |
| └─ `response_format` | string | Response format | `url`, `b64_json`, `base64` |

**Message format (messages)**:

| Field | Type | Description |
| :-- | :-- | :-- |
| `role` | string | `developer`, `system`, `user`, `assistant` |
| `content` | string/array | Plain text or multimodal array |

**Multimodal content block types (content array)**:

| type | Description | Example |
| :-- | :-- | :-- |
| `text` | Text | `{"type": "text", "text": "Describe this image"}` |
| `image_url` | Image URL | `{"type": "image_url", "image_url": {"url": "https://..."}}` |
| `input_audio` | Audio | `{"type": "input_audio", "input_audio": {"data": "https://..."}}` |
| `file` | File | `{"type": "file", "file": {"file_data": "https://..."}}` |

**Notes**:

- `image_url/input_audio/file` only supports URL or Data URI (`data:<mime>;base64,...`); raw base64 will be rejected.
- `reasoning_effort`: `none` disables thinking output; any other value enables it.
- `grok-imagine-1.0-edit` requires an image; if multiple are provided, the last image and last text are used.
- `grok-imagine-1.0-video` supports text-to-video and image-to-video via `image_url`.
- Any other parameters will be discarded and ignored.

<br>

</details>

<br>

### `POST /v1/images/generations`

> Image generation endpoint

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

| Field | Type | Description | Allowed values |
| :-- | :-- | :-- | :-- |
| `model` | string | Image model ID | `grok-imagine-1.0` |
| `prompt` | string | Prompt | - |
| `n` | integer | Number of images | `1` - `10` (streaming: `1` or `2` only) |
| `stream` | boolean | Enable streaming | `true`, `false` |
| `size` | string | Image size | `1280x720`, `720x1280`, `1792x1024`, `1024x1792`, `1024x1024` |
| `quality` | string | Image quality | - (not supported) |
| `response_format` | string | Response format | `url`, `b64_json`, `base64` |
| `style` | string | Style | - (not supported) |

**Notes**:

- `quality` and `style` are OpenAI compatibility placeholders and are not customizable yet.

<br>

</details>

<br>

### `POST /v1/images/edits`

> Image edit endpoint (multipart/form-data)

```bash
curl http://localhost:8000/v1/images/edits \
  -H "Authorization: Bearer $GROK2API_API_KEY" \
  -F "model=grok-imagine-1.0-edit" \
  -F "prompt=Make the image clearer" \
  -F "image=@/path/to/image.png" \
  -F "n=1"
```

<details>
<summary>Supported request parameters</summary>

<br>

| Field | Type | Description | Allowed values |
| :-- | :-- | :-- | :-- |
| `model` | string | Image model ID | `grok-imagine-1.0-edit` |
| `prompt` | string | Edit prompt | - |
| `image` | file | Source image | `png`, `jpg`, `webp` |
| `n` | integer | Number of images | `1` - `10` (streaming: `1` or `2` only) |
| `stream` | boolean | Enable streaming | `true`, `false` |
| `size` | string | Image size | `1280x720`, `720x1280`, `1792x1024`, `1024x1792`, `1024x1024` |
| `quality` | string | Image quality | - (not supported) |
| `response_format` | string | Response format | `url`, `b64_json`, `base64` |
| `style` | string | Style | - (not supported) |

**Notes**:

- `quality` and `style` are OpenAI compatibility placeholders and are not customizable yet.

<br>

</details>

<br>

## Configuration

Config file: `data/config.toml`

> [!NOTE]
> In production or behind a reverse proxy, make sure `app.app_url` is a publicly accessible URL,
> otherwise file links may be incorrect or result in 403.

> [!TIP]
> **v2.0 config migration**: old configs are automatically migrated. The old `[grok]` section
> is mapped into the new config structure.

| Module | Field | Key | Description | Default |
| :-- | :-- | :-- | :-- | :-- |
| **app** | `app_url` | App URL | External base URL used for file links. | `http://127.0.0.1:8000` |
|  | `app_key` | Admin password | Login password for admin panel. | `grok2api` |
|  | `api_key` | API key | Optional API key for access. | `""` |
|  | `image_format` | Image format | `url` or `base64`. | `url` |
|  | `video_format` | Video format | `html` or `url` (processed link). | `html` |
|  | `temporary` | Temporary chat | Enable temporary chat mode. | `true` |
|  | `disable_memory` | Disable memory | Disable Grok memory. | `true` |
|  | `stream` | Stream | Enable streaming by default. | `true` |
|  | `thinking` | Thinking | Enable reasoning output. | `true` |
|  | `dynamic_statsig` | Dynamic statsig | Generate dynamic Statsig values. | `true` |
|  | `filter_tags` | Filter tags | Filter special tags in responses. | `["xaiartifact", "xai:tool_usage_card", "grok:render"]` |
| **proxy** | `base_proxy_url` | Base proxy URL | Proxy to Grok web. | `""` |
|  | `asset_proxy_url` | Asset proxy URL | Proxy to Grok assets (img/video). | `""` |
|  | `cf_clearance` | CF Clearance | Cloudflare clearance cookie. | `""` |
|  | `browser` | Browser fingerprint | curl_cffi fingerprint (e.g. chrome136). | `chrome136` |
|  | `user_agent` | User-Agent | HTTP User-Agent string. | `Mozilla/5.0 (Macintosh; ...)` |
| **voice** | `timeout` | Timeout | Voice request timeout (seconds). | `120` |
| **chat** | `concurrent` | Concurrency | Reverse interface concurrency limit. | `10` |
|  | `timeout` | Timeout | Reverse request timeout (seconds). | `60` |
|  | `stream_timeout` | Stream idle timeout | Stream idle timeout (seconds). | `60` |
| **video** | `concurrent` | Concurrency | Reverse interface concurrency limit. | `10` |
|  | `timeout` | Timeout | Reverse request timeout (seconds). | `60` |
|  | `stream_timeout` | Stream idle timeout | Stream idle timeout (seconds). | `60` |
| **retry** | `max_retry` | Max retry | Max retries for upstream failures. | `3` |
|  | `retry_status_codes` | Retry codes | HTTP status codes that trigger retry. | `[401, 429, 403]` |
|  | `retry_backoff_base` | Backoff base | Retry backoff base seconds. | `0.5` |
|  | `retry_backoff_factor` | Backoff factor | Exponential backoff factor. | `2.0` |
|  | `retry_backoff_max` | Backoff max | Max delay per retry (seconds). | `30.0` |
|  | `retry_budget` | Retry budget | Max total retry time (seconds). | `90.0` |
| **image** | `timeout` | Timeout | WebSocket timeout (seconds). | `120` |
|  | `stream_timeout` | Stream idle timeout | WS stream idle timeout (seconds). | `120` |
|  | `final_timeout` | Final timeout | Wait time after medium image (seconds). | `15` |
|  | `nsfw` | NSFW | Enable NSFW. | `true` |
|  | `medium_min_bytes` | Medium min bytes | Minimum size for medium image. | `30000` |
|  | `final_min_bytes` | Final min bytes | Minimum size for final image (JPG > 100KB typical). | `100000` |
| **token** | `auto_refresh` | Auto refresh | Enable token auto refresh. | `true` |
|  | `refresh_interval_hours` | Refresh interval | Basic token refresh interval (hours). | `8` |
|  | `super_refresh_interval_hours` | Super refresh interval | Super token refresh interval (hours). | `2` |
|  | `fail_threshold` | Fail threshold | Consecutive failures to disable. | `5` |
|  | `save_delay_ms` | Save delay | Merge write delay (ms). | `500` |
|  | `reload_interval_sec` | Reload interval | Multi-worker token reload interval (seconds). | `30` |
| **cache** | `enable_auto_clean` | Auto clean | Enable cache auto cleanup. | `true` |
|  | `limit_mb` | Size limit | Cleanup threshold (MB). | `1024` |
| **asset** | `upload_concurrent` | Upload concurrency | Max upload concurrency (recommended 30). | `30` |
|  | `upload_timeout` | Upload timeout | Upload timeout (seconds). | `60` |
|  | `download_concurrent` | Download concurrency | Max download concurrency (recommended 30). | `30` |
|  | `download_timeout` | Download timeout | Download timeout (seconds). | `60` |
|  | `list_concurrent` | List concurrency | Max list concurrency (recommended 10). | `10` |
|  | `list_timeout` | List timeout | List timeout (seconds). | `60` |
|  | `list_batch_size` | List batch size | Tokens per list batch (recommended 10). | `10` |
|  | `delete_concurrent` | Delete concurrency | Max delete concurrency (recommended 10). | `10` |
|  | `delete_timeout` | Delete timeout | Delete timeout (seconds). | `60` |
|  | `delete_batch_size` | Delete batch size | Tokens per delete batch (recommended 10). | `10` |
| **nsfw** | `concurrent` | Concurrency | NSFW batch enable concurrency (recommended 10). | `10` |
|  | `batch_size` | Batch size | NSFW batch size (recommended 50). | `50` |
|  | `timeout` | Timeout | NSFW request timeout (seconds). | `60` |
| **usage** | `concurrent` | Concurrency | Usage refresh concurrency (recommended 10). | `10` |
|  | `batch_size` | Batch size | Usage batch size (recommended 50). | `50` |
|  | `timeout` | Timeout | Usage request timeout (seconds). | `60` |

<br>

## Star History

[![Star History Chart](https://api.star-history.com/svg?repos=Chenyme/grok2api&type=Timeline)](https://star-history.com/#Chenyme/grok2api&Timeline)
