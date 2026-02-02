# Grok2API

**中文** | [English](README.en.md)

> [!NOTE]
> 本项目仅供学习与研究，使用者必须在遵循 Grok 的 **使用条款** 以及 **法律法规** 的情况下使用，不得用于非法用途。

基于 **FastAPI** 重构的 Grok2API，全面适配最新 Web 调用格式，支持流/非流式对话、图像生成/编辑、深度思考，号池并发与自动负载均衡一体化。

<img width="2562" height="1280" alt="image" src="https://github.com/user-attachments/assets/356d772a-65e1-47bd-abc8-c00bb0e2c9cc" />

<br>

## 使用说明

### 如何启动

- 本地开发

```
uv sync

uv run main.py
```

- 项目部署

```
git clone https://github.com/chenyme/grok2api

docker compose up -d
```

### 管理面板

访问地址：`http://<host>:8000/admin`  
默认登录密码：`grok2api`（对应配置项 `app.app_key`，建议修改）。

### 环境变量

> 配置 `.env` 文件

| 变量名                  | 说明                                                | 默认值      | 示例                                                |
| :---------------------- | :-------------------------------------------------- | :---------- | :-------------------------------------------------- |
| `LOG_LEVEL`           | 日志级别                                            | `INFO`    | `DEBUG`                                           |
| `SERVER_HOST`         | 服务监听地址                                        | `0.0.0.0` | `0.0.0.0`                                         |
| `SERVER_PORT`         | 服务端口                                            | `8000`    | `8000`                                            |
| `SERVER_WORKERS`      | Uvicorn worker 数量                                 | `1`       | `2`                                               |
| `SERVER_STORAGE_TYPE` | 存储类型（`local`/`redis`/`mysql`/`pgsql`） | `local`   | `pgsql`                                           |
| `SERVER_STORAGE_URL`  | 存储连接串（local 时可为空）                        | `""`      | `postgresql+asyncpg://user:password@host:5432/db` |

### 可用次数

- Basic 账号：80 次 / 20h
- Super 账号：无账号，作者未测试

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

| 字段                 | 类型    | 说明                           | 可用参数                                           |
| :------------------- | :------ | :----------------------------- | :------------------------------------------------- |
| `model`            | string  | 模型名称                       | -                                                  |
| `messages`         | array   | 消息列表                       | `developer`, `system`, `user`, `assistant` |
| `stream`           | boolean | 是否开启流式输出               | `true`, `false`                                |
| `thinking`         | string  | 思维链模式                     | `enabled`, `disabled`, `null`                |
| `video_config`     | object  | **视频模型专用配置对象** | -                                                  |
| └─`aspect_ratio` | string  | 视频宽高比                     | `16:9`, `9:16`, `1:1`, `2:3`, `3:2`      |
| └─`video_length` | integer | 视频时长 (秒)                  | `5` - `15`                                     |
| └─`resolution`   | string  | 分辨率                         | `SD`, `HD`                                     |
| └─`preset`       | string  | 风格预设                       | `fun`, `normal`, `spicy`                     |

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

| 字段       | 类型    | 说明             | 可用参数                                     |
| :--------- | :------ | :--------------- | :------------------------------------------- |
| `model`  | string  | 图像模型名       | `grok-imagine-1.0`                         |
| `prompt` | string  | 图像描述提示词   | -                                            |
| `n`      | integer | 生成数量         | `1` - `10` (流式模式仅限 `1` 或 `2`) |
| `stream` | boolean | 是否开启流式输出 | `true`, `false`                          |

注：除上述外的其他参数将自动丢弃并忽略

<br>

</details>

<br>

## 参数配置

配置文件：`data/config.toml`

> [!NOTE]
> 生产环境或反向代理部署时，请确保 `app.app_url` 配置为对外可访问的完整 URL，
> 否则可能出现文件访问链接不正确或 403 等问题。

