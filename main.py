"""
Grok2API 应用入口

FastAPI 应用初始化和路由注册
"""

from contextlib import asynccontextmanager
import os
import platform
import sys
from pathlib import Path

from dotenv import load_dotenv

env_file = Path(__file__).parent / ".env"
if env_file.exists():
    load_dotenv(env_file)

from fastapi import FastAPI  # noqa: E402
from fastapi.middleware.cors import CORSMiddleware  # noqa: E402
from fastapi import Depends  # noqa: E402
from fastapi.responses import FileResponse  # noqa: E402

from app.core.auth import verify_api_key  # noqa: E402
from app.core.config import get_config  # noqa: E402
from app.core.logger import logger, setup_logging  # noqa: E402
from app.core.exceptions import register_exception_handlers  # noqa: E402
from app.core.response_middleware import ResponseLoggerMiddleware  # noqa: E402
from app.core.security_middleware import (  # noqa: E402
    BodySizeLimitMiddleware,
    RateLimitMiddleware,
)
from app.api.v1.chat import router as chat_router  # noqa: E402
from app.api.v1.image import router as image_router  # noqa: E402
from app.api.v1.video import router as video_router  # noqa: E402
from app.api.v1.files import router as files_router  # noqa: E402
from app.api.v1.models import router as models_router  # noqa: E402
from app.api.v1.health import router as health_router  # noqa: E402
from app.services.token import get_scheduler  # noqa: E402

# 初始化日志
setup_logging(
    level=os.getenv("LOG_LEVEL", "INFO"), json_console=False, file_logging=True
)


@asynccontextmanager
async def lifespan(app: FastAPI):
    """应用生命周期管理"""
    # 1. 加载配置
    from app.core.config import config

    await config.load()

    # 安全检查：拒绝使用默认密码启动（提供环境变量逃生舱）
    app_password = get_config("app.app_password", "")
    if app_password in ("grok2api", "CHANGE_ME_NOW"):
        if os.getenv("ALLOW_DEFAULT_PASSWORD", "").lower() in ("true", "1", "yes"):
            logger.warning(
                f"Default app_password ({app_password}) in use. Change before production."
            )
        else:
            raise RuntimeError(
                f"Refusing to start with default password '{app_password}'. "
                "Set a strong password in config.toml or set ALLOW_DEFAULT_PASSWORD=true to override."
            )
    if not get_config("app.api_key", ""):
        logger.warning(
            "Admin api_key is not configured. Admin access will rely on app_password only."
        )

    # 2. 初始化代理池
    proxy_url = get_config("proxy.proxy_url", "") or get_config(
        "network.base_proxy_url", ""
    )
    proxy_pool_url = get_config("proxy.proxy_pool_url", "")
    if proxy_url or proxy_pool_url:
        from app.core.proxy_pool import proxy_pool

        proxy_pool.configure(
            proxy_url=proxy_url,
            proxy_pool_url=proxy_pool_url,
            proxy_pool_interval=get_config("proxy.proxy_pool_interval", 300),
        )

    # 3. 初始化统计/日志服务
    if get_config("stats.enabled", True):
        from app.services.stats import request_stats, request_logger

        await request_stats.init()
        await request_logger.init()
        logger.info("Stats and logging services initialized")

    # 4. 启动服务显示
    logger.info("Starting Grok2API...")
    logger.info(f"Platform: {platform.system()} {platform.release()}")
    logger.info(f"Python: {sys.version.split()[0]}")

    # 5. 启动 Token 刷新调度器
    refresh_enabled = get_config("token.auto_refresh", True)
    if refresh_enabled:
        basic_interval = get_config("token.refresh_interval_hours", 8)
        super_interval = get_config("token.super_refresh_interval_hours", 2)
        interval = min(basic_interval, super_interval)
        scheduler = get_scheduler(interval)
        scheduler.start()

    logger.info("Application startup complete.")
    yield

    # 关闭
    logger.info("Shutting down Grok2API...")

    # 关闭共享 HTTP 会话池
    from app.services.grok.services.session_pool import close_all_sessions

    await close_all_sessions()

    # flush 统计数据
    if get_config("stats.enabled", True):
        try:
            from app.services.stats import request_stats, request_logger

            await request_stats._save_data()
            await request_logger._save_data()
            logger.info("Stats flushed on shutdown")
        except Exception as e:
            logger.warning(f"Failed to flush stats on shutdown: {e}")

    from app.core.storage import StorageFactory

    if StorageFactory._instance:
        await StorageFactory._instance.close()

    if refresh_enabled:
        scheduler = get_scheduler()
        scheduler.stop()


