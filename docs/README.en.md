# Grok2API

[中文](README.md) | **English**

> [!NOTE]
> This project is for learning and research only. You must comply with Grok's Terms of Use and applicable laws. Do not use it for illegal purposes.

Grok2API rebuilt with **FastAPI**, fully aligned with the latest web call format. Supports streaming and non-streaming chat, image generation/editing, deep thinking, token pool concurrency, and automatic load balancing.

<img width="2562" height="1280" alt="image" src="https://github.com/user-attachments/assets/356d772a-65e1-47bd-abc8-c00bb0e2c9cc" />

<br>

## Usage

### How to start

- Local development

```
uv sync

uv run main.py
```

- Deployment

```
git clone https://github.com/chenyme/grok2api

docker compose up -d
```

### Admin panel

URL: `http://<host>:8000/admin`  
Default password: `grok2api` (config key `app.app_key`, change it in production).

### Environment variables

| Variable | Description | Default | Example |
| :--- | :--- | :--- | :--- |
| `LOG_LEVEL` | Log level | `INFO` | `DEBUG` |
| `SERVER_HOST` | Bind address | `0.0.0.0` | `0.0.0.0` |
| `SERVER_PORT` | Service port | `8000` | `8000` |
| `SERVER_WORKERS` | Uvicorn worker count | `1` | `2` |
| `SERVER_STORAGE_TYPE` | Storage type (`local`/`redis`/`mysql`/`pgsql`) | `local` | `pgsql` |
| `SERVER_STORAGE_URL` | Storage URL (empty for local) | `""` | `postgresql+asyncpg://user:password@host:5432/db` |

> MySQL example: `mysql+aiomysql://user:password@host:3306/db` (if you set `mysql://`, it will be normalized to `mysql+aiomysql://`)

### Usage limits

- Basic account: 80 requests / 20h
- Super account: 140 requests / 2h

### Models

| Model | Cost | Account | Chat | Image | Video |
| :--- | :---: | :--- | :---: | :---: | :---: |
| `grok-3` | 1 | Basic/Super | Yes | Yes | - |
| `grok-3-fast` | 1 | Basic/Super | Yes | Yes | - |
| `grok-4` | 1 | Basic/Super | Yes | Yes | - |
| `grok-4-mini` | 1 | Basic/Super | Yes | Yes | - |
| `grok-4-fast` | 1 | Basic/Super | Yes | Yes | - |
| `grok-4-heavy` | 4 | Super | Yes | Yes | - |
| `grok-4.1` | 1 | Basic/Super | Yes | Yes | - |
| `grok-4.1-thinking` | 4 | Basic/Super | Yes | Yes | - |
| `grok-imagine-1.0` | 4 | Basic/Super | - | Yes | - |
| `grok-imagine-1.0-edit` | 4 | Basic/Super | - | Yes | - |
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
| :--- | :--- | :--- | :--- |
| `model` | string | Model ID | - |
| `messages` | array | Message list | `developer`, `system`, `user`, `assistant` |
| `stream` | boolean | Enable streaming | `true`, `false` |
| `thinking` | string | Thinking mode | `enabled`, `disabled`, `null` |
| `video_config` | object | **Video model only** | - |
| └─ `aspect_ratio` | string | Video aspect ratio | `16:9`, `9:16`, `1:1`, `2:3`, `3:2` |
| └─ `video_length` | integer | Video length (seconds) | `6`, `10`, `15` |
| └─ `resolution_name` | string | Resolution | `480p`, `720p` |
| └─ `preset` | string | Style preset | `fun`, `normal`, `spicy` |

Note: any other parameters will be discarded and ignored.

<br>

