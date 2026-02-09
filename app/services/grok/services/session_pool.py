"""共享 AsyncSession 连接池

按浏览器指纹标识缓存 AsyncSession 实例，避免每次请求创建/销毁连接。
应用关闭时通过 close_all_sessions() 统一释放。
"""

from curl_cffi.requests import AsyncSession
from app.core.config import get_config

_DEFAULT_BROWSER = "chrome136"

# 模块级共享会话缓存: browser_id -> AsyncSession
_sessions: dict[str, AsyncSession] = {}


def get_shared_session(impersonate: str = None) -> AsyncSession:
    """获取共享的 AsyncSession 实例

    Args:
        impersonate: 浏览器指纹标识，None 时使用配置默认值

    Returns:
        共享的 AsyncSession 实例（按 impersonate 分组）
    """
    browser = impersonate or get_config("security.browser", _DEFAULT_BROWSER)
    if browser not in _sessions:
        _sessions[browser] = AsyncSession(impersonate=browser)
    return _sessions[browser]


async def close_all_sessions():
    """关闭所有共享会话（仅在应用关闭时调用）"""
    for session in _sessions.values():
        try:
            await session.close()
        except Exception:
            pass
    _sessions.clear()