def create_app() -> FastAPI:
    """创建 FastAPI 应用"""
    app = FastAPI(
        title="Grok2API",
        lifespan=lifespan,
    )

    # CORS 配置
    cors_origins = get_config(
        "security.cors_allow_origins",
        ["http://127.0.0.1:8000", "http://localhost:8000"],
    )
    if isinstance(cors_origins, str):
        cors_origins = [
            item.strip() for item in cors_origins.split(",") if item.strip()
        ]
    if not isinstance(cors_origins, list) or not cors_origins:
        cors_origins = ["http://127.0.0.1:8000", "http://localhost:8000"]

    allow_credentials = bool(get_config("security.cors_allow_credentials", True))
    if "*" in cors_origins and allow_credentials:
        logger.warning(
            "CORS misconfiguration detected: wildcard origin with credentials. "
            "Forcing allow_credentials=False."
        )
        allow_credentials = False

    app.add_middleware(
        CORSMiddleware,
        allow_origins=cors_origins,
        allow_credentials=allow_credentials,
        allow_methods=["*"],
        allow_headers=["*"],
    )

    # 安全中间件
    app.add_middleware(BodySizeLimitMiddleware)
    app.add_middleware(RateLimitMiddleware)

    # 请求日志和 ID 中间件
    app.add_middleware(ResponseLoggerMiddleware)

    # 注册异常处理器
    register_exception_handlers(app)

    # 注册路由
    app.include_router(
        chat_router, prefix="/v1", dependencies=[Depends(verify_api_key)]
    )
    app.include_router(
        image_router, prefix="/v1", dependencies=[Depends(verify_api_key)]
    )
    app.include_router(
        models_router, prefix="/v1", dependencies=[Depends(verify_api_key)]
    )
    app.include_router(
        video_router, prefix="/v1", dependencies=[Depends(verify_api_key)]
    )
    app.include_router(
        files_router, prefix="/v1/files", dependencies=[Depends(verify_api_key)]
    )
    app.include_router(health_router)

    # 静态文件服务
    from fastapi.staticfiles import StaticFiles

    static_dir = Path(__file__).parent / "app" / "static"
    if static_dir.exists():
        app.mount("/static", StaticFiles(directory=static_dir), name="static")

        favicon_path = static_dir / "favicon" / "favicon.ico"
        if favicon_path.exists():

            @app.get("/favicon.ico", include_in_schema=False)
            async def favicon():
                return FileResponse(favicon_path)

    # 注册管理路由
    from app.api.v1.admin import router as admin_router

    app.include_router(admin_router)

    return app


app = create_app()


if __name__ == "__main__":
    import uvicorn

    host = os.getenv("SERVER_HOST", "0.0.0.0")
    port = int(os.getenv("SERVER_PORT", "8000"))
    workers = int(os.getenv("SERVER_WORKERS", "1"))

    # 平台检查
    is_windows = platform.system() == "Windows"

    # 自动降级
    if is_windows and workers > 1:
        logger.warning(
            f"Windows platform detected. Multiple workers ({workers}) is not supported. "
            "Using single worker instead."
        )
        workers = 1

    uvicorn.run(
        "main:app",
        host=host,
        port=port,
        workers=workers,
        log_level=os.getenv("LOG_LEVEL", "INFO").lower(),
    )
