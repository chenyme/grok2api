"""
gRPC-Web 协议工具

提供 framing 编码/解码、trailer 解析等通用功能。
支持 application/grpc-web+proto 和 application/grpc-web-text (base64) 两种格式。
"""

from __future__ import annotations

import base64
import re
import struct
from dataclasses import dataclass
from typing import Dict, List, Mapping, Tuple
from urllib.parse import unquote


_B64_RE = re.compile(rb"^[A-Za-z0-9+/=\r\n]+$")


def encode_grpc_web_payload(data: bytes) -> bytes:
    """
    编码 gRPC-Web data frame

    Frame format:
      1-byte flags + 4-byte big-endian length + message bytes
    """
    return b"\x00" + struct.pack(">I", len(data)) + data


def _maybe_decode_grpc_web_text(body: bytes, content_type: str | None) -> bytes:
    """处理 grpc-web-text 模式的 base64 解码"""
    ct = (content_type or "").lower()
    if "grpc-web-text" in ct:
        compact = b"".join(body.split())
        return base64.b64decode(compact, validate=False)

    # 启发式：body 仅包含 base64 字符才尝试解码
    head = body[: min(len(body), 2048)]
    if head and _B64_RE.fullmatch(head):
        compact = b"".join(body.split())
        try:
            return base64.b64decode(compact, validate=True)
        except Exception:
            return body
    return body


def _parse_trailer_block(payload: bytes) -> Dict[str, str]:
    """解析 trailer frame 内容"""
    text = payload.decode("utf-8", errors="replace")
    lines = [ln for ln in re.split(r"\r\n|\n", text) if ln]

    trailers: Dict[str, str] = {}
    for ln in lines:
        if ":" not in ln:
            continue
        k, v = ln.split(":", 1)
        trailers[k.strip().lower()] = v.strip()

    # grpc-message 可能是 percent-encoding
    if "grpc-message" in trailers:
        trailers["grpc-message"] = unquote(trailers["grpc-message"])

    return trailers


def parse_grpc_web_response(
    body: bytes,
    content_type: str | None = None,
    headers: Mapping[str, str] | None = None,
) -> Tuple[List[bytes], Dict[str, str]]:
    """
    解析 gRPC-Web 响应

    Returns:
        (messages, trailers): data frames 列表和合并后的 trailers
    """
    decoded = _maybe_decode_grpc_web_text(body, content_type)

    messages: List[bytes] = []
    trailers: Dict[str, str] = {}

    i = 0
    n = len(decoded)
    while i < n:
        if n - i < 5:
            break

        flag = decoded[i]
        length = int.from_bytes(decoded[i + 1 : i + 5], "big")
        i += 5

        if n - i < length:
            break

        payload = decoded[i : i + length]
        i += length

        if flag & 0x80:  # trailer frame
            trailers.update(_parse_trailer_block(payload))
        elif flag & 0x01:  # compressed (不支持)
            raise ValueError("grpc-web compressed flag not supported")
        else:
            messages.append(payload)

    # 兼容：grpc-status 可能在 response headers 中
    if headers:
        lower = {k.lower(): v for k, v in headers.items()}
        if "grpc-status" in lower and "grpc-status" not in trailers:
            trailers["grpc-status"] = str(lower["grpc-status"]).strip()
        if "grpc-message" in lower and "grpc-message" not in trailers:
            trailers["grpc-message"] = unquote(str(lower["grpc-message"]).strip())

    return messages, trailers


@dataclass(frozen=True)
class GrpcStatus:
    code: int
    message: str = ""

    @property
    def ok(self) -> bool:
        return self.code == 0

    @property
    def http_equiv(self) -> int:
        """映射到类 HTTP 状态码"""
        mapping = {
            0: 200,  # OK
            16: 401,  # UNAUTHENTICATED
            7: 403,  # PERMISSION_DENIED
            8: 429,  # RESOURCE_EXHAUSTED
            4: 504,  # DEADLINE_EXCEEDED
            14: 503,  # UNAVAILABLE
        }
        return mapping.get(self.code, 502)


def get_grpc_status(trailers: Mapping[str, str]) -> GrpcStatus:
    """从 trailers 提取 gRPC 状态"""
    raw = str(trailers.get("grpc-status", "")).strip()
    msg = str(trailers.get("grpc-message", "")).strip()
    try:
        code = int(raw)
    except Exception:
        code = -1
    return GrpcStatus(code=code, message=msg)


__all__ = [
    "encode_grpc_web_payload",
    "parse_grpc_web_response",
    "get_grpc_status",
    "GrpcStatus",
]
