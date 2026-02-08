"""简化的主应用 - 移除 MCP 依赖和复杂启动逻辑"""

import os
import sys
import asyncio
from contextlib import asynccontextmanager
from pathlib import Path

from fastapi import FastAPI
from fastapi.middleware.cors import CORSMiddleware
from fastapi.staticfiles import StaticFiles

# 使用配置管理器
from app.core.config import setting

from app.core.logger import logger
from app.core.exception import register_exception_handlers
from app.core.storage import storage_manager
from app.services.grok.token import token_manager
from app.services.call_log import call_log_service
from app.api.v1.chat import router as chat_router
from app.api.v1.models import router as models_router
from app.api.v1.images import router as images_router
from app.api.admin.manage import router as admin_router
from app.services.mcp import mcp

# 兼容性检测
try:
    if sys.platform != "win32":
        import uvloop

        uvloop.install()
        logger.info("[Grok2API] 启用uvloop高性能事件循环")
    else:
        logger.info("[Grok2API] Windows系统，使用默认asyncio事件循环")
except ImportError:
    logger.info("[Grok2API] uvloop未安装，使用默认asyncio事件循环")

# 创建MCP应用实例
mcp_app = mcp.http_app(stateless_http=True, transport="streamable-http")


# 简化的应用生命周期
@asynccontextmanager
async def lifespan(app: FastAPI):
    """
    简化的启动顺序:
    1. 初始化核心服务 (storage, settings, token_manager)
    2. 异步加载 token 数据
    3. 启动批量保存任务

    关闭顺序 (LIFO):
    1. 关闭批量保存任务并刷新数据
    2. 关闭核心服务
    """
    logger.info("[Grok2API] 应用正在启动...")

    try:
        # 1. 初始化核心服务
        await storage_manager.init()
        storage = storage_manager.get_storage()
        setting.set_storage(storage)
        logger.info("[Grok2API] 核心服务初始化完成")

        # 2. 设置存储管理器
        token_manager.set_storage(storage)

        # 3. 异步加载数据
        await token_manager._load_data()
        logger.info("[Grok2API] Token数据加载完成")

        # 4. 启动调用日志服务
        await call_log_service.start()
        logger.info("[Grok2API] 调用日志服务启动完成")

        # 5. 启动后台任务
        token_manager._save_task = asyncio.create_task(
            token_manager._batch_save_worker()
        )
        token_manager._refresh_task = asyncio.create_task(
            token_manager._refresh_status_worker()
        )

        # 6. 初始化MCP服务
        mcp_lifespan_context = mcp_app.lifespan(app)
        await mcp_lifespan_context.__aenter__()
        app.state.mcp_lifespan = mcp_lifespan_context

        logger.info("[Grok2API] 应用启动成功")

        yield

    except Exception as e:
        logger.error(f"[Grok2API] 启动失败: {e}")
        raise
    finally:
        # 关闭顺序 (LIFO)
        logger.info("[Grok2API] 应用正在关闭...")

        # 1. 关闭MCP服务
        if hasattr(app.state, "mcp_lifespan"):
            try:
                await app.state.mcp_lifespan.__aexit__(None, None, None)
                logger.info("[MCP] MCP服务已关闭")
            except:
                pass

        # 2. 停止调用日志服务
        await call_log_service.shutdown()
        logger.info("[CallLog] 调用日志服务已关闭")

        # 2. 停止token管理器后台任务
        if hasattr(token_manager, "_save_task") and token_manager._save_task:
            token_manager._shutdown = True
            token_manager._save_task.cancel()
            token_manager._refresh_task.cancel()
            try:
                await token_manager._save_task
                await token_manager._refresh_task
            except asyncio.CancelledError:
                pass
            await token_manager._save_data()  # 最后保存
            logger.info("[Token] Token管理器已关闭")

        logger.info("[Grok2API] 应用关闭成功")


# 创建应用实例
app = FastAPI(
    title="Grok2API",
    description="Grok API 转换服务",
    version="1.0.3",
    lifespan=lifespan,
    redirect_slashes=False,
)

# 注册异常处理器
register_exception_handlers(app)

# 添加 CORS 中间件
app.add_middleware(
    CORSMiddleware,
    allow_origins=["*"],
    allow_credentials=True,
    allow_methods=["*"],
    allow_headers=["*"],
)

# 注册路由
app.include_router(chat_router, prefix="/v1", tags=["Chat"])
app.include_router(models_router, prefix="/v1", tags=["Models"])
app.include_router(images_router, prefix="/v1", tags=["Images"])
app.include_router(admin_router, prefix="", tags=["Admin"])


# 健康检查
@app.get("/health")
async def health():
    return {"status": "healthy", "service": "Grok2API", "version": "1.0.3"}


# 静态文件服务
try:
    static_path = Path(__file__).parent / "static"
    if static_path.exists():
        app.mount("/static", StaticFiles(directory=str(static_path)), name="static")
except Exception as e:
    logger.warning(f"[Grok2API] 静态文件服务初始化失败: {e}")

# 挂载MCP服务到/mcp路径 (添加末尾斜杠重定向处理)
app.mount("/mcp/", mcp_app)
app.mount("/mcp", mcp_app)
logger.info("[MCP] MCP服务已挂载到 /mcp 和 /mcp/")


if __name__ == "__main__":
    import uvicorn

    port = int(os.getenv("PORT", 8000))
    uvicorn.run(app, host="0.0.0.0", port=port)
