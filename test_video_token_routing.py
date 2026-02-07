"""
测试视频 Token 智能路由功能
"""
import sys
sys.path.insert(0, '.')

from app.services.token.models import TokenInfo, TokenStatus, BASIC__DEFAULT_QUOTA, SUPER_DEFAULT_QUOTA
from app.services.token.pool import TokenPool
from app.services.token.manager import TokenManager


def test_token_routing():
    """测试 Token 路由逻辑"""
    print("🧪 测试视频 Token 智能路由功能\n")
    
    # 创建模拟 Token 池
    basic_pool = TokenPool("ssoBasic")
    super_pool = TokenPool("ssoSuper")
    
    # 添加测试 Token
    basic_token = TokenInfo(
        token="basic_test_token_123",
        status=TokenStatus.ACTIVE,
        quota=BASIC__DEFAULT_QUOTA
    )
    super_token = TokenInfo(
        token="super_test_token_456",
        status=TokenStatus.ACTIVE,
        quota=SUPER_DEFAULT_QUOTA
    )
    
    basic_pool.add(basic_token)
    super_pool.add(super_token)
    
    # 创建 TokenManager 并设置池
    manager = TokenManager.__new__(TokenManager)
    manager.pools = {
        "ssoBasic": basic_pool,
        "ssoSuper": super_pool
    }
    
    # 测试场景 1: 480p, 6s - 应该使用 Basic
    print("场景 1: 480p, 6s")
    result = manager.get_token_for_video("480p", 6)
    assert result is not None, "应该获取到 Token"
    assert result.token == "basic_test_token_123", f"应该使用 Basic Token, 但得到: {result.token}"
    print("  ✅ 正确使用 Basic Token\n")
    
    # 测试场景 2: 720p, 6s - 应该使用 Super
    print("场景 2: 720p, 6s")
    result = manager.get_token_for_video("720p", 6)
    assert result is not None, "应该获取到 Token"
    assert result.token == "super_test_token_456", f"应该使用 Super Token, 但得到: {result.token}"
    print("  ✅ 正确使用 Super Token\n")
    
    # 测试场景 3: 480p, 10s (>6s) - 应该使用 Super
    print("场景 3: 480p, 10s (>6s)")
    result = manager.get_token_for_video("480p", 10)
    assert result is not None, "应该获取到 Token"
    assert result.token == "super_test_token_456", f"应该使用 Super Token, 但得到: {result.token}"
    print("  ✅ 正确使用 Super Token\n")
    
    # 测试场景 4: 720p, 但 Super 池为空 - 应该回退到 Basic
    print("场景 4: 720p, 但 Super 池为空（回退测试）")
    super_pool.remove(super_token.token)  # 清空 Super 池
    result = manager.get_token_for_video("720p", 6)
    assert result is not None, "应该获取到 Token（回退）"
    assert result.token == "basic_test_token_123", f"应该回退到 Basic Token, 但得到: {result.token}"
    print("  ✅ 正确回退到 Basic Token\n")
    
    print("🎉 所有测试通过！")


if __name__ == "__main__":
    test_token_routing()
