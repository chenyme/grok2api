"""
管理接口模块

提供Token管理功能，包括登录验证、Token增删查等操作。
"""

import secrets
from typing import Dict, Any, List, Optional
from datetime import datetime, timedelta
from pathlib import Path
from fastapi import APIRouter, HTTPException, Depends, Header
from fastapi.responses import HTMLResponse
from pydantic import BaseModel

from app.core.config import setting
from app.core.logger import logger
from app.services.grok.token import token_manager
from app.models.grok_models import TokenType


# 创建路由器
router = APIRouter(tags=["管理"])

# 常量定义
STATIC_DIR = Path(__file__).parents[2] / "template"
TEMP_DIR = Path(__file__).parents[3] / "data" / "temp"
IMAGE_CACHE_DIR = TEMP_DIR / "image"
VIDEO_CACHE_DIR = TEMP_DIR / "video"
SESSION_EXPIRE_HOURS = 24
BYTES_PER_KB = 1024
BYTES_PER_MB = 1024 * 1024

# 简单的会话存储
_sessions: Dict[str, datetime] = {}


# === 请求/响应模型 ===

class LoginRequest(BaseModel):
    """登录请求"""
    username: str
    password: str


class LoginResponse(BaseModel):
    """登录响应"""
    success: bool
    token: Optional[str] = None
    message: str


class AddTokensRequest(BaseModel):
    """批量添加Token请求"""
    tokens: List[str]
    token_type: str  # "sso" 或 "ssoSuper"


class DeleteTokensRequest(BaseModel):
    """批量删除Token请求"""
    tokens: List[str]
    token_type: str  # "sso" 或 "ssoSuper"


class TokenInfo(BaseModel):
    """Token信息"""
    token: str
    token_type: str
    created_time: Optional[int] = None
    remaining_queries: int
    heavy_remaining_queries: int
    status: str  # "未使用"、"限流中"、"失效"、"正常"


class TokenListResponse(BaseModel):
    """Token列表响应"""
    success: bool
    data: List[TokenInfo]
    total: int


# === 辅助函数 ===

def validate_token_type(token_type_str: str) -> TokenType:
    """验证并转换Token类型字符串为枚举"""
    if token_type_str not in ["sso", "ssoSuper"]:
        raise HTTPException(
            status_code=400,
            detail={"error": "无效的Token类型，必须是 'sso' 或 'ssoSuper'", "code": "INVALID_TYPE"}
        )
    return TokenType.NORMAL if token_type_str == "sso" else TokenType.SUPER


def parse_created_time(created_time) -> Optional[int]:
    """解析创建时间，统一处理不同格式"""
    if isinstance(created_time, str):
        return int(created_time) if created_time else None
    elif isinstance(created_time, int):
        return created_time
    return None


def calculate_token_stats(tokens: Dict[str, Any], token_type: str) -> Dict[str, int]:
    """计算Token统计信息"""
    total = len(tokens)
    expired = sum(1 for t in tokens.values() if t.get("status") == "expired")

    if token_type == "normal":
        unused = sum(1 for t in tokens.values()
                    if t.get("status") != "expired" and t.get("remainingQueries", -1) == -1)
        limited = sum(1 for t in tokens.values()
                     if t.get("status") != "expired" and t.get("remainingQueries", -1) == 0)
        active = sum(1 for t in tokens.values()
                    if t.get("status") != "expired" and t.get("remainingQueries", -1) > 0)
    else:  # super token
        unused = sum(1 for t in tokens.values()
                    if t.get("status") != "expired" and
                    t.get("remainingQueries", -1) == -1 and t.get("heavyremainingQueries", -1) == -1)
        limited = sum(1 for t in tokens.values()
                     if t.get("status") != "expired" and
                     (t.get("remainingQueries", -1) == 0 or t.get("heavyremainingQueries", -1) == 0))
        active = sum(1 for t in tokens.values()
                    if t.get("status") != "expired" and
                    (t.get("remainingQueries", -1) > 0 or t.get("heavyremainingQueries", -1) > 0))

    return {
        "total": total,
        "unused": unused,
        "limited": limited,
        "expired": expired,
        "active": active
    }


