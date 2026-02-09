"""
通用工具函数
"""


def mask_token(token: str, prefix: int = 8, suffix: int = 8) -> str:
    """
    掩码 Token 字符串，用于日志展示

    Args:
        token: 原始 token 字符串
        prefix: 保留前缀长度
        suffix: 保留后缀长度

    Returns:
        掩码后的字符串，如 "abc12345...xyz98765"
    """
    if not token:
        return ""
    raw = token[4:] if token.startswith("sso=") else token
    if len(raw) <= prefix + suffix:
        return raw
    return f"{raw[:prefix]}...{raw[-suffix:]}"


__all__ = ["mask_token"]
