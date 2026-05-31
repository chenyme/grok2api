"""Grok2API application entry point.

Start with:
  uv run granian --interface asgi --host 0.0.0.0 --port 8000 --workers 1 app.main:app

Multi-worker notes:
  - All workers run a lightweight account-directory sync loop (ACCOUNT_SYNC_INTERVAL env, default 30 s).
  - Only the worker that wins the advisory file lock runs the heavy AccountRefreshScheduler.
    On Linux/macOS this uses fcntl.flock; on Windows it falls back to "always be leader"
    (i.e. every worker runs the scheduler — acceptable because Windows deployments are
    almost always single-worker anyway).
"""

import asyncio
import os
import platform
import sys
from contextlib import asynccontextmanager
from pathlib import Path

from dotenv import load_dotenv
from fastapi import FastAPI, Request
from fastapi.exceptions import RequestValidationError
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import JSONResponse
from fastapi.staticfiles import StaticFiles

from app.platform.logging.logger import logger, setup_logging, reload_logging
from app.platform.config.snapshot import config as _config
from app.platform.errors import AppError
from app.platform.meta import get_project_version
from app.platform.paths import data_path
from app.platform.storage import reconcile_local_media_cache_async


load_dotenv()


# ---------------------------------------------------------------------------
# Scheduler leader-election via advisory file lock
# ---------------------------------------------------------------------------

_LOCK_FILE = data_path(".scheduler.lock")
_lock_fd: int | None = None


def _try_acquire_scheduler_lock() -> bool:
    """Non-blocking attempt to become the scheduler leader.

    Returns True if this worker acquired the lock (i.e. is the leader).
    Falls back to True on Windows where fcntl is unavailable.
    """
    global _lock_fd
    try:
        import fcntl

        _LOCK_FILE.parent.mkdir(parents=True, exist_ok=True)
        fd = os.open(str(_LOCK_FILE), os.O_CREAT | os.O_WRONLY)
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        _lock_fd = fd
        return True
    except BlockingIOError:
        # Another worker already holds the lock.
        return False
    except (ImportError, OSError, AttributeError):
        # fcntl unavailable (Windows) or unexpected FS error — treat as leader.
        return True


def _release_scheduler_lock() -> None:
    global _lock_fd
    if _lock_fd is None:
        return
    try:
        import fcntl

        fcntl.flock(_lock_fd, fcntl.LOCK_UN)
        os.close(_lock_fd)
    except (ImportError, OSError):
        pass
    _lock_fd = None


# ---------------------------------------------------------------------------
# Early logging setup (before config is loaded)
# ---------------------------------------------------------------------------

setup_logging(
    level=os.getenv("LOG_LEVEL", "INFO"),
    file_logging=os.getenv("LOG_FILE_ENABLED", "true").strip().lower()
    in {"1", "true", "yes", "on"},
)


# ---------------------------------------------------------------------------
# Lifespan
# ---------------------------------------------------------------------------