def verify_admin_session(authorization: Optional[str] = Header(None)) -> bool:
    """验证管理员会话"""
    if not authorization or not authorization.startswith("Bearer "):
        raise HTTPException(
            status_code=401,
            detail={"error": "未授权访问", "code": "UNAUTHORIZED"}
        )
    
    token = authorization[7:]  # 移除 "Bearer " 前缀
    
    # 检查token是否存在且未过期
    if token not in _sessions:
        raise HTTPException(
            status_code=401,
            detail={"error": "会话已过期或无效", "code": "SESSION_INVALID"}
        )
    
    # 检查会话是否过期（24小时）
    if datetime.now() > _sessions[token]:
        del _sessions[token]
        raise HTTPException(
            status_code=401,
            detail={"error": "会话已过期", "code": "SESSION_EXPIRED"}
        )
    
    return True


def get_token_status(token_data: Dict[str, Any], token_type: str) -> str:
    """获取Token状态"""
    # 首先检查是否失效（来自 token.json 的 status 字段）
    if token_data.get("status") == "expired":
        return "失效"
    
    # 获取剩余次数
    remaining_queries = token_data.get("remainingQueries", -1)
    heavy_remaining = token_data.get("heavyremainingQueries", -1)
    
    # 根据token类型选择正确的字段
    if token_type == "ssoSuper":
        # Super token 可能使用 heavy 模型
        relevant_remaining = max(remaining_queries, heavy_remaining)
    else:
        # 普通token主要看 remaining_queries
        relevant_remaining = remaining_queries
    
    if relevant_remaining == -1:
        return "未使用"
    elif relevant_remaining == 0:
        return "限流中"
    else:
        return "正常"


# === 页面路由 ===

@router.get("/login", response_class=HTMLResponse)
async def login_page():
    """登录页面"""
    login_html = STATIC_DIR / "login.html"
    if login_html.exists():
        return login_html.read_text(encoding="utf-8")
    raise HTTPException(status_code=404, detail="登录页面不存在")


@router.get("/manage", response_class=HTMLResponse)
async def manage_page():
    """管理页面"""
    admin_html = STATIC_DIR / "admin.html"
    if admin_html.exists():
        return admin_html.read_text(encoding="utf-8")
    raise HTTPException(status_code=404, detail="管理页面不存在")


# === API端点 ===

@router.post("/api/login", response_model=LoginResponse)
async def admin_login(request: LoginRequest) -> LoginResponse:
    """
    管理员登录
    
    验证用户名和密码，成功后返回会话token。
    """
    try:
        logger.debug(f"[Admin] 管理员登录尝试 - 用户名: {request.username}")

        # 验证用户名和密码
        expected_username = setting.global_config.get("admin_username", "")
        expected_password = setting.global_config.get("admin_password", "")

        if request.username != expected_username or request.password != expected_password:
            logger.warning(f"[Admin] 登录失败: 用户名或密码错误 - 用户名: {request.username}")
            return LoginResponse(
                success=False,
                message="用户名或密码错误"
            )

        # 生成会话token
        session_token = secrets.token_urlsafe(32)

        # 设置会话过期时间
        expire_time = datetime.now() + timedelta(hours=SESSION_EXPIRE_HOURS)
        _sessions[session_token] = expire_time

        logger.debug(f"[Admin] 管理员登录成功 - 用户名: {request.username}")

        return LoginResponse(
            success=True,
            token=session_token,
            message="登录成功"
        )

    except Exception as e:
        logger.error(f"[Admin] 登录处理异常 - 用户名: {request.username}, 错误: {str(e)}")
        raise HTTPException(
            status_code=500,
            detail={"error": f"登录失败: {str(e)}", "code": "LOGIN_ERROR"}
        )


