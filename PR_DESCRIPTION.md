# 功能实现总结：智能 Token 路由（视频生成）

## 🎯 功能概述

实现了视频生成的智能 Token 路由功能，根据视频需求自动选择最合适的 Token 池（ssoSuper vs ssoBasic）。

## 🔧 实现内容

### 1. Token 管理器增强 (`app/services/token/manager.py`)

新增两个方法：

#### `get_token_info(pool_name)` 
- 返回完整的 TokenInfo 对象（而非仅 token 字符串）
- 便于后续获取 Token 元数据

#### `get_token_for_video(resolution, video_length)` ⭐ 核心功能
智能路由逻辑：
- **720p 或 >6s 视频** → 优先使用 `ssoSuper` Token
- **480p 且 ≤6s 视频** → 使用 `ssoBasic` Token
- **Super Token 不可用** → 自动回退到 Basic Token（带警告日志）

### 2. 视频服务修改 (`app/services/grok/services/media.py`)

- 使用新的 `get_token_for_video()` 方法替代原有的循环查找
- 自动根据视频配置选择最佳 Token
- 保持向后兼容

### 3. 配置选项 (`config.defaults.toml`)

新增 `[video]` 配置段：
```toml
[video]
# 是否启用智能 Token 路由
smart_token_routing = true

# 当需要 Super Token 但不可用时是否允许回退
allow_fallback_to_basic = true

# 回退时的警告日志
log_fallback_warning = true
```

## 📊 修改统计

- **新增代码**: ~90 行
- **修改文件**: 3 个
- **测试文件**: 1 个 (`test_video_token_routing.py`)

## ✅ 解决的问题

1. **非 SSO Super Token 被静默降级**: 现在系统会主动选择正确的 Token 类型
2. **720p 视频生成失败**: 自动使用 Super Token 避免降级
3. **Token 选择不透明**: 详细的日志记录便于调试

## 🧪 测试场景

测试覆盖以下场景：
1. ✅ 480p, 6s → 使用 Basic Token
2. ✅ 720p, 6s → 使用 Super Token
3. ✅ 480p, 10s → 使用 Super Token（>6s）
4. ✅ 720p, Super 池空 → 回退到 Basic Token

## 📝 使用说明

无需修改现有代码，系统会自动根据视频请求参数选择 Token：

```python
# 请求 720p 视频
{
    "model": "grok-video",
    "video_config": {
        "resolution_name": "720p",
        "video_length": 6
    }
}
# → 自动使用 ssoSuper Token

# 请求 480p 视频
{
    "model": "grok-video", 
    "video_config": {
        "resolution_name": "480p",
        "video_length": 6
    }
}
# → 使用 ssoBasic Token
```

## 🔍 日志示例

```
# 正常路由
Video token routing: resolution=720p, length=6s -> pool=ssoSuper (token=eyJ0eXAiOi...)

# 回退情况
Video token routing: ssoSuper pool has no available token for resolution=720p, length=6s. 
Falling back to ssoBasic pool.
Video token routing: fallback from ssoSuper -> ssoBasic (token=eyJ0eXAiOi...)
```

## 🎉 预期效果

- 720p 视频请求将正确使用 SSO Super Token
- 避免因 Token 类型不匹配导致的静默降级
- 当 Super Token 不足时自动回退，保证服务可用性