@asynccontextmanager
async def lifespan(app: FastAPI):
    # 1. Load configuration.
    await _config.load()
    reload_logging(
        level=os.getenv("LOG_LEVEL", "INFO"),
        file_level=_config.get_str("logging.file_level", "") or None,
        max_files=_config.get_int("logging.max_files", 7),
    )
    logger.info(
        "application startup: service=grok2api python={} platform={}",
        sys.version.split()[0],
        platform.system(),
    )

    # 2. Initialise account repository and bootstrap runtime table.
    from app.control.account.backends.factory import (
        create_repository,
        describe_repository_target,
    )
    from app.control.account.runtime import (
        reconcile_refresh_runtime,
        set_refresh_scheduler,
        set_refresh_scheduler_leader,
        set_refresh_service,
    )
    from app.control.account.scheduler import get_account_refresh_scheduler
    from app.dataplane.account import get_account_directory

    storage_backend, storage_target = describe_repository_target()
    logger.info(
        "account storage configured: backend={} target={}",
        storage_backend,
        storage_target,
    )

    repo = create_repository()
    await repo.initialize()

    # 2a. First-boot migrations (config seed / account migration from SQLite).
    from app.platform.startup import run_startup_migrations

    await run_startup_migrations(
        config_backend=_config._get_backend(),
        account_repo=repo,
    )
    # Reload config in case it was just seeded/migrated into the backend.
    await _config.load()
    await reconcile_local_media_cache_async()

    directory = await get_account_directory(repo)

    # Expose repository on app.state for admin handlers.
    app.state.repository = repo
    app.state.directory = directory

    # 3. Account directory sync loop — all workers, lightweight incremental pull.
    #    Keeps each worker's in-memory table eventually consistent with the repo.
    #
    #    Adaptive interval strategy:
    #      - After detecting changes  → re-check in ACCOUNT_SYNC_ACTIVE_INTERVAL (default 3 s)
    #        so rapid back-to-back writes (bulk import, refresh cycle) are picked up quickly.
    #      - After N idle polls       → back off toward ACCOUNT_SYNC_INTERVAL (default 30 s).
    #    scan_changes() is an indexed DB query that costs < 1 ms when nothing changed,
    #    so polling aggressively after a change is essentially free.
    _SYNC_IDLE_INTERVAL = int(os.getenv("ACCOUNT_SYNC_INTERVAL", "30"))
    _SYNC_ACTIVE_INTERVAL = int(os.getenv("ACCOUNT_SYNC_ACTIVE_INTERVAL", "3"))
    _SYNC_IDLE_AFTER = 5  # consecutive empty polls before returning to idle pace

    async def _sync_loop() -> None:
        idle_streak = 0
        while True:
            interval = (
                _SYNC_ACTIVE_INTERVAL
                if idle_streak < _SYNC_IDLE_AFTER
                else _SYNC_IDLE_INTERVAL
            )
            await asyncio.sleep(interval)
            try:
                changed = await directory.sync_if_changed()
                idle_streak = 0 if changed else min(idle_streak + 1, _SYNC_IDLE_AFTER)
            except asyncio.CancelledError:
                raise
            except Exception as exc:
                logger.debug("account directory sync error: error={}", exc)
                idle_streak = _SYNC_IDLE_AFTER  # back off on errors

    sync_task = asyncio.create_task(_sync_loop(), name="account-dir-sync")

    # 4. Account refresh scheduler — only the leader worker.
    #    Uses an advisory file lock so exactly one process runs the heavy
    #    upstream quota-fetch loop regardless of worker count.
    #
    #    ``account.refresh.enabled`` selects between two independent runtime
    #    strategies:
    #      - true  → "quota" selector + scheduler running (default).
    #      - false → "random" selector + scheduler idle (no upstream probing).
    from app.control.account.refresh import AccountRefreshService

    refresh_enabled = _config.get_bool("account.refresh.enabled", False)

    refresh_svc = AccountRefreshService(repo)
    set_refresh_service(refresh_svc)
    app.state.refresh_service = refresh_svc

    is_leader = _try_acquire_scheduler_lock()
    scheduler = get_account_refresh_scheduler(refresh_svc)
    set_refresh_scheduler(scheduler)
    set_refresh_scheduler_leader(is_leader)
    app.state.account_refresh_scheduler = scheduler
    app.state.account_refresh_is_leader = is_leader

    strategy_name = reconcile_refresh_runtime(refresh_enabled)
    if is_leader and strategy_name == "quota":
        logger.info(
            "scheduler leader: pid={} strategy=quota active_sync_s={} idle_sync_s={}",
            os.getpid(),
            _SYNC_ACTIVE_INTERVAL,
            _SYNC_IDLE_INTERVAL,
        )
    elif is_leader:
        logger.info(
            "scheduler leader: pid={} strategy=random (scheduler idle) "
            "active_sync_s={} idle_sync_s={}",
            os.getpid(),
            _SYNC_ACTIVE_INTERVAL,
            _SYNC_IDLE_INTERVAL,
        )
    else:
        logger.info(
            "scheduler follower: pid={} strategy={} active_sync_s={} idle_sync_s={}",
            os.getpid(),
            strategy_name,
            _SYNC_ACTIVE_INTERVAL,
            _SYNC_IDLE_INTERVAL,
        )

    # 5. Initialise proxy directory and start clearance refresh scheduler.
    from app.control.proxy import get_proxy_directory
    from app.control.proxy.scheduler import ProxyClearanceScheduler

    proxy_dir = await get_proxy_directory()
    proxy_scheduler = ProxyClearanceScheduler(proxy_dir)
    if is_leader:
        proxy_scheduler.start()

    # 6. Asset cleanup scheduler — leader only, deletes stale Grok cloud assets.
    from app.control.asset_cleanup import AssetCleanupService

    _repo_getter = lambda: app.state.repository  # noqa: E731
    asset_cleanup = AssetCleanupService(_repo_getter)
    if is_leader:
        asset_cleanup.start()

    logger.info("application startup completed")
    yield

    # -----------
    # Shutdown
    # -----------
    logger.info("application shutdown started")
    sync_task.cancel()
    try:
        await sync_task
    except asyncio.CancelledError:
        pass

    if is_leader:
        scheduler.stop()
        proxy_scheduler.stop()
        asset_cleanup.stop()
        _release_scheduler_lock()

    set_refresh_scheduler(None)
    set_refresh_scheduler_leader(False)
    set_refresh_service(None)
    await repo.close()
    logger.info("application shutdown completed")


