"""Token 服务外观（Facade）"""

from typing import List, Optional, Dict

from app.services.token.manager import get_token_manager
from app.services.token.models import TokenInfo, EffortType


class TokenService:
    """
    Token 服务外观
    
    提供简化的 API，隐藏内部实现细节
    """
    
    @staticmethod
    async def get_token(pool_name: str = "ssoBasic") -> Optional[str]:
        """
        获取可用 Token
        
        Args:
            pool_name: Token 池名称
            
        Returns:
            Token 字符串（不含 sso= 前缀）或 None
        """
        manager = await get_token_manager()
        return manager.get_token(pool_name)
    
    @staticmethod
    async def consume(token: str, effort: EffortType = EffortType.LOW) -> bool:
        """
        消耗 Token 配额（本地预估）
        
        Args:
            token: Token 字符串
            effort: 消耗力度
            
        Returns:
            是否成功
        """
        manager = await get_token_manager()
        return await manager.consume(token, effort)
    
    @staticmethod
    async def sync_usage(
        token: str, 
        model: str, 
        effort: EffortType = EffortType.LOW
    ) -> bool:
        """
        同步 Token 使用量（优先 API，降级本地）
        
        Args:
            token: Token 字符串
            model: 模型名称
            effort: 降级时的消耗力度
            
        Returns:
            是否成功
        """
        manager = await get_token_manager()
        return await manager.sync_usage(token, model, effort)
    
    @staticmethod
    async def record_fail(token: str, status_code: int = 401, reason: str = "") -> bool:
        """
        记录 Token 失败
        
        Args:
            token: Token 字符串
            status_code: HTTP 状态码
            reason: 失败原因
            
        Returns:
            是否成功
        """
        manager = await get_token_manager()
        return await manager.record_fail(token, status_code, reason)
    
    @staticmethod
    async def add_token(token: str, pool_name: str = "ssoBasic") -> bool:
        """
        添加 Token
        
        Args:
            token: Token 字符串
            pool: Token 池名称
            
        Returns:
            是否成功
        """
        manager = await get_token_manager()
        return await manager.add(token, pool_name)
    
    @staticmethod
    async def remove_token(token: str) -> bool:
        """
        删除 Token
        
        Args:
            token: Token 字符串
            
        Returns:
            是否成功
        """
        manager = await get_token_manager()
        return await manager.remove(token)
    
    @staticmethod
    async def reset_token(token: str) -> bool:
        """
        重置单个 Token
        
        Args:
            token: Token 字符串
            
        Returns:
            是否成功
        """
        manager = await get_token_manager()
        return await manager.reset_token(token)
    
    @staticmethod
    async def reset_all():
        """重置所有 Token"""
        manager = await get_token_manager()
        await manager.reset_all()
    
    @staticmethod
    async def get_stats() -> Dict[str, dict]:
        """
        获取统计信息
        
        Returns:
            各池的统计信息
        """
        manager = await get_token_manager()
        return manager.get_stats()
    
    @staticmethod
    async def list_tokens(pool_name: str = "ssoBasic") -> List[TokenInfo]:
        """
        获取指定池的所有 Token
        
        Args:
            pool_name: Token 池名称
            
        Returns:
            Token 列表
        """
        manager = await get_token_manager()
        return manager.get_pool_tokens(pool_name)


__all__ = ["TokenService"]
