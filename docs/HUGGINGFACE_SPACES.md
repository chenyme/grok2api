# Hugging Face Spaces 部署

本项目可直接以 **Docker Space** 方式部署到 Hugging Face Spaces，默认使用 **local 存储**，不依赖 Redis / MySQL / PostgreSQL。

## 适配目标

- 默认 `SERVER_STORAGE_TYPE=local`
- 不额外引入数据库、缓存服务等依赖
- 在 Hugging Face Spaces 环境下自动：
  - 优先使用 `/data` 作为数据目录
  - 若 `/data` 不可用，则回退到临时目录
  - 关闭文件日志 `LOG_FILE_ENABLED=false`
  - 保持单进程 `SERVER_WORKERS=1`
  - 若平台注入 `PORT`，自动映射到 `SERVER_PORT`

## 1. 创建 Docker Space

创建 Space 时选择 **Docker** SDK。

根据 Hugging Face 官方文档，Docker Space 需要在 **Space 仓库根目录的 `README.md` 顶部**保留 YAML 元数据。当前仓库已经直接内置了这段 metadata，核心字段如下：

```yaml
---
title: Grok2API
sdk: docker
app_port: 8000
pinned: false
---
```

如果你直接把当前仓库同步到 Hugging Face Space，根 `README.md` 会一并带上这些 Space 配置，无需再手动补。

## 2. 推荐 Variables / Secrets

最小可运行配置：

| Key | Value | 说明 |
| :-- | :-- | :-- |
| `SERVER_STORAGE_TYPE` | `local` | 默认本地存储 |
| `SERVER_STORAGE_URL` | `""` | local 模式留空 |
| `LOG_LEVEL` | `INFO` | 可选 |

通常 **不需要** 显式设置以下变量，因为入口脚本已自动兼容：

- `DATA_DIR`
- `LOG_DIR`
- `TMP_DIR`
- `LOG_FILE_ENABLED`
- `SERVER_PORT`

如果你的 Space 开启了持久化存储，应用会优先使用 `/data`；否则会自动回退到临时目录。

## 3. 持久化说明

Hugging Face 官方文档说明：

- 免费 Space 的磁盘是**易失**的，重启后会丢失内容
- 持久化存储挂载在 `/data`
- `/data` **只在运行时可用**，不能在 Docker build 阶段依赖它

本项目的适配逻辑正是按这个约束实现的：

- 运行时检测到 Space 环境后，优先将 `DATA_DIR` 指向 `/data`
- 如果 `/data` 不可写或不可用，自动回退到 `/tmp/grok2api-data`

## 4. 当前仓库已做的适配

### Docker 权限

Hugging Face 官方说明 Docker Space 容器运行时使用 **UID 1000**。  
因此当前仓库已在 `Dockerfile` 中把 `/app` 和 `/data` 相关目录所有权调整为 `1000:1000`，避免运行期写配置、token、缓存目录时报权限错误。

### 启动脚本

`scripts/entrypoint.sh` 已增加 Hugging Face Spaces 环境检测：

- 识别 `SPACE_ID` / `SPACE_HOST`
- 自动设置 local 存储默认值
- 自动处理 `PORT -> SERVER_PORT`
- 自动选择 `DATA_DIR` / `LOG_DIR` / `TMP_DIR`
- 自动关闭文件日志

## 5. 部署后建议检查

部署成功后建议确认：

1. `GET /health` 返回 `{"status":"ok"}`
2. `GET /admin` 能正常打开
3. 首次启动后已生成：
   - `config.toml`
   - `token.json`
4. 若配置了持久化存储，重启后配置和 token 文件仍然存在

## 6. 限制说明

Hugging Face 官方文档当前说明：

- Docker Space 默认对外端口是 `7860`，若应用监听其他端口，需要在 README YAML 中设置 `app_port`
- 对外网络请求通常只允许走 `80` / `443` / `8080`
- 免费 Space 会休眠，磁盘也是易失的

如果你依赖：

- 非标准端口代理
- 长驻后台刷新任务
- 持久化 token/配置

请结合自己的硬件等级、存储等级和代理出口再决定是否适合长期放在免费 Space 上。