| 模块                  | 字段                         | 配置名       | 说明                                                 | 默认值                                                    |
| :-------------------- | :--------------------------- | :----------- | :--------------------------------------------------- | :-------------------------------------------------------- |
| **app**         | `app_url`                  | 应用地址     | 当前 Grok2API 服务的外部访问 URL，用于文件链接访问。 | `http://127.0.0.1:8000`                                 |
|                       | `app_key`                  | 后台密码     | 登录 Grok2API 服务管理后台的密码，请妥善保管。       | `grok2api`                                              |
|                       | `api_key`                  | API 密钥     | 调用 Grok2API 服务所需的 Bearer Token，请妥善保管。  | `""`                                                    |
|                       | `image_format`             | 图片格式     | 生成的图片格式（url 或 base64）。                    | `url`                                                   |
|                       | `video_format`             | 视频格式     | 生成的视频格式（仅支持 url）。                       | `url`                                                   |
| **grok**        | `temporary`                | 临时对话     | 是否启用临时对话模式。                               | `true`                                                  |
|                       | `stream`                   | 流式响应     | 是否默认启用流式输出。                               | `true`                                                  |
|                       | `thinking`                 | 思维链       | 是否启用模型思维链输出。                             | `true`                                                  |
|                       | `dynamic_statsig`          | 动态指纹     | 是否启用动态生成 Statsig 值。                        | `true`                                                  |
|                       | `filter_tags`              | 过滤标签     | 自动过滤 Grok 响应中的特殊标签。                     | `["xaiartifact", "xai:tool_usage_card", "grok:render"]` |
|                       | `timeout`                  | 超时时间     | 请求 Grok 服务的超时时间（秒）。                     | `120`                                                   |
|                       | `base_proxy_url`           | 基础代理 URL | 代理请求到 Grok 官网的基础服务地址。                 | `""`                                                    |
|                       | `asset_proxy_url`          | 资源代理 URL | 代理请求到 Grok 官网的静态资源（图片/视频）地址。    | `""`                                                    |
|                       | `cf_clearance`             | CF Clearance | Cloudflare 验证 Cookie，用于验证 Cloudflare 的验证。 | `""`                                                    |
|                       | `max_retry`                | 最大重试     | 请求 Grok 服务失败时的最大重试次数。                 | `3`                                                     |
|                       | `retry_status_codes`       | 重试状态码   | 触发重试的 HTTP 状态码列表。                         | `[401, 429, 403]`                                       |
| **token**       | `auto_refresh`             | 自动刷新     | 是否开启 Token 自动刷新机制。                        | `true`                                                  |
|                       | `refresh_interval_hours`   | 刷新间隔     | Token 刷新的时间间隔（小时）。                       | `8`                                                     |
|                       | `fail_threshold`           | 失败阈值     | 单个 Token 连续失败多少次后被标记为不可用。          | `5`                                                     |
|                       | `save_delay_ms`            | 保存延迟     | Token 变更合并写入的延迟（毫秒）。                   | `500`                                                   |
|                       | `reload_interval_sec`      | 一致性刷新   | 多 worker 场景下 Token 状态刷新间隔（秒）。          | `30`                                                    |
| **cache**       | `enable_auto_clean`        | 自动清理     | 是否启用缓存自动清理，开启后按上限自动回收。         | `true`                                                  |
|                       | `limit_mb`                 | 清理阈值     | 缓存大小阈值（MB），超过阈值会触发清理。             | `1024`                                                  |
| **performance** | `assets_max_concurrent`    | 资产并发上限 | 资源上传/下载/列表的并发上限。推荐 25。              | `25`                                                    |
|                       | `media_max_concurrent`     | 媒体并发上限 | 视频/媒体生成请求的并发上限。推荐 50。               | `50`                                                    |
|                       | `usage_max_concurrent`     | 用量并发上限 | 用量查询请求的并发上限。推荐 25。                    | `25`                                                    |
|                       | `assets_delete_batch_size` | 资产清理批量 | 在线资产删除单批并发数量。推荐 10。                  | `10`                                                    |
|                       | `admin_assets_batch_size`  | 管理端批量   | 管理端在线资产统计/清理批量并发数量。推荐 10。       | `10`                                                    |

<br>

## Star History

[![Star History Chart](https://api.star-history.com/svg?repos=Chenyme/grok2api&type=Timeline)](https://star-history.com/#Chenyme/grok2api&Timeline)
