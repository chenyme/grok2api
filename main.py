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

BASE_DIR = Path(__file__).resolve().parent
PUBLIC_DIR = BASE_DIR / "_public"

# Ensure the project root is on sys.path (helps when Vercel sets a different CWD)
if str(BASE_DIR) not in sys.path:
    sys.path.insert(0, str(BASE_DIR))

env_file = BASE_DIR / ".env"
if env_file.exists():
    load_dotenv(env_file)

from fastapi import FastAPI, Request  # noqa: E402
from fastapi.middleware.cors import CORSMiddleware  # noqa: E402
from fastapi import Depends  # noqa: E402

from app.core.auth import verify_api_key  # noqa: E402
from app.core.config import config, get_config  # noqa: E402
from app.core.logger import logger, setup_logging  # noqa: E402
from app.core.exceptions import register_exception_handlers  # noqa: E402
from app.core.response_middleware import ResponseLoggerMiddleware  # noqa: E402
from app.api.v1.chat import router as chat_router  # noqa: E402
from app.api.v1.image import router as image_router  # noqa: E402
from app.api.v1.video import router as video_router  # noqa: E402
from app.api.v1.files import router as files_router  # noqa: E402
from app.api.v1.models import router as models_router  # noqa: E402
from app.api.v1.response import router as responses_router  # noqa: E402
from app.services.token import get_scheduler  # noqa: E402
from app.api.v1.admin import router as admin_router
from app.api.v1.function import router as function_router
from app.api.pages import router as pages_router
from fastapi.responses import RedirectResponse
from fastapi.staticfiles import StaticFiles

# 初始化日志
setup_logging(
    level=os.getenv("LOG_LEVEL", "INFO"), json_console=False, file_logging=True
)


@asynccontextmanager
async def lifespan(app: FastAPI):
    """应用生命周期管理"""
    # 1. 注册服务默认配置
    from app.core.config import config, register_defaults
    from app.services.grok.defaults import get_grok_defaults

    register_defaults(get_grok_defaults())

    # 2. 加载配置
    await config.ensure_loaded()

    # 3. 启动服务显示
    logger.info("Starting Grok2API...")
    logger.info(f"Platform: {platform.system()} {platform.release()}")
    logger.info(f"Python: {sys.version.split()[0]}")

    # 4. 启动 Token 刷新调度器
    refresh_enabled = get_config("token.auto_refresh", True)
    if refresh_enabled:
        basic_interval = get_config("token.refresh_interval_hours", 8)
        super_interval = get_config("token.super_refresh_interval_hours", 2)
        interval = min(basic_interval, super_interval)
        scheduler = get_scheduler(interval)
        scheduler.start()

    # 5. 启动 cf_clearance 自动刷新
    #    环境变量 FLARESOLVERR_URL 会作为初始值写入配置（兼容旧部署方式）
    _flaresolverr_env = os.getenv("FLARESOLVERR_URL", "")
    if _flaresolverr_env and not get_config("proxy.flaresolverr_url"):
        await config.update({
            "proxy": {
                "enabled": True,
                "flaresolverr_url": _flaresolverr_env,
                "refresh_interval": int(os.getenv("CF_REFRESH_INTERVAL", "600")),
                "timeout": int(os.getenv("CF_TIMEOUT", "60")),
            }
        })

    from app.services.cf_refresh import start as cf_refresh_start
    cf_refresh_start()

    logger.info("Application startup complete.")
    yield

    # 关闭
    logger.info("Shutting down Grok2API...")

    from app.services.cf_refresh import stop as cf_refresh_stop
    cf_refresh_stop()

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
    app.add_middleware(
        CORSMiddleware,
        allow_origins=["*"],
        allow_credentials=True,
        allow_methods=["*"],
        allow_headers=["*"],
    )

    # 请求日志和 ID 中间件
    app.add_middleware(ResponseLoggerMiddleware)

    @app.middleware("http")
    async def ensure_config_loaded(request: Request, call_next):
        await config.ensure_loaded()
        return await call_next(request)

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
        responses_router, prefix="/v1", dependencies=[Depends(verify_api_key)]
    )
    app.include_router(
        video_router, prefix="/v1", dependencies=[Depends(verify_api_key)]
    )
    app.include_router(files_router, prefix="/v1/files")

    # 静态文件服务（统一使用 /_public/static）
    static_dir = PUBLIC_DIR / "static"
    if static_dir.exists():
        app.mount("/static", StaticFiles(directory=static_dir), name="static")

    # 注册管理与功能玩法路由
    app.include_router(admin_router, prefix="/v1/admin")
    app.include_router(function_router, prefix="/v1/function")
    app.include_router(pages_router)

    @app.get("/favicon.ico", include_in_schema=False)
    def favicon():
        return RedirectResponse(url="/static/common/img/favicon/favicon.ico")
    
    # 健康检查接口（用于 Render、服务器保活检测等）
    @app.get("/health")
    def health():
        """
        健康检查接口，用于服务器保活或 Render 自动检测
        """
        return {"status": "ok"}

    return app    


app = create_app()


if __name__ == "__main__":
    host = os.getenv("SERVER_HOST", "0.0.0.0")
    port = int(os.getenv("SERVER_PORT", "8000"))
    workers = int(os.getenv("SERVER_WORKERS", "1"))
    log_level = os.getenv("LOG_LEVEL", "INFO").lower()
    logger.error(
        "Direct startup via `python main.py` is disabled. "
        "Please run with Granian CLI to avoid Python wrapper issues."
    )
    logger.error(
        "Use: uv run granian --interface asgi "
        f"--host {host} --port {port} --workers {workers} "
        f"--log-level {log_level} main:app"
    )
    raise SystemExit(1)