</details>

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
| :--- | :--- | :--- | :--- |
| `model` | string | Image model ID | `grok-imagine-1.0` |
| `prompt` | string | Prompt | - |
| `n` | integer | Number of images | `1` - `10` (streaming: `1` or `2` only) |
| `stream` | boolean | Enable streaming | `true`, `false` |
| `size` | string | Image size | `1024x1024` (WS mode maps to aspect ratio) |
| `quality` | string | Image quality | `standard` (not customizable yet) |
| `response_format` | string | Response format | `url`, `b64_json` |
| `style` | string | Style | - (not supported yet) |

Note: when `grok.image_ws=true`, `size` is mapped to aspect ratio (only 5 supported: `16:9`, `9:16`, `1:1`, `2:3`, `3:2`); you can also pass those ratio strings directly:  
`1024x576/1280x720/1536x864 -> 16:9`, `576x1024/720x1280/864x1536 -> 9:16`, `1024x1024/512x512 -> 1:1`, `1024x1536/512x768/768x1024 -> 2:3`, `1536x1024/768x512/1024x768 -> 3:2`, otherwise defaults to `2:3`. Other parameters are ignored.

<br>

</details>

<br>

### `POST /v1/images/edits`
> Image edit endpoint (multipart/form-data)

```bash
curl http://localhost:8000/v1/images/edits \
  -H "Authorization: Bearer $GROK2API_API_KEY" \
  -F "model=grok-imagine-1.0-edit" \
  -F "prompt=Make it sharper" \
  -F "image=@/path/to/image.png" \
  -F "n=1"
```

<details>
<summary>Supported request parameters</summary>

<br>

| Field | Type | Description | Allowed values |
| :--- | :--- | :--- | :--- |
| `model` | string | Image model ID | `grok-imagine-1.0-edit` |
| `prompt` | string | Edit prompt | - |
| `image` | file | Image file | `png`, `jpg`, `webp` |
| `n` | integer | Number of images | `1` - `10` (streaming: `1` or `2` only) |
| `stream` | boolean | Enable streaming | `true`, `false` |
| `size` | string | Image size | `1024x1024` (not customizable yet) |
| `quality` | string | Image quality | `standard` (not customizable yet) |
| `response_format` | string | Response format | `url`, `b64_json` |
| `style` | string | Style | - (not supported yet) |

Note: `size`, `quality`, `style` are OpenAI compatibility placeholders and are not customizable yet.

<br>

</details>

<br>

## Configuration

Config file: `data/config.toml`

> [!NOTE]
> In production or behind a reverse proxy, make sure `app.app_url` is set to the public URL.
> Otherwise file links may be incorrect or return 403.

