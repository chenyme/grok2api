# Grok2API

**中文** | [English](docs/README.en.md)

> [!NOTE]
> 本项目仅供学习与研究，使用者必须在遵循 Grok 的 **使用条款** 以及 **法律法规** 的情况下使用，不得用于非法用途。

> [!NOTE]
> 开源项目欢迎大家支持二开和PR，但请保留原作者标识和前端标识，尊重他人劳动成果～！

基于 **FastAPI** 重构的 Grok2API，全面适配最新 Web 调用格式，支持流/非流式对话、图像生成/编辑、视频生成/超分（文生视频 / 图生视频）、深度思考，号池并发与自动负载均衡一体化。

<img width="2618" height="1658" alt="image" src="https://github.com/user-attachments/assets/a8c406f8-4c28-483a-8099-c23df5df7605" />

<br>

## 快速开始

### 本地开发

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

### Vercel 部署

[![Deploy with Vercel](https://vercel.com/button)](https://vercel.com/new/clone?repository-url=https://github.com/chenyme/grok2api&env=LOG_LEVEL,LOG_FILE_ENABLED,DATA_DIR,SERVER_STORAGE_TYPE,SERVER_STORAGE_URL&envDefaults=%7B%22DATA_DIR%22%3A%22/tmp/data%22%2C%22LOG_FILE_ENABLED%22%3A%22false%22%2C%22LOG_LEVEL%22%3A%22INFO%22%2C%22SERVER_STORAGE_TYPE%22%3A%22local%22%2C%22SERVER_STORAGE_URL%22%3A%22%22%7D)

> 请务必设置 `DATA_DIR=/tmp/data` 并关闭文件日志 `LOG_FILE_ENABLED=false`。
>
> 持久化请使用 MySQL / Redis / PostgreSQL，并设置：`SERVER_STORAGE_TYPE` 与 `SERVER_STORAGE_URL`。

### Render 部署

[![Deploy to Render](https://render.com/images/deploy-to-render-button.svg)](https://render.com/deploy?repo=https://github.com/chenyme/grok2api)

> Render 免费实例 15 分钟无访问会休眠；重启/重新部署会丢失数据。
>
> 持久化请使用 MySQL / Redis / PostgreSQL，并设置：`SERVER_STORAGE_TYPE` 与 `SERVER_STORAGE_URL`。

<br>

## 管理面板

- 访问地址：`http://<host>:8000/admin`
- 默认密码：`grok2api`（配置项 `app.app_key`，建议修改）

**功能说明**：

- **Token 管理**：导入/添加/删除 Token，查看状态和配额
- **状态筛选**：按状态（正常/限流/失效）或 NSFW 状态筛选
- **批量操作**：批量刷新、导出、删除、开启 NSFW
- **NSFW 开启**：一键为 Token 开启 Unhinged 模式（需代理或 `cf_clearance`）
- **配置管理**：在线修改系统配置
- **缓存管理**：查看和清理媒体缓存

<br>

## 环境变量

> 配置 `.env` 文件

| 变量名 | 说明 | 默认值 | 示例 |
| :-- | :-- | :-- | :-- |
| `LOG_LEVEL` | 日志级别 | `INFO` | `DEBUG` |
| `LOG_FILE_ENABLED` | 是否启用文件日志 | `true` | `false` |
| `DATA_DIR` | 数据目录（配置/Token/锁） | `./data` | `/data` |
| `SERVER_HOST` | 服务监听地址 | `0.0.0.0` | `0.0.0.0` |
| `SERVER_PORT` | 服务端口 | `8000` | `8000` |
| `SERVER_WORKERS` | Uvicorn worker 数量 | `1` | `2` |
| `SERVER_STORAGE_TYPE` | 存储类型（`local`/`redis`/`mysql`/`pgsql`） | `local` | `pgsql` |
| `SERVER_STORAGE_URL` | 存储连接串（local 时可为空） | `""` | `postgresql+asyncpg://user:password@host:5432/db` |

> MySQL 示例：`mysql+aiomysql://user:password@host:3306/db`（若填 `mysql://` 会自动转为 `mysql+aiomysql://`）

<br>

## 可用次数

- Basic 账号：80 次 / 20h
- Super 账号：140 次 / 2h

<br>

## 可用模型

| 模型名 | 计次 | 可用账号 | 对话功能 | 图像功能 | 视频功能 |
| :-- | :--: | :-- | :--: | :--: | :--: |
| `grok-3` | 1 | Basic/Super | 支持 | 支持 | - |
| `grok-3-mini` | 1 | Basic/Super | 支持 | 支持 | - |
| `grok-3-thinking` | 1 | Basic/Super | 支持 | 支持 | - |
| `grok-4` | 1 | Basic/Super | 支持 | 支持 | - |
| `grok-4-mini` | 1 | Basic/Super | 支持 | 支持 | - |
| `grok-4-thinking` | 1 | Basic/Super | 支持 | 支持 | - |
| `grok-4-heavy` | 4 | Super | 支持 | 支持 | - |
| `grok-4.1-mini` | 1 | Basic/Super | 支持 | 支持 | - |
| `grok-4.1-fast` | 1 | Basic/Super | 支持 | 支持 | - |
| `grok-4.1-expert` | 4 | Basic/Super | 支持 | 支持 | - |
| `grok-4.1-thinking` | 4 | Basic/Super | 支持 | 支持 | - |
| `grok-4.20-beta` | 1 | Basic/Super | 支持 | 支持 | - |
| `grok-imagine-1.0` | - | Basic/Super | - | 支持 | - |
| `grok-imagine-1.0-edit` | - | Basic/Super | - | 支持 | - |
| `grok-imagine-1.0-video` | - | Basic/Super | - | - | 支持 |

<br>

## 接口说明

### `POST /v1/chat/completions`

> 通用接口，支持对话聊天、图像生成、图像编辑、视频生成、视频超分

```bash
curl http://localhost:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $GROK2API_API_KEY" \
  -d '{
    "model": "grok-4",
    "messages": [{"role":"user","content":"你好"}]
  }'
```

<details>
<summary>支持的请求参数</summary>

<br>

| 字段 | 类型 | 说明 | 可用参数 |
| :-- | :-- | :-- | :-- |
| `model` | string | 模型名称 | 见上方模型列表 |
| `messages` | array | 消息列表 | 见下方消息格式 |
| `stream` | boolean | 是否开启流式输出 | `true`, `false` |
| `reasoning_effort` | string | 推理强度 | `none`, `minimal`, `low`, `medium`, `high`, `xhigh` |
| `temperature` | number | 采样温度 | `0` ~ `2` |
| `top_p` | number | nucleus 采样 | `0` ~ `1` |
| `video_config` | object | **视频模型专用配置对象** | 支持：`grok-imagine-1.0-video` |
| └─`aspect_ratio` | string | 视频宽高比 | `16:9`, `9:16`, `1:1`, `2:3`, `3:2`, `1280x720`, `720x1280`, `1792x1024`, `1024x1792`, `1024x1024` |
| └─`video_length` | integer | 视频时长 (秒) | `6`, `10`, `15` |
| └─`resolution_name` | string | 分辨率 | `480p`, `720p` |
| └─`preset` | string | 风格预设 | `fun`, `normal`, `spicy`, `custom` |
| `image_config` | object | **图片模型专用配置对象** | 支持：`grok-imagine-1.0` / `grok-imagine-1.0-edit` |
| └─`n` | integer | 生成数量 | `1` ~ `10` |
| └─`size` | string | 图片尺寸 | `1280x720`, `720x1280`, `1792x1024`, `1024x1792`, `1024x1024` |
| └─`response_format` | string | 响应格式 | `url`, `b64_json`, `base64` |

**消息格式 (messages)**：

| 字段 | 类型 | 说明 |
| :-- | :-- | :-- |
| `role` | string | 角色：`developer`, `system`, `user`, `assistant` |
| `content` | string/array | 消息内容，支持纯文本或多模态数组 |

**多模态内容块类型 (content array)**：

| type | 说明 | 示例 |
| :-- | :-- | :-- |
| `text` | 文本内容 | `{"type": "text", "text": "描述这张图片"}` |
| `image_url` | 图片 URL | `{"type": "image_url", "image_url": {"url": "https://..."}}` |
| `input_audio` | 音频 | `{"type": "input_audio", "input_audio": {"data": "https://..."}}` |
| `file` | 文件 | `{"type": "file", "file": {"file_data": "https://..."}}` |

**注意事项**：

- `image_url/input_audio/file` 仅支持 URL 或 Data URI（`data:<mime>;base64,...`），裸 base64 会报错。
- `reasoning_effort`：`none` 表示不输出思考，其他值都会输出思考内容。
- `grok-imagine-1.0-edit` 必须提供图片，多图默认取最后一张与最后一个文本。
- `grok-imagine-1.0-video` 支持文生视频与图生视频（通过 `image_url` 传参考图）。
- 除上述外的其他参数将自动丢弃并忽略。

<br>

</details>

<br>

### `POST /v1/images/generations`

> 图像生成接口

```bash
curl http://localhost:8000/v1/images/generations \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $GROK2API_API_KEY" \
  -d '{
    "model": "grok-imagine-1.0",
    "prompt": "一只在太空漂浮的猫",
    "n": 1
  }'
```

<details>
<summary>支持的请求参数</summary>

<br>

| 字段 | 类型 | 说明 | 可用参数 |
| :-- | :-- | :-- | :-- |
| `model` | string | 图像模型名 | `grok-imagine-1.0` |
| `prompt` | string | 图像描述提示词 | - |
| `n` | integer | 生成数量 | `1` - `10` (流式模式仅限 `1` 或 `2`) |
| `stream` | boolean | 是否开启流式输出 | `true`, `false` |
| `size` | string | 图片尺寸 | `1280x720`, `720x1280`, `1792x1024`, `1024x1792`, `1024x1024` |
| `quality` | string | 图片质量 | - (暂不支持) |
| `response_format` | string | 响应格式 | `url`, `b64_json`, `base64` |
| `style` | string | 风格 | - (暂不支持) |

**注意事项**：

- `quality`、`style` 参数为 OpenAI 兼容保留，当前版本暂不支持自定义。

<br>

</details>

<br>

### `POST /v1/images/edits`

> 图像编辑接口（multipart/form-data）

```bash
curl http://localhost:8000/v1/images/edits \
  -H "Authorization: Bearer $GROK2API_API_KEY" \
  -F "model=grok-imagine-1.0-edit" \
  -F "prompt=把图片变清晰" \
  -F "image=@/path/to/image.png" \
  -F "n=1"
```

<details>
<summary>支持的请求参数</summary>

<br>

| 字段 | 类型 | 说明 | 可用参数 |
| :-- | :-- | :-- | :-- |
| `model` | string | 图像模型名 | `grok-imagine-1.0-edit` |
| `prompt` | string | 编辑描述 | - |
| `image` | file | 待编辑图片 | `png`, `jpg`, `webp` |
| `n` | integer | 生成数量 | `1` - `10` (流式模式仅限 `1` 或 `2`) |
| `stream` | boolean | 是否开启流式输出 | `true`, `false` |
| `size` | string | 图片尺寸 | `1280x720`, `720x1280`, `1792x1024`, `1024x1792`, `1024x1024` |
| `quality` | string | 图片质量 | - (暂不支持) |
| `response_format` | string | 响应格式 | `url`, `b64_json`, `base64` |
| `style` | string | 风格 | - (暂不支持) |

**注意事项**：

- `quality`、`style` 参数为 OpenAI 兼容保留，当前版本暂不支持自定义。

<br>

</details>

<br>

## 参数配置

配置文件：`data/config.toml`

> [!NOTE]
> 生产环境或反向代理部署时，请确保 `app.app_url` 配置为对外可访问的完整 URL，
> 否则可能出现文件访问链接不正确或 403 等问题。

> [!TIP]
> **v2.0 配置结构升级**：旧版本用户更新后，配置会**自动迁移**到新结构，无需手动修改。
> 旧的 `[grok]` 配置节中的自定义值会自动映射到对应的新配置节。

| 模块 | 字段 | 配置名 | 说明 | 默认值 |
| :-- | :-- | :-- | :-- | :-- |
| **app** | `app_url` | 应用地址 | 当前 Grok2API 服务的外部访问 URL，用于文件链接访问。 | `http://127.0.0.1:8000` |
|  | `app_key` | 后台密码 | 登录 Grok2API 管理后台的密码（必填）。 | `grok2api` |
|  | `api_key` | API 密钥 | 调用 Grok2API 服务的 Token（可选）。 | `""` |
|  | `image_format` | 图片格式 | 生成的图片格式（url 或 base64）。 | `url` |
|  | `video_format` | 视频格式 | 生成的视频格式（html 或 url，url 为处理后的链接）。 | `html` |
|  | `temporary` | 临时对话 | 是否启用临时对话模式。 | `true` |
|  | `disable_memory` | 禁用记忆 | 禁用 Grok 记忆功能，防止响应中出现不相关上下文。 | `true` |
|  | `stream` | 流式响应 | 是否默认启用流式输出。 | `true` |
|  | `thinking` | 思维链 | 是否启用模型思维链输出。 | `true` |
|  | `dynamic_statsig` | 动态指纹 | 是否启用动态生成 Statsig 值。 | `true` |
|  | `filter_tags` | 过滤标签 | 自动过滤 Grok 响应中的特殊标签。 | `["xaiartifact", "xai:tool_usage_card", "grok:render"]` |
| **proxy** | `base_proxy_url` | 基础代理 URL | 代理请求到 Grok 官网的基础服务地址。 | `""` |
|  | `asset_proxy_url` | 资源代理 URL | 代理请求到 Grok 官网的静态资源（图片/视频）地址。 | `""` |
|  | `cf_clearance` | CF Clearance | Cloudflare 验证 Cookie，用于绕过反爬虫验证。 | `""` |
|  | `browser` | 浏览器指纹 | curl_cffi 浏览器指纹标识（如 chrome136）。 | `chrome136` |
|  | `user_agent` | User-Agent | HTTP 请求的 User-Agent 字符串。 | `Mozilla/5.0 (Macintosh; ...)` |
| **voice** | `timeout` | 请求超时 | Voice 请求超时时间（秒）。 | `120` |
| **chat** | `concurrent` | 并发上限 | Reverse 接口并发上限。 | `10` |
|  | `timeout` | 请求超时 | Reverse 接口超时时间（秒）。 | `60` |
|  | `stream_timeout` | 流空闲超时 | 流式空闲超时时间（秒）。 | `60` |
| **video** | `concurrent` | 并发上限 | Reverse 接口并发上限。 | `10` |
|  | `timeout` | 请求超时 | Reverse 接口超时时间（秒）。 | `60` |
|  | `stream_timeout` | 流空闲超时 | 流式空闲超时时间（秒）。 | `60` |
| **retry** | `max_retry` | 最大重试 | 请求 Grok 服务失败时的最大重试次数。 | `3` |
|  | `retry_status_codes` | 重试状态码 | 触发重试的 HTTP 状态码列表。 | `[401, 429, 403]` |
|  | `retry_backoff_base` | 退避基数 | 重试退避的基础延迟（秒）。 | `0.5` |
|  | `retry_backoff_factor` | 退避倍率 | 重试退避的指数放大系数。 | `2.0` |
|  | `retry_backoff_max` | 退避上限 | 单次重试等待的最大延迟（秒）。 | `30.0` |
|  | `retry_budget` | 退避预算 | 单次请求的最大重试总耗时（秒）。 | `90.0` |
| **image** | `timeout` | 请求超时 | WebSocket 请求超时时间（秒）。 | `120` |
|  | `stream_timeout` | 流空闲超时 | WebSocket 流式空闲超时时间（秒）。 | `120` |
|  | `final_timeout` | 最终图超时 | 收到中等图后等待最终图的超时秒数。 | `15` |
|  | `nsfw` | NSFW 模式 | WebSocket 请求是否启用 NSFW。 | `true` |
|  | `medium_min_bytes` | 中等图最小字节 | 判定中等质量图的最小字节数。 | `30000` |
|  | `final_min_bytes` | 最终图最小字节 | 判定最终图的最小字节数（通常 JPG > 100KB）。 | `100000` |
| **token** | `auto_refresh` | 自动刷新 | 是否开启 Token 自动刷新机制。 | `true` |
|  | `refresh_interval_hours` | 刷新间隔 | 普通 Token 刷新的时间间隔（小时）。 | `8` |
|  | `super_refresh_interval_hours` | Super 刷新间隔 | Super Token 刷新的时间间隔（小时）。 | `2` |
|  | `fail_threshold` | 失败阈值 | 单个 Token 连续失败多少次后被标记为不可用。 | `5` |
|  | `save_delay_ms` | 保存延迟 | Token 变更合并写入的延迟（毫秒）。 | `500` |
|  | `reload_interval_sec` | 同步间隔 | 多 worker 场景下 Token 状态刷新间隔（秒）。 | `30` |
| **cache** | `enable_auto_clean` | 自动清理 | 是否启用缓存自动清理，开启后按上限自动回收。 | `true` |
|  | `limit_mb` | 清理阈值 | 缓存大小阈值（MB），超过阈值会触发清理。 | `1024` |
| **asset** | `upload_concurrent` | 上传并发 | 上传接口的最大并发数。推荐 30。 | `30` |
|  | `upload_timeout` | 上传超时 | 上传接口超时时间（秒）。推荐 60。 | `60` |
|  | `download_concurrent` | 下载并发 | 下载接口的最大并发数。推荐 30。 | `30` |
|  | `download_timeout` | 下载超时 | 下载接口超时时间（秒）。推荐 60。 | `60` |
|  | `list_concurrent` | 查询并发 | 资产查询接口的最大并发数。推荐 10。 | `10` |
|  | `list_timeout` | 查询超时 | 资产查询接口超时时间（秒）。推荐 60。 | `60` |
|  | `list_batch_size` | 查询批次大小 | 单次查询可处理的 Token 数量。推荐 10。 | `10` |
|  | `delete_concurrent` | 删除并发 | 资产删除接口的最大并发数。推荐 10。 | `10` |
|  | `delete_timeout` | 删除超时 | 资产删除接口超时时间（秒）。推荐 60。 | `60` |
|  | `delete_batch_size` | 删除批次大小 | 单次删除可处理的 Token 数量。推荐 10。 | `10` |
| **nsfw** | `concurrent` | 并发上限 | 批量开启 NSFW 模式时的并发请求上限。推荐 10。 | `10` |
|  | `batch_size` | 批次大小 | 批量开启 NSFW 模式的单批处理数量。推荐 50。 | `50` |
|  | `timeout` | 请求超时 | NSFW 开启相关请求的超时时间（秒）。推荐 60。 | `60` |
| **usage** | `concurrent` | 并发上限 | 批量刷新用量时的并发请求上限。推荐 10。 | `10` |
|  | `batch_size` | 批次大小 | 批量刷新用量的单批处理数量。推荐 50。 | `50` |
|  | `timeout` | 请求超时 | 用量查询接口的超时时间（秒）。推荐 60。 | `60` |

<br>

## Star History

[![Star History Chart](https://api.star-history.com/svg?repos=Chenyme/grok2api&type=Timeline)](https://star-history.com/#Chenyme/grok2api&Timeline)