@router.post("/api/logout")
async def admin_logout(_: bool = Depends(verify_admin_session),
                       authorization: Optional[str] = Header(None)) -> Dict[str, Any]:
    """
    管理员登出
    
    清除会话token。
    """
    try:
        if authorization and authorization.startswith("Bearer "):
            token = authorization[7:]
            if token in _sessions:
                del _sessions[token]
                logger.debug("[Admin] 管理员登出成功")
                return {"success": True, "message": "登出成功"}

        logger.warning("[Admin] 登出失败: 无效或缺失的会话Token")
        return {"success": False, "message": "无效的会话"}

    except Exception as e:
        logger.error(f"[Admin] 登出处理异常 - 错误: {str(e)}")
        raise HTTPException(
            status_code=500,
            detail={"error": f"登出失败: {str(e)}", "code": "LOGOUT_ERROR"}
        )


@router.get("/api/tokens", response_model=TokenListResponse)
async def list_tokens(_: bool = Depends(verify_admin_session)) -> TokenListResponse:
    """
    获取所有Token列表
    
    返回系统中所有Token及其状态信息。
    """
    try:
        logger.debug("[Admin] 开始获取Token列表")

        all_tokens_data = token_manager.get_tokens()
        token_list: List[TokenInfo] = []

        # 处理普通Token
        normal_tokens = all_tokens_data.get(TokenType.NORMAL.value, {})
        for token, data in normal_tokens.items():
            token_list.append(TokenInfo(
                token=token,
                token_type="sso",
                created_time=parse_created_time(data.get("createdTime")),
                remaining_queries=data.get("remainingQueries", -1),
                heavy_remaining_queries=data.get("heavyremainingQueries", -1),
                status=get_token_status(data, "sso")
            ))

        # 处理Super Token
        super_tokens = all_tokens_data.get(TokenType.SUPER.value, {})
        for token, data in super_tokens.items():
            token_list.append(TokenInfo(
                token=token,
                token_type="ssoSuper",
                created_time=parse_created_time(data.get("createdTime")),
                remaining_queries=data.get("remainingQueries", -1),
                heavy_remaining_queries=data.get("heavyremainingQueries", -1),
                status=get_token_status(data, "ssoSuper")
            ))

        normal_count = len(normal_tokens)
        super_count = len(super_tokens)
        total_count = len(token_list)

        logger.debug(f"[Admin] Token列表获取成功 - 普通Token: {normal_count}, Super Token: {super_count}, 总计: {total_count}")

        return TokenListResponse(
            success=True,
            data=token_list,
            total=total_count
        )

    except Exception as e:
        logger.error(f"[Admin] 获取Token列表异常 - 错误: {str(e)}")
        raise HTTPException(
            status_code=500,
            detail={"error": f"获取Token列表失败: {str(e)}", "code": "LIST_ERROR"}
        )


@router.post("/api/tokens/add")
async def add_tokens(request: AddTokensRequest,
                    _: bool = Depends(verify_admin_session)) -> Dict[str, Any]:
    """
    批量添加Token
    
    支持添加普通Token(sso)和Super Token(ssoSuper)。
    """
    try:
        logger.debug(f"[Admin] 批量添加Token - 类型: {request.token_type}, 数量: {len(request.tokens)}")

        # 验证并转换token类型
        token_type = validate_token_type(request.token_type)

        # 添加Token
        await token_manager.add_token(request.tokens, token_type)

        logger.debug(f"[Admin] Token添加成功 - 类型: {request.token_type}, 数量: {len(request.tokens)}")

        return {
            "success": True,
            "message": f"成功添加 {len(request.tokens)} 个Token",
            "count": len(request.tokens)
        }

    except HTTPException:
        raise
    except Exception as e:
        logger.error(f"[Admin] Token添加异常 - 类型: {request.token_type}, 数量: {len(request.tokens)}, 错误: {str(e)}")
        raise HTTPException(
            status_code=500,
            detail={"error": f"添加Token失败: {str(e)}", "code": "ADD_ERROR"}
        )