# ---------------------------------------------------------------------------
# Application factory
# ---------------------------------------------------------------------------


def create_app() -> FastAPI:
    openapi_tags = [
        {"name": "OpenAI - Models", "description": "Model discovery endpoints."},
        {
            "name": "OpenAI - Chat",
            "description": "Chat Completions compatible endpoints.",
        },
        {
            "name": "OpenAI - Responses",
            "description": "Responses API compatible endpoints.",
        },
        {
            "name": "OpenAI - Images",
            "description": "Image generation and image edit endpoints.",
        },
        {
            "name": "OpenAI - Videos",
            "description": "Video job creation and retrieval endpoints.",
        },
        {
            "name": "OpenAI - Files",
            "description": "Locally cached media file endpoints.",
        },
        {
            "name": "Anthropic - Messages",
            "description": "Anthropic Messages API compatible endpoints.",
        },
        {
            "name": "Admin - System",
            "description": "Admin system status and runtime configuration endpoints.",
        },
        {
            "name": "Admin - Tokens",
            "description": "Admin account token management endpoints.",
        },
        {"name": "Admin - Batch", "description": "Admin batch operation endpoints."},
        {
            "name": "Admin - Assets",
            "description": "Admin online asset management endpoints.",
        },
        {
            "name": "Admin - Cache",
            "description": "Admin local cache management endpoints.",
        },
        {
            "name": "WebUI - System",
            "description": "WebUI authentication and system endpoints.",
        },
        {"name": "WebUI - Chat", "description": "WebUI chat API endpoints."},
        {"name": "WebUI - Voice", "description": "WebUI voice API endpoints."},
    ]
    app = FastAPI(
        title="Grok2API",
        version=get_project_version(),
        description="OpenAI-compatible API gateway for Grok",
        openapi_tags=openapi_tags,
        lifespan=lifespan,
    )

    app.add_middleware(
        CORSMiddleware,
        allow_origins=["*"],
        allow_credentials=True,
        allow_methods=["*"],
        allow_headers=["*"],
    )

    # Ensure config is loaded on every request.
    @app.middleware("http")
    async def _ensure_config(request: Request, call_next):
        from app.control.account.runtime import reconcile_refresh_runtime

        await _config.load()
        reconcile_refresh_runtime()
        return await call_next(request)

    # Global exception handler — converts AppError to JSON.
    @app.exception_handler(AppError)
    async def _app_error_handler(request: Request, exc: AppError):
        return JSONResponse(exc.to_dict(), status_code=exc.status)

    @app.exception_handler(RequestValidationError)
    async def _request_validation_handler(
        request: Request,
        exc: RequestValidationError,
    ):
        errors = exc.errors()
        first = errors[0] if errors else {}
        loc = tuple(first.get("loc") or ())
        param_parts = [
            str(part)
            for part in loc
            if str(part) not in {"body", "query", "path", "header", "cookie"}
        ]
        param = ".".join(param_parts)
        message = first.get("msg") or "Request validation failed"
        logger.warning(
            "request validation failed: method={} path={} param={} errors={}",
            request.method,
            request.url.path,
            param or "-",
            errors,
        )
        payload = {
            "error": {
                "message": message,
                "type": "invalid_request_error",
                "code": "invalid_value",
            }
        }
        if param:
            payload["error"]["param"] = param
        return JSONResponse(payload, status_code=400)

    @app.exception_handler(Exception)
    async def _generic_error_handler(request: Request, exc: Exception):
        logger.exception("unhandled application exception: error={}", exc)
        return JSONResponse(
            {"error": {"message": "Internal server error", "type": "server_error"}},
            status_code=500,
        )

    # Routers.
    from app.products.web import router as web_router
    from app.products.openai.router import router as openai_router
    from app.products.anthropic.router import router as anthropic_router

    app.include_router(web_router)
    app.include_router(openai_router)
    app.include_router(anthropic_router)

    # Static assets — new statics only.
    _statics_dir = Path(__file__).resolve().parent / "statics"
    if _statics_dir.is_dir():
        app.mount("/static", StaticFiles(directory=_statics_dir), name="static")

    @app.get("/favicon.ico", include_in_schema=False)
    def favicon():
        from fastapi.responses import FileResponse as _FR

        _ico = _statics_dir / "favicon.ico"
        if _ico.exists():
            return _FR(_ico)
        return JSONResponse({"error": "not found"}, status_code=404)

    @app.get("/health", include_in_schema=False)
    def health():
        return {"status": "ok"}

    return app


app = create_app()


if __name__ == "__main__":
    logger.error(
        "direct startup is disabled: use "
        "uv run granian --interface asgi --host 0.0.0.0 --port 8000 app.main:app"
    )
    raise SystemExit(1)
