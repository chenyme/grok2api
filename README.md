# Grok2API

**中文** | [English](docs/README.en.md)

> [!NOTE]
> 本项目仅供学习与研究，使用者必须在遵循 Grok 的 **使用条款** 以及 **法律法规** 的情况下使用，不得用于非法用途。

基于 **FastAPI** 重构的 Grok2API，全面适配最新 Web 调用格式，支持流/非流式对话、图像生成/编辑、视频生成、深度思考，号池并发与自动负载均衡一体化。

## 致谢

本项目基于以下优秀项目开发整合，特此感谢：

- [@Chenyme/grok2api](https://github.com/chenyme/grok2api) - 原始项目作者
- [@Tomiya233/grok2api](https://github.com/Tomiya233/grok2api) - 功能增强版本

<br>

## 特性亮点

- **OpenAI 兼容 API** — 对话、图像、视频全走 `/v1/chat/completions`，兼容所有 OpenAI SDK 客户端
- **多后端存储** — Local / Redis / MySQL / PostgreSQL，生产环境推荐 Redis
- **Redis 原子操作** — Lua 脚本实现配额消耗、统计更新、冷却切换一步到位，高并发零竞态
- **Token 双池** — ssoBasic (80 次/20h) + ssoSuper (140 次/2h) 自动切换，多策略负载均衡
- **安全中间件** — IP 限速、请求体大小限制、CORS 白名单、鉴权策略可配
- **Imagine 瀑布流** — WebSocket + SSE 双模式实时图片生成，支持自动降级
- **统计监控** — 请求统计、日志审计、代理状态面板
- **代理池** — 可配置代理 URL 或代理池 API，定时自动轮换
- **缓存管理** — 图片/视频本地缓存，按阈值自动清理

<br>

## 快速开始

### Docker Compose 部署（推荐）

> [!TIP]
> 推荐使用 Docker Compose + Redis 部署，开箱即用，支持多 worker 并发和原子操作优化。

```bash
git clone https://github.com/CPU-JIA/grok2api
cd grok2api

# 按需修改 docker-compose.yml 中的环境变量
docker compose up -d
```

默认配置 8 个 worker + Redis 存储，生产环境建议修改 `data/config.toml` 中的 `app.app_password`。

### 本地开发

```bash
uv sync
uv run main.py
```

### 管理面板

访问地址：`http://<host>:8000/admin`
默认登录账户 / 密码：`admin` / `CHANGE_ME_NOW`（对应配置项 `app.app_username` 和 `app.app_password`，首次启动请立即修改）。

**功能模块**：

- **Token 管理**：导入/添加/删除 Token，查看状态和配额，按状态或 NSFW 筛选
- **Key 管理**：管理 API Key，支持多 Key 分发
- **统计监控**：请求统计、日志审计、代理状态监控
- **缓存预览**：查看和清理本地媒体缓存（图片/视频）
- **配置管理**：在线修改系统配置，实时生效
- **Imagine 瀑布流**：WebSocket/SSE 实时图片生成，支持批量下载
- **Voice Live**：LiveKit 语音会话，连接 Grok Voice

**批量操作**：批量刷新、导出、删除 Token，一键开启 NSFW（Unhinged 模式）

<details>
<summary>界面预览</summary>
<br>

**登录**

![登录](Images/登录.png)

**Token 管理**

![Token 管理](Images/Token管理.png)

**Imagine 瀑布流**

![Imagine](Images/Imagine.png)

**Voice Live**

![Voice](Images/Voice.png)

**Key 管理**

![Key 管理](Images/Key管理.png)

**统计监控 — 请求统计**

![统计监控-请求统计](Images/统计监控-请求统计.png)

**统计监控 — 日志审计**

![统计监控-日志审计](Images/请求监控-日志统计.png)

**缓存预览**

![缓存预览](Images/缓存预览.png)

**配置管理 — 全局配置 & Grok 配置**

![配置管理1](Images/配置管理1.png)

**配置管理 — Token 池 & 性能配置**

![配置管理2](Images/配置管理2.png)

</details>

### 环境变量

> 通过 `.env` 文件或 `docker-compose.yml` 的 `environment` 配置

| 变量名                | 说明                                        | 默认值    | 示例                   |
| :-------------------- | :------------------------------------------ | :-------- | :--------------------- |
| `LOG_LEVEL`           | 日志级别                                    | `INFO`    | `DEBUG`                |
| `SERVER_HOST`         | 服务监听地址                                | `0.0.0.0` | `0.0.0.0`              |
| `SERVER_PORT`         | 服务端口                                    | `8000`    | `8000`                 |
| `SERVER_WORKERS`      | Uvicorn worker 数量                         | `1`       | `4`                    |
| `SERVER_STORAGE_TYPE` | 存储类型（`local`/`redis`/`mysql`/`pgsql`） | `local`   | `redis`                |
| `SERVER_STORAGE_URL`  | 存储连接串（local 时可为空）                | `""`      | `redis://redis:6379/0` |
| `DATA_DIR`            | 数据目录路径                                | `./data`  | `/app/data`            |

> **存储连接串示例**：
>
> - Redis：`redis://redis:6379/0`
> - MySQL：`mysql+aiomysql://user:password@host:3306/db`（若填 `mysql://` 会自动转为 `mysql+aiomysql://`）
> - PostgreSQL：`postgresql+asyncpg://user:password@host:5432/db`

### 可用次数

- Basic 账号：80 次 / 20h
- Super 账号：140 次 / 2h

### 可用模型

| 模型名                   | 计次 | 可用账号    | 对话功能 | 图像功能 | 视频功能 |
| :----------------------- | :--: | :---------- | :------: | :------: | :------: |
| `grok-3`                 |  1   | Basic/Super |   支持   |   支持   |    -     |
| `grok-3-mini`            |  1   | Basic/Super |   支持   |   支持   |    -     |
| `grok-3-thinking`        |  1   | Basic/Super |   支持   |   支持   |    -     |
| `grok-4`                 |  1   | Basic/Super |   支持   |   支持   |    -     |
| `grok-4-fast`            |  1   | Basic/Super |   支持   |   支持   |    -     |
| `grok-4-mini`            |  1   | Basic/Super |   支持   |   支持   |    -     |
| `grok-4-thinking`        |  1   | Basic/Super |   支持   |   支持   |    -     |
| `grok-4-expert`          |  4   | Basic/Super |   支持   |   支持   |    -     |
| `grok-4-heavy`           |  4   | Super       |   支持   |   支持   |    -     |
| `grok-4.1`               |  1   | Basic/Super |   支持   |   支持   |    -     |
| `grok-4.1-mini`          |  1   | Basic/Super |   支持   |   支持   |    -     |
| `grok-4.1-fast`          |  1   | Basic/Super |   支持   |   支持   |    -     |
| `grok-4.1-thinking`      |  4   | Basic/Super |   支持   |   支持   |    -     |
| `grok-4.1-expert`        |  4   | Basic/Super |   支持   |   支持   |    -     |
| `grok-imagine-1.0`       |  4   | Basic/Super |    -     |   支持   |    -     |
| `grok-imagine-1.0-video` |  -   | Basic/Super |    -     |    -     |   支持   |

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

| 字段                | 类型    | 说明                     | 可用参数                            |
| :------------------ | :------ | :----------------------- | :---------------------------------- |
| `model`             | string  | 模型名称                 | 见上方模型列表                      |
| `messages`          | array   | 消息列表                 | 见下方消息格式                      |
| `stream`            | boolean | 是否开启流式输出         | `true`, `false`                     |
| `thinking`          | string  | 思维链模式               | `enabled`, `disabled`, `null`       |
| `video_config`      | object  | **视频模型专用配置对象** | -                                   |
| └─`aspect_ratio`    | string  | 视频宽高比               | `16:9`, `9:16`, `1:1`, `2:3`, `3:2` |
| └─`video_length`    | integer | 视频时长 (秒)            | `6`, `10`                           |
| └─`resolution_name` | string  | 分辨率                   | `480p`, `720p`                      |
| └─`preset`          | string  | 风格预设                 | `fun`, `normal`, `spicy`, `custom`  |

**消息格式 (messages)**：

| 字段      | 类型         | 说明                                             |
| :-------- | :----------- | :----------------------------------------------- |
| `role`    | string       | 角色：`developer`, `system`, `user`, `assistant` |
| `content` | string/array | 消息内容，支持纯文本或多模态数组                 |

**多模态内容块类型 (content array)**：

| type        | 说明     | 示例                                                         |
| :---------- | :------- | :----------------------------------------------------------- |
| `text`      | 文本内容 | `{"type": "text", "text": "描述这张图片"}`                   |
| `image_url` | 图片 URL | `{"type": "image_url", "image_url": {"url": "https://..."}}` |
| `file`      | 文件     | `{"type": "file", "file": {"url": "https://..."}}`           |

注：除上述外的其他参数将自动丢弃并忽略

<br>

</details>

### `POST /v1/images/generations`

> 图像接口，支持图像生成、图像编辑

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

| 字段              | 类型    | 说明             | 可用参数                             |
| :---------------- | :------ | :--------------- | :----------------------------------- |
| `model`           | string  | 图像模型名       | `grok-imagine-1.0`                   |
| `prompt`          | string  | 图像描述提示词   | -                                    |
| `n`               | integer | 生成数量         | `1` - `10` (流式模式仅限 `1` 或 `2`) |
| `stream`          | boolean | 是否开启流式输出 | `true`, `false`                      |
| `size`            | string  | 图片尺寸         | `1024x1024` (暂不支持自定义)         |
| `quality`         | string  | 图片质量         | `standard` (暂不支持自定义)          |
| `response_format` | string  | 响应格式         | `url`, `b64_json`                    |
| `style`           | string  | 风格             | - (暂不支持)                         |

注：`size`、`quality`、`style` 参数为 OpenAI 兼容保留，当前版本暂不支持自定义

<br>

</details>

<br>

## 参数配置

配置文件：`data/config.toml`（首次启动从 `config.defaults.toml` 自动生成）

> [!NOTE]
> 生产环境或反向代理部署时，请确保 `app.app_url` 配置为对外可访问的完整 URL，
> 否则可能出现文件访问链接不正确或 403 等问题。

### `[app]` 应用设置

| 字段           | 说明                                                   | 默认值                  |
| :------------- | :----------------------------------------------------- | :---------------------- |
| `app_url`      | 服务外部访问 URL，用于文件链接生成                     | `http://127.0.0.1:8000` |
| `app_username` | 管理后台用户名                                         | `admin`                 |
| `app_password` | 管理后台密码（必填，首次启动请立即修改）               | `CHANGE_ME_NOW`         |
| `api_key`      | 调用 API 的密钥（可选，为空则不校验）                  | `""`                    |
| `image_format` | 图片输出格式                                           | `url`                   |
| `video_format` | 视频输出格式（`html` = Markdown 链接，`url` = 纯 URL） | `html`                  |

### `[network]` 网络配置

| 字段              | 说明                           | 默认值 |
| :---------------- | :----------------------------- | :----- |
| `timeout`         | 请求 Grok 服务超时时间（秒）   | `120`  |
| `base_proxy_url`  | Grok 官网基础代理地址          | `""`   |
| `asset_proxy_url` | Grok 静态资源（图片/视频）代理 | `""`   |

### `[security]` 安全配置

| 字段                    | 说明                     | 默认值                           |
| :---------------------- | :----------------------- | :------------------------------- |
| `cf_clearance`          | Cloudflare 验证 Cookie   | `""`                             |
| `browser`               | 浏览器指纹标识           | `chrome136`                      |
| `allow_anonymous_api`   | 是否允许匿名调用 API     | `false`                          |
| `allow_anonymous_admin` | 是否允许匿名访问管理后台 | `false`                          |
| `cors_allow_origins`    | CORS 白名单              | `["http://127.0.0.1:8000", ...]` |
| `max_body_size_mb`      | 请求体大小上限（MB）     | `50`                             |
| `rate_limit_enabled`    | 是否开启 IP 限速         | `true`                           |
| `rate_limit_per_minute` | 每分钟请求上限           | `120`                            |
| `rate_limit_burst`      | 突发请求上限             | `60`                             |

### `[chat]` 对话配置

| 字段              | 说明                           | 默认值                                                  |
| :---------------- | :----------------------------- | :------------------------------------------------------ |
| `temporary`       | 是否启用临时对话模式           | `true`                                                  |
| `disable_memory`  | 是否禁用对话记忆               | `true`                                                  |
| `stream`          | 是否默认开启流式输出           | `true`                                                  |
| `thinking`        | 是否启用思维链输出             | `true`                                                  |
| `dynamic_statsig` | 是否启用动态 Statsig 指纹      | `true`                                                  |
| `filter_tags`     | 自动过滤 Grok 响应中的特殊标签 | `["xaiartifact", "xai:tool_usage_card", "grok:render"]` |

### `[retry]` 重试策略

| 字段                   | 说明                   | 默认值            |
| :--------------------- | :--------------------- | :---------------- |
| `max_retry`            | 最大重试次数           | `3`               |
| `retry_status_codes`   | 触发重试的 HTTP 状态码 | `[401, 429, 403]` |
| `retry_backoff_base`   | 退避基础延迟（秒）     | `0.5`             |
| `retry_backoff_factor` | 退避指数倍率           | `2.0`             |
| `retry_backoff_max`    | 单次重试最大等待（秒） | `30.0`            |
| `retry_budget`         | 重试总耗时预算（秒）   | `90.0`            |

### `[timeout]` 超时配置

| 字段                              | 说明                   | 默认值  |
| :-------------------------------- | :--------------------- | :------ |
| `stream_idle_timeout`             | 流式响应空闲超时（秒） | `120.0` |
| `video_idle_timeout`              | 视频生成空闲超时（秒） | `90.0`  |
| `video_result_wait_timeout`       | 视频结果等待超时（秒） | `90.0`  |
| `video_result_poll_interval`      | 视频结果轮询间隔（秒） | `1.0`   |
| `video_result_scan_pages`         | 视频结果扫描页数       | `12`    |
| `video_result_scan_assets`        | 视频结果扫描资产数     | `800`   |
| `video_result_candidate_attempts` | 视频候选尝试次数       | `4`     |

### `[image]` 图片生成

| 字段                        | 说明                        | 默认值   |
| :-------------------------- | :-------------------------- | :------- |
| `image_ws`                  | 是否启用 WebSocket 图片生成 | `true`   |
| `image_ws_nsfw`             | WS 图片生成是否开启 NSFW    | `true`   |
| `image_ws_blocked_seconds`  | 被拦截后等待时间（秒）      | `15`     |
| `image_ws_final_min_bytes`  | 最终图片最小字节数          | `100000` |
| `image_ws_medium_min_bytes` | 中间图片最小字节数          | `30000`  |
| `image_ws_max_per_request`  | 单次请求最大图片数          | `6`      |

### `[token]` Token 池管理

| 字段                           | 说明                           | 默认值      |
| :----------------------------- | :----------------------------- | :---------- |
| `auto_refresh`                 | 是否开启自动刷新               | `true`      |
| `refresh_interval_hours`       | Basic Token 刷新间隔（小时）   | `8`         |
| `super_refresh_interval_hours` | Super Token 刷新间隔（小时）   | `2`         |
| `fail_threshold`               | 连续失败多少次后标记不可用     | `5`         |
| `save_delay_ms`                | Token 变更合并写入延迟（毫秒） | `2000`      |
| `reload_interval_sec`          | 多 worker 状态刷新间隔（秒）   | `120`       |
| `selection_strategy`           | 选择策略                       | `max_quota` |

> 选择策略可选：`max_quota`（优先高配额）、`random`（随机）、`weighted`（加权）、`lru`（最久未用）

### `[cache]` 缓存管理

| 字段                   | 说明                | 默认值 |
| :--------------------- | :------------------ | :----- |
| `enable_auto_clean`    | 是否启用自动清理    | `true` |
| `image_limit_mb`       | 图片缓存阈值（MB）  | `2048` |
| `video_limit_mb`       | 视频缓存阈值（MB）  | `4096` |
| `cleanup_target_ratio` | 清理目标比例（0~1） | `0.8`  |
| `cleanup_interval_sec` | 清理检查间隔（秒）  | `30`   |
| `cleanup_max_delete`   | 单次清理最大删除数  | `1000` |

### `[proxy]` 代理池

| 字段                  | 说明                 | 默认值 |
| :-------------------- | :------------------- | :----- |
| `proxy_url`           | 固定代理地址         | `""`   |
| `proxy_pool_url`      | 代理池 API 地址      | `""`   |
| `proxy_pool_interval` | 代理池刷新间隔（秒） | `300`  |

### `[stats]` 统计监控

| 字段                 | 说明                     | 默认值 |
| :------------------- | :----------------------- | :----- |
| `enabled`            | 是否启用统计             | `true` |
| `hourly_retention`   | 小时级数据保留时长（时） | `48`   |
| `daily_retention`    | 日级数据保留时长（天）   | `30`   |
| `log_max_entries`    | 日志最大条数             | `1000` |
| `flush_interval_sec` | 刷新间隔（秒）           | `2`    |

### `[performance]` 并发性能

| 字段                       | 说明               | 默认值 |
| :------------------------- | :----------------- | :----- |
| `nsfw_max_concurrent`      | NSFW 批量并发上限  | `50`   |
| `nsfw_batch_size`          | NSFW 批量批次大小  | `100`  |
| `nsfw_max_tokens`          | NSFW 批量最大数量  | `5000` |
| `usage_max_concurrent`     | 用量刷新并发上限   | `100`  |
| `usage_batch_size`         | 用量刷新批次大小   | `100`  |
| `usage_max_tokens`         | 用量刷新最大数量   | `5000` |
| `assets_max_concurrent`    | 资产操作并发上限   | `100`  |
| `assets_batch_size`        | 资产操作批次大小   | `50`   |
| `assets_max_tokens`        | 资产操作最大数量   | `5000` |
| `assets_delete_batch_size` | 资产删除批量       | `50`   |
| `media_max_concurrent`     | 媒体生成并发上限   | `200`  |
| `sse_keepalive_sec`        | SSE 心跳间隔（秒） | `15`   |

<br>

## Redis 存储优化

生产环境推荐使用 Redis 存储，相比 Local 存储具有以下优势：

- **原子配额消耗** — Lua 脚本实现「检查配额 → 扣减 → 更新统计 → 冷却切换」单次 RTT 完成，高并发下无竞态
- **原子批量更新** — 多 Token 字段更新通过 Lua 脚本事务性保证，避免部分更新
- **连接池管理** — 内置异步连接池，自动复用连接
- **多 Worker 共享** — 多进程部署时所有 worker 共享同一状态，配额实时一致
- **持久化** — Docker Compose 默认开启 AOF 持久化，容器重启数据不丢失

```yaml
# docker-compose.yml 默认已配置 Redis
environment:
  SERVER_STORAGE_TYPE: redis
  SERVER_STORAGE_URL: redis://redis:6379/0
```

<br>

## Star History

[![Star History Chart](https://api.star-history.com/svg?repos=CPU-JIA/grok2api&type=Timeline)](https://star-history.com/#CPU-JIA/grok2api&Timeline)