@router.post("/api/tokens/delete")
async def delete_tokens(request: DeleteTokensRequest,
                       _: bool = Depends(verify_admin_session)) -> Dict[str, Any]:
    """
    批量删除Token
    
    支持删除普通Token(sso)和Super Token(ssoSuper)。
    """
    try:
        logger.debug(f"[Admin] 批量删除Token - 类型: {request.token_type}, 数量: {len(request.tokens)}")

        # 验证并转换token类型
        token_type = validate_token_type(request.token_type)

        # 删除Token
        await token_manager.delete_token(request.tokens, token_type)

        logger.debug(f"[Admin] Token删除成功 - 类型: {request.token_type}, 数量: {len(request.tokens)}")

        return {
            "success": True,
            "message": f"成功删除 {len(request.tokens)} 个Token",
            "count": len(request.tokens)
        }

    except HTTPException:
        raise
    except Exception as e:
        logger.error(f"[Admin] Token删除异常 - 类型: {request.token_type}, 数量: {len(request.tokens)}, 错误: {str(e)}")
        raise HTTPException(
            status_code=500,
            detail={"error": f"删除Token失败: {str(e)}", "code": "DELETE_ERROR"}
        )


@router.get("/api/settings")
async def get_settings(_: bool = Depends(verify_admin_session)) -> Dict[str, Any]:
    """获取全局配置"""
    try:
        logger.debug("[Admin] 获取全局配置")
        return {
            "success": True,
            "data": {
                "global": setting.global_config,
                "grok": setting.grok_config
            }
        }
    except Exception as e:
        logger.error(f"[Admin] 获取配置失败: {str(e)}")
        raise HTTPException(status_code=500, detail={"error": f"获取配置失败: {str(e)}", "code": "GET_SETTINGS_ERROR"})


class UpdateSettingsRequest(BaseModel):
    """更新配置请求"""
    global_config: Optional[Dict[str, Any]] = None
    grok_config: Optional[Dict[str, Any]] = None


class StreamTimeoutSettings(BaseModel):
    """流式超时配置"""
    stream_chunk_timeout: int = 120
    stream_first_response_timeout: int = 30
    stream_total_timeout: int = 600


@router.post("/api/settings")
async def update_settings(request: UpdateSettingsRequest, _: bool = Depends(verify_admin_session)) -> Dict[str, Any]:
    """更新全局配置"""
    try:
        import toml
        import aiofiles
        logger.debug("[Admin] 更新全局配置")

        # 异步读取现有配置
        async with aiofiles.open(setting.config_path, "r", encoding="utf-8") as f:
            content = await f.read()
            config = toml.loads(content)

        # 更新配置
        if request.global_config:
            config["global"].update(request.global_config)
        if request.grok_config:
            config["grok"].update(request.grok_config)

        # 异步写回配置文件
        async with aiofiles.open(setting.config_path, "w", encoding="utf-8") as f:
            await f.write(toml.dumps(config))

        # 重新加载配置
        setting.global_config = setting.load("global")
        setting.grok_config = setting.load("grok")

        logger.debug("[Admin] 配置更新成功")
        return {"success": True, "message": "配置更新成功"}
    except Exception as e:
        logger.error(f"[Admin] 更新配置失败: {str(e)}")
        raise HTTPException(status_code=500, detail={"error": f"更新配置失败: {str(e)}", "code": "UPDATE_SETTINGS_ERROR"})


def _calculate_dir_size(directory: Path) -> int:
    """计算目录中所有文件的大小（字节）"""
    total_size = 0
    for file_path in directory.iterdir():
        if file_path.is_file():
            try:
                total_size += file_path.stat().st_size
            except Exception as e:
                logger.warning(f"[Admin] 无法获取文件大小: {file_path.name}, 错误: {str(e)}")
    return total_size