| Module | Field | Key | Description | Default |
| :--- | :--- | :--- | :--- | :--- |
| **app** | `app_url` | App URL | External access URL for Grok2API (used for file links). | `http://127.0.0.1:8000` |
| | `app_key` | Admin password | Password for the Grok2API admin panel (required). | `grok2api` |
| | `api_key` | API key | Token for calling Grok2API (optional). | `""` |
| | `image_format` | Image format | Output image format (`url` or `base64`). | `url` |
| | `video_format` | Video format | Output video format (html tag or processed url). | `html` |
| **grok** | `temporary` | Temporary chat | Enable temporary conversation mode. | `true` |
| | `stream` | Streaming | Enable streaming by default. | `true` |
| | `thinking` | Thinking chain | Enable model thinking output. | `true` |
| | `dynamic_statsig` | Dynamic fingerprint | Enable dynamic Statsig value generation. | `true` |
| | `filter_tags` | Filter tags | Auto-filter special tags in Grok responses. | `["xaiartifact", "xai:tool_usage_card", "grok:render"]` |
| | `timeout` | Timeout | Timeout for Grok requests (seconds). | `120` |
| | `base_proxy_url` | Base proxy URL | Base service address proxying Grok official site. | `""` |
| | `asset_proxy_url` | Asset proxy URL | Proxy URL for Grok static assets (images/videos). | `""` |
| | `cf_clearance` | CF Clearance | Cloudflare clearance cookie for verification. | `""` |
| | `max_retry` | Max retries | Max retries on Grok request failure. | `3` |
| | `retry_status_codes` | Retry status codes | HTTP status codes that trigger retry. | `[401, 429, 403]` |
| | `retry_backoff_base` | Backoff base | Base delay for retry backoff (seconds). | `0.5` |
| | `retry_backoff_factor` | Backoff factor | Exponential multiplier for retry backoff. | `2.0` |
| | `retry_backoff_max` | Backoff max | Max wait per retry (seconds). | `30.0` |
| | `retry_budget` | Backoff budget | Max total retry time per request (seconds). | `90.0` |
| | `stream_idle_timeout` | Stream idle timeout | Idle timeout for streaming responses (seconds). | `45.0` |
| | `video_idle_timeout` | Video idle timeout | Idle timeout for video generation (seconds). | `90.0` |
| | `image_ws` | Image WS generation | When enabled, `/v1/images/generations` uses WebSocket. | `false` |
| | `image_ws_blocked_seconds` | WS blocked threshold | Mark blocked if no final image after this many seconds post-medium. | `15` |
| | `image_ws_final_min_bytes` | WS final min bytes | Minimum bytes to treat an image as final (JPG usually > 100KB). | `100000` |
| | `image_ws_nsfw` | WS NSFW | Enable NSFW in WS requests. | `true` |
| **token** | `auto_refresh` | Auto refresh | Enable automatic token refresh. | `true` |
| | `refresh_interval_hours` | Refresh interval | Token refresh interval (hours). | `8` |
| | `super_refresh_interval_hours` | Super refresh interval | Super token refresh interval (hours). | `2` |
| | `fail_threshold` | Failure threshold | Consecutive failures before a token is disabled. | `5` |
| | `save_delay_ms` | Save delay | Debounced save delay for token changes (ms). | `500` |
| | `reload_interval_sec` | Consistency refresh | Token state refresh interval in multi-worker setups (sec). | `30` |
| **cache** | `enable_auto_clean` | Auto clean | Enable cache auto clean; cleanup when exceeding limit. | `true` |
| | `limit_mb` | Cleanup threshold | Cache size threshold (MB) that triggers cleanup. | `1024` |
| **performance** | `nsfw_max_concurrent` | NSFW enable concurrency | Concurrency cap for enabling NSFW in batch. Recommended 10. | `10` |
| | `nsfw_batch_size` | NSFW enable batch size | Batch size for enabling NSFW. Recommended 50. | `50` |
| | `nsfw_max_tokens` | NSFW enable max tokens | Max tokens per NSFW batch to avoid mistakes. Recommended 1000. | `1000` |
| | `usage_max_concurrent` | Token usage refresh concurrency | Concurrency cap for batch usage refresh. Recommended 25. | `25` |
| | `usage_batch_size` | Token usage refresh batch size | Batch size for usage refresh. Recommended 50. | `50` |
| | `usage_max_tokens` | Token usage refresh max tokens | Max tokens per usage refresh batch. Recommended 1000. | `1000` |
| | `assets_max_concurrent` | Online assets find/delete concurrency | Concurrency cap for online asset find/delete. Recommended 25. | `25` |
| | `assets_batch_size` | Online assets find/delete batch size | Batch size for online asset find/delete. Recommended 10. | `10` |
| | `assets_max_tokens` | Online assets find/delete max tokens | Max tokens per online asset find/delete batch. Recommended 1000. | `1000` |
| | `assets_delete_batch_size` | Online assets delete batch | Batch concurrency for online asset deletion. Recommended 10. | `10` |
| | `media_max_concurrent` | Media concurrency | Concurrency cap for video/media generation. Recommended 50. | `50` |

<br>

## Star History

[![Star History Chart](https://api.star-history.com/svg?repos=Chenyme/grok2api&type=Timeline)](https://star-history.com/#Chenyme/grok2api&Timeline)
