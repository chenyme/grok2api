"""定时调度：周期性刷新 cf_clearance（集成到 grok2api 进程内）"""

import asyncio

from loguru import logger

from .config import get_refresh_interval, get_flaresolverr_url, is_enabled
from .solver import solve_cf_challenge

_task: asyncio.Task | None = None


async def _update_app_config(
    cf_cookies: str,
    user_agent: str = "",
    browser: str = "",
    cf_clearance: str = "",
) -> bool:
    """直接更新 grok2api 的运行时配置"""
    try:
        from app.core.config import config

        proxy_update = {"cf_cookies": cf_cookies}
        if cf_clearance:
            proxy_update["cf_clearance"] = cf_clearance
        if user_agent:
            proxy_update["user_agent"] = user_agent
        if browser:
            proxy_update["browser"] = browser

        await config.update({"proxy": proxy_update})

        logger.info(f"配置已更新: cf_cookies (长度 {len(cf_cookies)}), 指纹: {browser}")
        if user_agent:
            logger.info(f"配置已更新: user_agent = {user_agent}")
        return True
    except Exception as e:
        logger.error(f"更新配置失败: {e}")
        return False


async def refresh_once() -> bool:
    """执行一次刷新流程"""
    logger.info("=" * 50)
    logger.info("开始刷新 cf_clearance...")

    result = await solve_cf_challenge()
    if not result:
        logger.error("刷新失败：无法获取 cf_clearance")
        return False

    success = await _update_app_config(
        cf_cookies=result["cookies"],
        cf_clearance=result.get("cf_clearance", ""),
        user_agent=result.get("user_agent", ""),
        browser=result.get("browser", ""),
    )

    if success:
        logger.info("刷新完成")
    else:
        logger.error("刷新失败: 更新配置失败")

    return success


async def _scheduler_loop():
    """后台调度循环"""
    logger.info(
        f"cf_refresh scheduler started (FlareSolverr: {get_flaresolverr_url()}, interval: {get_refresh_interval()}s)"
    )

    # 周期性刷新（每次循环重新读取配置，支持面板修改实时生效）
    while True:
        if is_enabled():
            await refresh_once()
        else:
            logger.debug("cf_refresh disabled, skip refresh")
        interval = get_refresh_interval()
        await asyncio.sleep(interval)


def start():
    """启动后台刷新任务"""
    global _task
    if _task is not None:
        return
    _task = asyncio.get_event_loop().create_task(_scheduler_loop())
    logger.info("cf_refresh background task started")


def stop():
    """停止后台刷新任务"""
    global _task
    if _task is not None:
        _task.cancel()
        _task = None
        logger.info("cf_refresh background task stopped")