def _format_size(size_bytes: int) -> str:
    """格式化字节大小为可读字符串"""
    size_mb = size_bytes / BYTES_PER_MB
    if size_mb < 1:
        size_kb = size_bytes / BYTES_PER_KB
        return f"{size_kb:.1f} KB"
    return f"{size_mb:.1f} MB"


@router.get("/api/cache/size")
async def get_cache_size(_: bool = Depends(verify_admin_session)) -> Dict[str, Any]:
    """获取缓存大小"""
    try:
        logger.debug("[Admin] 开始获取缓存大小")

        # 计算图片缓存大小
        image_size = 0
        if IMAGE_CACHE_DIR.exists():
            image_size = _calculate_dir_size(IMAGE_CACHE_DIR)
        
        # 计算视频缓存大小
        video_size = 0
        if VIDEO_CACHE_DIR.exists():
            video_size = _calculate_dir_size(VIDEO_CACHE_DIR)
        
        # 总大小
        total_size = image_size + video_size

        logger.debug(f"[Admin] 缓存大小获取完成 - 图片: {_format_size(image_size)}, 视频: {_format_size(video_size)}, 总计: {_format_size(total_size)}")
        
        return {
            "success": True,
            "data": {
                "image_size": _format_size(image_size),
                "video_size": _format_size(video_size),
                "total_size": _format_size(total_size),
                "image_size_bytes": image_size,
                "video_size_bytes": video_size,
                "total_size_bytes": total_size
            }
        }

    except Exception as e:
        logger.error(f"[Admin] 获取缓存大小异常 - 错误: {str(e)}")
        raise HTTPException(
            status_code=500,
            detail={"error": f"获取缓存大小失败: {str(e)}", "code": "CACHE_SIZE_ERROR"}
        )


@router.post("/api/cache/clear")
async def clear_cache(_: bool = Depends(verify_admin_session)) -> Dict[str, Any]:
    """清理缓存

    删除所有临时文件"""
    try:
        logger.debug("[Admin] 开始清理缓存")

        deleted_count = 0
        image_count = 0
        video_count = 0

        # 清理图片缓存
        if IMAGE_CACHE_DIR.exists():
            for file_path in IMAGE_CACHE_DIR.iterdir():
                if file_path.is_file():
                    try:
                        file_path.unlink()
                        image_count += 1
                        logger.debug(f"[Admin] 删除图片缓存: {file_path.name}")
                    except Exception as e:
                        logger.error(f"[Admin] 删除图片缓存失败: {file_path.name}, 错误: {str(e)}")

        # 清理视频缓存
        if VIDEO_CACHE_DIR.exists():
            for file_path in VIDEO_CACHE_DIR.iterdir():
                if file_path.is_file():
                    try:
                        file_path.unlink()
                        video_count += 1
                        logger.debug(f"[Admin] 删除视频缓存: {file_path.name}")
                    except Exception as e:
                        logger.error(f"[Admin] 删除视频缓存失败: {file_path.name}, 错误: {str(e)}")

        deleted_count = image_count + video_count
        logger.debug(f"[Admin] 缓存清理完成 - 图片: {image_count}, 视频: {video_count}, 总计: {deleted_count}")

        return {
            "success": True,
            "message": f"成功清理缓存，删除图片 {image_count} 个，视频 {video_count} 个，共 {deleted_count} 个文件",
            "data": {
                "deleted_count": deleted_count,
                "image_count": image_count,
                "video_count": video_count
            }
        }

    except Exception as e:
        logger.error(f"[Admin] 清理缓存异常 - 错误: {str(e)}")
        raise HTTPException(
            status_code=500,
            detail={"error": f"清理缓存失败: {str(e)}", "code": "CACHE_CLEAR_ERROR"}
        )


