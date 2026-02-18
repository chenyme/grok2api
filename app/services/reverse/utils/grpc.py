"""
gRPC-Web helpers for reverse interfaces.
"""

import base64
import json
import re
import struct
from dataclasses import dataclass
from typing import Dict, List, Mapping, Optional, Tuple
from urllib.parse import unquote

from app.core.logger import logger

# Base64 正则
B64_RE = re.compile(rb"^[A-Za-z0-9+/=\r\n]+$")


@dataclass(frozen=True)
class GrpcStatus:
    code: int
    message: str = ""

    @property
    def ok(self) -> bool:
        return self.code == 0

    @property
    def http_equiv(self) -> int:
        mapping = {
            0: 200,
            16: 401,
            7: 403,
            8: 429,
            4: 504,
            14: 503,
        }
        return mapping.get(self.code, 502)


class GrpcClient:
    """gRPC-Web helpers wrapper."""

    @staticmethod
    def _safe_headers(headers: Optional[Mapping[str, str]]) -> Dict[str, str]:
        if not headers:
            return {}
        safe: Dict[str, str] = {}
        for k, v in headers.items():
            if k.lower() in ("set-cookie", "cookie", "authorization"):
                safe[k] = "<redacted>"
            else:
                safe[k] = str(v)
        return safe

    @staticmethod
    def _b64(data: bytes) -> str:
        return base64.b64encode(data).decode()

    @staticmethod
    def encode_payload(data: bytes) -> bytes:
        """Encode gRPC-Web data frame."""
        return b"\x00" + struct.pack(">I", len(data)) + data

    @staticmethod
    def _maybe_decode_grpc_web_text(body: bytes, content_type: Optional[str]) -> bytes:
        ct = (content_type or "").lower()
        if "grpc-web-text" in ct:
            compact = b"".join(body.split())
            return base64.b64decode(compact, validate=False)

        head = body[: min(len(body), 2048)]
        if head and B64_RE.fullmatch(head):
            compact = b"".join(body.split())
            try:
                return base64.b64decode(compact, validate=True)
            except Exception:
                return body
        return body

    @staticmethod
    def _parse_trailer_block(payload: bytes) -> Dict[str, str]:
        text = payload.decode("utf-8", errors="replace")
        lines = [ln for ln in re.split(r"\r\n|\n", text) if ln]

        trailers: Dict[str, str] = {}
        for ln in lines:
            if ":" not in ln:
                continue
            k, v = ln.split(":", 1)
            trailers[k.strip().lower()] = v.strip()

        if "grpc-message" in trailers:
            trailers["grpc-message"] = unquote(trailers["grpc-message"])

        return trailers

    @classmethod
    def parse_response(
        cls,
        body: bytes,
        content_type: Optional[str] = None,
        headers: Optional[Mapping[str, str]] = None,
    ) -> Tuple[List[bytes], Dict[str, str]]:
        decoded = cls._maybe_decode_grpc_web_text(body, content_type)

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

            if flag & 0x80:
                trailers.update(cls._parse_trailer_block(payload))
            elif flag & 0x01:
                raise ValueError("grpc-web compressed flag not supported")
            else:
                messages.append(payload)

        if headers:
            lower = {k.lower(): v for k, v in headers.items()}
            if "grpc-status" in lower and "grpc-status" not in trailers:
                trailers["grpc-status"] = str(lower["grpc-status"]).strip()
            if "grpc-message" in lower and "grpc-message" not in trailers:
                trailers["grpc-message"] = unquote(str(lower["grpc-message"]).strip())

        # Log full response details on gRPC error
        raw_status = str(trailers.get("grpc-status", "")).strip()
        try:
            status_code = int(raw_status)
        except Exception:
            status_code = -1

        if status_code not in (0, -1):
            try:
                payload = {
                    "grpc_status": status_code,
                    "grpc_message": trailers.get("grpc-message", ""),
                    "content_type": content_type or "",
                    "headers": cls._safe_headers(headers),
                    "trailers": trailers,
                    "messages_b64": [cls._b64(m) for m in messages],
                    "body_b64": cls._b64(body),
                }
                logger.error(
                    "gRPC response error: {}",
                    json.dumps(payload, ensure_ascii=False),
                    extra={"error_type": "GrpcError"},
                )
            except Exception as e:
                logger.error(
                    f"gRPC response error: failed to log payload ({e})",
                    extra={"error_type": "GrpcError"},
                )

        return messages, trailers

    @staticmethod
    def get_status(trailers: Mapping[str, str]) -> GrpcStatus:
        raw = str(trailers.get("grpc-status", "")).strip()
        msg = str(trailers.get("grpc-message", "")).strip()
        try:
            code = int(raw)
        except Exception:
            code = -1
        return GrpcStatus(code=code, message=msg)


__all__ = [
    "GrpcStatus",
    "GrpcClient",
]
