# Grok2API Frontend

Grok2API 的管理端 SPA，用于管理账号池、模型路由、客户端密钥、请求审计和运行设置。

## 技术栈

- React 19 + TypeScript
- Vite 8 + Tailwind CSS
- shadcn/ui + Radix UI
- TanStack Query、React Hook Form、Zod

## 本地开发

先启动根目录中的 Go 后端，再运行：

```bash
cd frontend
pnpm install
pnpm dev
```

开发服务器默认地址为 `http://127.0.0.1:5173`，并将 `/api`、`/v1`、`/healthz` 和 `/readyz` 代理到 `http://127.0.0.1:8000`。

需要使用其他后端地址时：

```bash
VITE_DEV_API_TARGET=http://127.0.0.1:9000 pnpm dev
```

## 生产构建

```bash
pnpm build
```

构建结果输出到 `dist/`。后端通过根配置中的 `frontend.staticPath` 同源托管该目录，并为非 API 路径提供 SPA 回退。前端不读取原始 YAML，公开运行信息由后端受控接口提供。

## 代码结构

```text
src/app/             路由与应用壳层
src/features/        按业务能力组织的页面与交互
src/entities/        领域 DTO 与查询接口
src/shared/          API、鉴权、配置、组件和通用工具
src/components/ui/   shadcn/ui 基础组件
```

业务请求统一通过 `shared/api`，服务端状态由 TanStack Query 管理；页面只组合业务能力，不直接维护重复的请求、鉴权或格式化逻辑。

## 验证

```bash
pnpm lint
pnpm build
```