@router.post("/api/cache/clear/images")
async def clear_image_cache(_: bool = Depends(verify_admin_session)) -> Dict[str, Any]:
    """清理图片缓存

    仅删除图片缓存文件"""
    try:
        logger.debug("[Admin] 开始清理图片缓存")

        deleted_count = 0

        # 清理图片缓存
        if IMAGE_CACHE_DIR.exists():
            for file_path in IMAGE_CACHE_DIR.iterdir():
                if file_path.is_file():
                    try:
                        file_path.unlink()
                        deleted_count += 1
                        logger.debug(f"[Admin] 删除图片缓存: {file_path.name}")
                    except Exception as e:
                        logger.error(f"[Admin] 删除图片缓存失败: {file_path.name}, 错误: {str(e)}")

        logger.debug(f"[Admin] 图片缓存清理完成 - 删除 {deleted_count} 个文件")

        return {
            "success": True,
            "message": f"成功清理图片缓存，删除 {deleted_count} 个文件",
            "data": {
                "deleted_count": deleted_count,
                "type": "images"
            }
        }

    except Exception as e:
        logger.error(f"[Admin] 清理图片缓存异常 - 错误: {str(e)}")
        raise HTTPException(
            status_code=500,
            detail={"error": f"清理图片缓存失败: {str(e)}", "code": "IMAGE_CACHE_CLEAR_ERROR"}
        )


@router.post("/api/cache/clear/videos")
async def clear_video_cache(_: bool = Depends(verify_admin_session)) -> Dict[str, Any]:
    """清理视频缓存

    仅删除视频缓存文件"""
    try:
        logger.debug("[Admin] 开始清理视频缓存")

        deleted_count = 0

        # 清理视频缓存
        if VIDEO_CACHE_DIR.exists():
            for file_path in VIDEO_CACHE_DIR.iterdir():
                if file_path.is_file():
                    try:
                        file_path.unlink()
                        deleted_count += 1
                        logger.debug(f"[Admin] 删除视频缓存: {file_path.name}")
                    except Exception as e:
                        logger.error(f"[Admin] 删除视频缓存失败: {file_path.name}, 错误: {str(e)}")

        logger.debug(f"[Admin] 视频缓存清理完成 - 删除 {deleted_count} 个文件")

        return {
            "success": True,
            "message": f"成功清理视频缓存，删除 {deleted_count} 个文件",
            "data": {
                "deleted_count": deleted_count,
                "type": "videos"
            }
        }

    except Exception as e:
        logger.error(f"[Admin] 清理视频缓存异常 - 错误: {str(e)}")
        raise HTTPException(
            status_code=500,
            detail={"error": f"清理视频缓存失败: {str(e)}", "code": "VIDEO_CACHE_CLEAR_ERROR"}
        )


@router.get("/api/stats")
async def get_stats(_: bool = Depends(verify_admin_session)) -> Dict[str, Any]:
    """
    获取统计信息

    返回Token的统计数据。
    """
    try:
        logger.debug("[Admin] 开始获取统计信息")

        all_tokens_data = token_manager.get_tokens()

        # 统计普通Token
        normal_tokens = all_tokens_data.get(TokenType.NORMAL.value, {})
        normal_stats = calculate_token_stats(normal_tokens, "normal")

        # 统计Super Token
        super_tokens = all_tokens_data.get(TokenType.SUPER.value, {})
        super_stats = calculate_token_stats(super_tokens, "super")

        total_count = normal_stats["total"] + super_stats["total"]

        stats = {
            "success": True,
            "data": {
                "normal": normal_stats,
                "super": super_stats,
                "total": total_count
            }
        }

        logger.debug(f"[Admin] 统计信息获取成功 - 普通Token: {normal_stats['total']}, Super Token: {super_stats['total']}, 总计: {total_count}")
        return stats

    except Exception as e:
        logger.error(f"[Admin] 获取统计信息异常 - 错误: {str(e)}")
        raise HTTPException(
            status_code=500,
            detail={"error": f"获取统计信息失败: {str(e)}", "code": "STATS_ERROR"}
        )
