# cf_refresh - Cloudflare cf_clearance 自动刷新

通过 [FlareSolverr](https://github.com/FlareSolverr/FlareSolverr) 自动获取 Cloudflare `cf_clearance` cookie 和 `user_agent`，并更新到 Grok2API 服务配置中。

全自动、无需 GUI、服务器友好。

## 工作原理

1. FlareSolverr（独立 Docker 容器）内部运行 Chrome，自动通过 CF 挑战
2. cf_refresh 作为 grok2api 的后台任务，调用 FlareSolverr HTTP API 获取 `cf_clearance` 和 `user_agent`
3. 直接在进程内调用 `config.update()` 更新运行时配置并持久化到 `data/config.toml`
4. 按设定间隔重复以上步骤

## 配置方式

所有配置均可在管理面板 `/admin/config` 的 **CF 自动刷新** 区域中设置，也可通过环境变量初始化：

| 配置项 | 环境变量 | 默认值 | 说明 |
|--------|----------|--------|------|
| 启用自动刷新 | `FLARESOLVERR_URL`（非空即启用） | `false` | 是否开启自动刷新 |
| FlareSolverr 地址 | `FLARESOLVERR_URL` | — | FlareSolverr 服务的 HTTP 地址 |
| 刷新间隔（秒） | `CF_REFRESH_INTERVAL` | `600` | 定期刷新间隔 |
| 挑战超时（秒） | `CF_TIMEOUT` | `60` | CF 挑战等待超时 |

> **代理**：自动使用「代理配置 → 基础代理 URL」，无需单独设置，保证出口 IP 一致。

## 使用方式

### Docker Compose 部署

已集成在项目根目录 `docker-compose.yml` 中。只需在 grok2api 服务的环境变量中设置 `FLARESOLVERR_URL`，并添加 `flaresolverr` 服务即可：

```yaml
services:
  grok2api:
    environment:
      FLARESOLVERR_URL: http://flaresolverr:8191

  flaresolverr:
    image: ghcr.io/flaresolverr/flaresolverr:latest
    restart: unless-stopped
```

## 注意事项

- `cf_clearance` 与请求来源 IP 绑定，FlareSolverr 自动使用代理配置中的基础代理 URL 保证出口 IP 一致
- 启用自动刷新后，代理配置中的 CF Clearance、浏览器指纹和 User-Agent 由系统自动管理（面板中变灰）
- 建议刷新间隔不低于 5 分钟，避免触发 Cloudflare 频率限制
- FlareSolverr 需要约 500MB 内存（内部运行 Chrome）
