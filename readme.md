# Grok2API

**中文** | [English](docs/README.en.md)

> [!NOTE]
> 本项目仅供学习与研究，使用者必须在遵循 Grok 的 **使用条款** 以及 **法律法规** 的情况下使用，不得用于非法用途。

基于 **FastAPI** 重构的 Grok2API，全面适配最新 Web 调用格式，支持流/非流式对话、图像生成/编辑、深度思考，号池并发与自动负载均衡一体化。

### NOTE：项目近期停止接受 PR 和 暂停功能更新，最后优化一次项目结构～

<img width="2562" height="1280" alt="image" src="https://github.com/user-attachments/assets/356d772a-65e1-47bd-abc8-c00bb0e2c9cc" />

<br>

## 使用说明

### 如何启动

- 本地开发

```
uv sync

uv run main.py
```

### 如何部署


#### docker compose 部署
```
git clone https://github.com/chenyme/grok2api

docker compose up -d
```

#### Vercel 部署

[![Deploy with Vercel](https://vercel.com/button)](https://vercel.com/new/clone?repository-url=https://github.com/chenyme/grok2api&env=LOG_LEVEL,LOG_FILE_ENABLED,DATA_DIR,SERVER_STORAGE_TYPE,SERVER_STORAGE_URL&envDefaults=%7B%22DATA_DIR%22%3A%22/tmp/data%22%2C%22LOG_FILE_ENABLED%22%3A%22false%22%2C%22LOG_LEVEL%22%3A%22INFO%22%2C%22SERVER_STORAGE_TYPE%22%3A%22local%22%2C%22SERVER_STORAGE_URL%22%3A%22%22%7D)

> 请务必设置 DATA_DIR=/tmp/data，并关闭文件日志 LOG_FILE_ENABLED=false。
>
> 持久化请使用 MySQL / Redis / PostgreSQL，在 Vercel 环境变量中设置：SERVER_STORAGE_TYPE（mysql/redis/pgsql）与 SERVER_STORAGE_URL。

#### Render 部署

[![Deploy to Render](https://render.com/images/deploy-to-render-button.svg)](https://render.com/deploy?repo=https://github.com/chenyme/grok2api)

> Render 免费实例 15 分钟无访问会休眠，恢复/重启/重新部署会丢失。
>
> 持久化请使用 MySQL / Redis / PostgreSQL，在 Render 环境变量中设置：SERVER_STORAGE_TYPE（mysql/redis/pgsql）与 SERVER_STORAGE_URL。

### 管理面板

访问地址：`http://<host>:8000/admin`
默认登录密码：`grok2api`（对应配置项 `app.app_key`，建议修改）。

**功能说明**：

- **Token 管理**：导入/添加/删除 Token，查看状态和配额
- **状态筛选**：按状态（正常/限流/失效）或 NSFW 状态筛选
- **批量操作**：批量刷新、导出、删除、开启 NSFW
- **NSFW 开启**：一键为 Token 开启 Unhinged 模式（需代理或 cf_clearance）
- **配置管理**：在线修改系统配置
- **缓存管理**：查看和清理媒体缓存

### 环境变量

> 配置 `.env` 文件

| 变量名                  | 说明                                                | 默认值      | 示例                                                |
| :---------------------- | :-------------------------------------------------- | :---------- | :-------------------------------------------------- |
| `LOG_LEVEL`           | 日志级别                                            | `INFO`    | `DEBUG`                                           |
| `LOG_FILE_ENABLED`   | 是否启用文件日志                                    | `true`    | `false`                                           |
| `DATA_DIR`           | 数据目录（配置/Token/锁）                           | `./data`  | `/data`                                           |
| `SERVER_HOST`         | 服务监听地址                                        | `0.0.0.0` | `0.0.0.0`                                         |
| `SERVER_PORT`         | 服务端口                                            | `8000`    | `8000`                                            |
| `SERVER_WORKERS`      | Uvicorn worker 数量                                 | `1`       | `2`                                               |
| `SERVER_STORAGE_TYPE` | 存储类型（`local`/`redis`/`mysql`/`pgsql`） | `local`   | `pgsql`                                           |
| `SERVER_STORAGE_URL`  | 存储连接串（local 时可为空）                        | `""`      | `postgresql+asyncpg://user:password@host:5432/db` |

> MySQL 示例：`mysql+aiomysql://user:password@host:3306/db`（若填 `mysql://` 会自动转为 `mysql+aiomysql://`）

### 可用次数

- Basic 账号：80 次 / 20h
- Super 账号：140 次 / 2h

### 可用模型

| 模型名                     | 计次 | 可用账号    | 对话功能 | 图像功能 | 视频功能 |
| :------------------------- | :--: | :---------- | :------: | :------: | :------: |
| `grok-3`                 |  1  | Basic/Super |   支持   |   支持   |    -    |
| `grok-3-fast`            |  1  | Basic/Super |   支持   |   支持   |    -    |
| `grok-4`                 |  1  | Basic/Super |   支持   |   支持   |    -    |
| `grok-4-mini`            |  1  | Basic/Super |   支持   |   支持   |    -    |
| `grok-4-fast`            |  1  | Basic/Super |   支持   |   支持   |    -    |
| `grok-4-heavy`           |  4  | Super       |   支持   |   支持   |    -    |
| `grok-4.1`               |  1  | Basic/Super |   支持   |   支持   |    -    |
| `grok-4.1-thinking`      |  4  | Basic/Super |   支持   |   支持   |    -    |
| `grok-imagine-1.0`       |  4  | Basic/Super |    -    |   支持   |    -    |
| `grok-imagine-1.0-edit`  |  4  | Basic/Super |    -    |   支持   |    -    |
| `grok-imagine-1.0-video` |  -  | Basic/Super |    -    |    -    |   支持   |

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

| 字段                    | 类型    | 说明                           | 可用参数                                      |
| :---------------------- | :------ | :----------------------------- | :-------------------------------------------- |
| `model`               | string  | 模型名称                       | 见上方模型列表                                |
| `messages`            | array   | 消息列表                       | 见下方消息格式                                |
| `stream`              | boolean | 是否开启流式输出               | `true`, `false`                           |
| `thinking`            | string  | 思维链模式                     | `enabled`, `disabled`, `null`           |
| `video_config`        | object  | **视频模型专用配置对象** | -                                             |
| └─`aspect_ratio`    | string  | 视频宽高比                     | `16:9`, `9:16`, `1:1`, `2:3`, `3:2` |
| └─`video_length`    | integer | 视频时长 (秒)                  | `6`, `10`, `15`                         |
| └─`resolution_name` | string  | 分辨率                         | `480p`, `720p`                            |
| └─`preset`          | string  | 风格预设                       | `fun`, `normal`, `spicy`, `custom`    |

**消息格式 (messages)**：

| 字段        | 类型         | 说明                                                     |
| :---------- | :----------- | :------------------------------------------------------- |
| `role`    | string       | 角色：`developer`, `system`, `user`, `assistant` |
| `content` | string/array | 消息内容，支持纯文本或多模态数组                         |

**多模态内容块类型 (content array)**：

| type          | 说明     | 示例                                                           |
| :------------ | :------- | :------------------------------------------------------------- |
| `text`      | 文本内容 | `{"type": "text", "text": "描述这张图片"}`                   |
| `image_url` | 图片 URL | `{"type": "image_url", "image_url": {"url": "https://..."}}` |
| `file`      | 文件     | `{"type": "file", "file": {"url": "https://..."}}`           |

注：除上述外的其他参数将自动丢弃并忽略

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

| 字段                | 类型    | 说明             | 可用参数                                     |
| :------------------ | :------ | :--------------- | :------------------------------------------- |
| `model`           | string  | 图像模型名       | `grok-imagine-1.0`                         |
| `prompt`          | string  | 图像描述提示词   | -                                            |
| `n`               | integer | 生成数量         | `1` - `10` (流式模式仅限 `1` 或 `2`) |
| `stream`          | boolean | 是否开启流式输出 | `true`, `false`                          |
| `size`            | string  | 图片尺寸         | `1024x1024` (WS 模式支持映射到比例)        |
| `quality`         | string  | 图片质量         | `standard` (暂不支持自定义)                |
| `response_format` | string  | 响应格式         | `url`, `b64_json`                        |
| `style`           | string  | 风格             | - (暂不支持)                                 |

注：`quality`、`style` 参数为 OpenAI 兼容保留，当前版本暂不支持自定义。
当开启 `grok.image_ws=true` 时，`size` 将映射为宽高比（仅支持 5 种：`16:9`、`9:16`、`1:1`、`2:3`、`3:2`），也可以直接传以上比例字符串：
`1024x576/1280x720/1536x864 -> 16:9`，`576x1024/720x1280/864x1536 -> 9:16`，`1024x1024/512x512 -> 1:1`，`1024x1536/512x768/768x1024 -> 2:3`，`1536x1024/768x512/1024x768 -> 3:2`，其他值默认 `2:3`。

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

| 字段                | 类型    | 说明             | 可用参数                                     |
| :------------------ | :------ | :--------------- | :------------------------------------------- |
| `model`           | string  | 图像模型名       | `grok-imagine-1.0-edit`                    |
| `prompt`          | string  | 编辑描述         | -                                            |
| `image`           | file    | 待编辑图片       | `png`, `jpg`, `webp`                   |
| `n`               | integer | 生成数量         | `1` - `10` (流式模式仅限 `1` 或 `2`) |
| `stream`          | boolean | 是否开启流式输出 | `true`, `false`                          |
| `size`            | string  | 图片尺寸         | `1024x1024` (暂不支持自定义)               |
| `quality`         | string  | 图片质量         | `standard` (暂不支持自定义)                |
| `response_format` | string  | 响应格式         | `url`, `b64_json`                        |
| `style`           | string  | 风格             | - (暂不支持)                                 |

注：`size`、`quality`、`style` 参数为 OpenAI 兼容保留，当前版本暂不支持自定义

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

| 模块                  | 字段                             | 配置名             | 说明                                                  | 默认值                                                    |
| :-------------------- | :------------------------------- | :----------------- | :---------------------------------------------------- | :-------------------------------------------------------- |
| **app**         | `app_url`                      | 应用地址           | 当前 Grok2API 服务的外部访问 URL，用于文件链接访问。  | `http://127.0.0.1:8000`                                 |
|                       | `app_key`                      | 后台密码           | 登录 Grok2API 管理后台的密码（必填）。                | `grok2api`                                              |
|                       | `api_key`                      | API 密钥           | 调用 Grok2API 服务的 Token（可选）。                  | `""`                                                    |
|                       | `image_format`                 | 图片格式           | 生成的图片格式（url 或 base64）。                     | `url`                                                   |
|                       | `video_format`                 | 视频格式           | 生成的视频格式（html 或 url，url 为处理后的链接）。   | `html`                                                  |
| **network**     | `timeout`                      | 请求超时           | 请求 Grok 服务的超时时间（秒）。                      | `120`                                                   |
|                       | `base_proxy_url`               | 基础代理 URL       | 代理请求到 Grok 官网的基础服务地址。                  | `""`                                                    |
|                       | `asset_proxy_url`              | 资源代理 URL       | 代理请求到 Grok 官网的静态资源（图片/视频）地址。     | `""`                                                    |
| **security**    | `cf_clearance`                 | CF Clearance       | Cloudflare 验证 Cookie，用于绕过反爬虫验证。          | `""`                                                    |
|                       | `browser`                      | 浏览器指纹         | curl_cffi 浏览器指纹标识（如 chrome136）。            | `chrome136`                                             |
|                       | `user_agent`                   | User-Agent         | HTTP 请求的 User-Agent 字符串。                       | `Mozilla/5.0 (Macintosh; ...)`                          |
| **chat**        | `temporary`                    | 临时对话           | 是否启用临时对话模式。                                | `true`                                                  |
|                       | `disable_memory`               | 禁用记忆           | 禁用 Grok 记忆功能，防止响应中出现不相关上下文。      | `true`                                                  |
|                       | `stream`                       | 流式响应           | 是否默认启用流式输出。                                | `true`                                                  |
|                       | `thinking`                     | 思维链             | 是否启用模型思维链输出。                              | `true`                                                  |
|                       | `dynamic_statsig`              | 动态指纹           | 是否启用动态生成 Statsig 值。                         | `true`                                                  |
|                       | `filter_tags`                  | 过滤标签           | 自动过滤 Grok 响应中的特殊标签。                      | `["xaiartifact", "xai:tool_usage_card", "grok:render"]` |
| **retry**       | `max_retry`                    | 最大重试           | 请求 Grok 服务失败时的最大重试次数。                  | `3`                                                     |
|                       | `retry_status_codes`           | 重试状态码         | 触发重试的 HTTP 状态码列表。                          | `[401, 429, 403]`                                       |
|                       | `retry_backoff_base`           | 退避基数           | 重试退避的基础延迟（秒）。                            | `0.5`                                                   |
|                       | `retry_backoff_factor`         | 退避倍率           | 重试退避的指数放大系数。                              | `2.0`                                                   |
|                       | `retry_backoff_max`            | 退避上限           | 单次重试等待的最大延迟（秒）。                        | `30.0`                                                  |
|                       | `retry_budget`                 | 退避预算           | 单次请求的最大重试总耗时（秒）。                      | `90.0`                                                  |
| **timeout**     | `stream_idle_timeout`          | 流空闲超时         | 流式响应空闲超时（秒），超过将断开。                  | `120.0`                                                 |
|                       | `video_idle_timeout`           | 视频空闲超时       | 视频生成空闲超时（秒），超过将断开。                  | `90.0`                                                  |
| **image**       | `image_ws`                     | WebSocket 生成     | 启用后 `/v1/images/generations` 走 WebSocket 直连。 | `true`                                                  |
|                       | `image_ws_nsfw`                | NSFW 模式          | WebSocket 请求是否启用 NSFW。                         | `true`                                                  |
|                       | `image_ws_blocked_seconds`     | Blocked 阈值       | 收到中等图后超过该秒数仍无最终图则判定 blocked。      | `15`                                                    |
|                       | `image_ws_final_min_bytes`     | 最终图最小字节     | 判定最终图的最小字节数（通常 JPG > 100KB）。          | `100000`                                                |
|                       | `image_ws_medium_min_bytes`    | 中等图最小字节     | 判定中等质量图的最小字节数。                          | `30000`                                                 |
| **token**       | `auto_refresh`                 | 自动刷新           | 是否开启 Token 自动刷新机制。                         | `true`                                                  |
|                       | `refresh_interval_hours`       | 刷新间隔           | 普通 Token 刷新的时间间隔（小时）。                   | `8`                                                     |
|                       | `super_refresh_interval_hours` | Super 刷新间隔     | Super Token 刷新的时间间隔（小时）。                  | `2`                                                     |
|                       | `fail_threshold`               | 失败阈值           | 单个 Token 连续失败多少次后被标记为不可用。           | `5`                                                     |
|                       | `save_delay_ms`                | 保存延迟           | Token 变更合并写入的延迟（毫秒）。                    | `500`                                                   |
|                       | `reload_interval_sec`          | 同步间隔           | 多 worker 场景下 Token 状态刷新间隔（秒）。           | `30`                                                    |
| **cache**       | `enable_auto_clean`            | 自动清理           | 是否启用缓存自动清理，开启后按上限自动回收。          | `true`                                                  |
|                       | `limit_mb`                     | 清理阈值           | 缓存大小阈值（MB），超过阈值会触发清理。              | `1024`                                                  |
| **performance** | `media_max_concurrent`         | Media 并发上限     | 视频/媒体生成请求的并发上限。推荐 50。                | `50`                                                    |
|                       | `assets_max_concurrent`        | Assets 并发上限    | 批量查找/删除资产时的并发请求上限。推荐 25。          | `25`                                                    |
|                       | `assets_batch_size`            | Assets 批次大小    | 批量查找/删除资产时的单批处理数量。推荐 10。          | `10`                                                    |
|                       | `assets_max_tokens`            | Assets 最大数量    | 单次批量查找/删除资产时的处理数量上限。推荐 1000。    | `1000`                                                  |
|                       | `assets_delete_batch_size`     | Assets 删除批量    | 单账号批量删除资产时的单批并发数量。推荐 10。         | `10`                                                    |
|                       | `usage_max_concurrent`         | Token 刷新并发上限 | 批量刷新 Token 用量时的并发请求上限。推荐 25。        | `25`                                                    |
|                       | `usage_batch_size`             | Token 刷新批次大小 | 批量刷新 Token 用量的单批处理数量。推荐 50。          | `50`                                                    |
|                       | `usage_max_tokens`             | Token 刷新最大数量 | 单次批量刷新 Token 用量时的处理数量上限。推荐 1000。  | `1000`                                                  |
|                       | `nsfw_max_concurrent`          | NSFW 开启并发上限  | 批量开启 NSFW 模式时的并发请求上限。推荐 10。         | `10`                                                    |
|                       | `nsfw_batch_size`              | NSFW 开启批次大小  | 批量开启 NSFW 模式的单批处理数量。推荐 50。           | `50`                                                    |
|                       | `nsfw_max_tokens`              | NSFW 开启最大数量  | 单次批量开启 NSFW 的 Token 数量上限。推荐 1000。      | `1000`                                                  |

<br>

## Star History

[![Star History Chart](https://api.star-history.com/svg?repos=Chenyme/grok2api&type=Timeline)](https://star-history.com/#Chenyme/grok2api&Timeline)
