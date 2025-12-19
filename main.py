"""Grok2API"""

import os
import sys
from contextlib import asynccontextmanager
from pathlib import Path

from fastapi import FastAPI
from fastapi.middleware.cors import CORSMiddleware
from fastapi.staticfiles import StaticFiles
from app.core.logger import logger
from app.core.exception import register_exception_handlers
from app.core.storage import storage_manager
from app.core.config import setting
from app.services.grok.token import token_manager
from app.api.v1.chat import router as chat_router
from app.api.v1.models import router as models_router
from app.api.v1.images import router as images_router
from app.api.admin.manage import router as admin_router
from app.services.mcp import mcp

# 0. 兼容性检测
try:
    if sys.platform != 'win32':
        import uvloop
        uvloop.install()
        logger.info("[Grok2API] 启用uvloop高性能事件循环")
    else:
        logger.info("[Grok2API] Windows系统，使用默认asyncio事件循环")
except ImportError:
    logger.info("[Grok2API] uvloop未安装，使用默认asyncio事件循环")

# 1. 创建MCP的FastAPI应用实例
# 使用流式HTTP传输，支持高效的双向流式通信
mcp_app = mcp.http_app(stateless_http=True, transport="streamable-http")

# 2. 定义应用生命周期
@asynccontextmanager
async def lifespan(app: FastAPI):
    """
    启动顺序:
    1. 初始化核心服务 (storage, settings, token_manager)
    2. 异步加载 token 数据
    3. 启动批量保存任务
    4. 启动MCP服务生命周期
    
    关闭顺序 (LIFO):
    1. 关闭MCP服务生命周期
    2. 关闭批量保存任务并刷新数据
    3. 关闭核心服务
    """
    # --- 启动过程 ---
    # 1. 初始化核心服务
    await storage_manager.init()

    # 设置存储到配置和token管理器
    storage = storage_manager.get_storage()
    setting.set_storage(storage)
    token_manager.set_storage(storage)
    
    # 2. 重新加载配置
    await setting.reload()
    logger.info("[Grok2API] 核心服务初始化完成")
    
    # 2.5. 初始化代理池
    from app.core.proxy_pool import proxy_pool
    proxy_url = setting.grok_config.get("proxy_url", "")
    proxy_pool_url = setting.grok_config.get("proxy_pool_url", "")
    proxy_pool_interval = setting.grok_config.get("proxy_pool_interval", 300)
    proxy_pool.configure(proxy_url, proxy_pool_url, proxy_pool_interval)
    
    # 3. 异步加载 token 数据
    await token_manager._load_data()
    logger.info("[Grok2API] Token数据加载完成")
    
    # 4. 启动批量保存任务
    await token_manager.start_batch_save()

    # 5. 管理MCP服务的生命周期
    mcp_lifespan_context = mcp_app.lifespan(app)
    await mcp_lifespan_context.__aenter__()
    logger.info("[MCP] MCP服务初始化完成")

    logger.info("[Grok2API] 应用启动成功")
    
    try:
        yield
    finally:
        # --- 关闭过程 ---
        # 1. 退出MCP服务的生命周期
        await mcp_lifespan_context.__aexit__(None, None, None)
        logger.info("[MCP] MCP服务已关闭")
        
        # 2. 关闭批量保存任务并刷新数据
        await token_manager.shutdown()
        logger.info("[Token] Token管理器已关闭")
        
        # 3. 关闭核心服务
        await storage_manager.close()
        logger.info("[Grok2API] 应用关闭成功")


# 初始化日志
logger.info("[Grok2API] 应用正在启动...")

# 创建FastAPI应用
app = FastAPI(
    title="Grok2API",
    description="Grok API 转换服务",
    version="1.3.1",
    lifespan=lifespan
)

# 注册全局异常处理器
register_exception_handlers(app)

# 注册路由
app.include_router(chat_router, prefix="/v1")
app.include_router(models_router, prefix="/v1")
app.include_router(images_router)
app.include_router(admin_router)

# 挂载静态文件
app.mount("/static", StaticFiles(directory="app/template"), name="template")

@app.get("/")
async def root():
    """根路径"""
    from fastapi.responses import RedirectResponse
    return RedirectResponse(url="/login")


@app.get("/health")
async def health_check():
    """健康检查接口"""
    return {
        "status": "healthy",
        "service": "Grok2API",
        "version": "1.0.3"
    }

# 挂载MCP服务器 
app.mount("", mcp_app)


if __name__ == "__main__":
    import uvicorn
    import os
    
    # 读取 worker 数量，默认为 1
    workers = int(os.getenv("WORKERS", "1"))
    
    # 提示多进程模式
    if workers > 1:
        logger.info(
            f"[Grok2API] 多进程模式已启用 (workers={workers})。"
            f"建议使用 Redis/MySQL 存储以获得最佳性能。"
        )
    
    # 确定事件循环类型
    loop_type = "auto"
    if workers == 1 and sys.platform != 'win32':
        try:
            import uvloop
            loop_type = "uvloop"
        except ImportError:
            pass
    
    uvicorn.run(
        "main:app",
        host="0.0.0.0",
        port=8001,
        workers=workers,
        loop=loop_type
    )