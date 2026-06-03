"""gRPC-Web framing and status helpers.

Encodes data frames for gRPC-Web requests and parses response frames,
including trailer extraction and gRPC status code mapping.
"""

import base64
import re
import struct
from dataclasses import dataclass
from typing import Dict, List, Mapping, Optional, Tuple
from urllib.parse import unquote

from app.platform.logging.logger import logger

_B64_RE = re.compile(rb"^[A-Za-z0-9+/=\r\n]+$")

# gRPC status code → HTTP equivalent (best-effort mapping).
_GRPC_HTTP: Dict[int, int] = {
    0:  200,
    4:  504,
    7:  403,
    8:  429,
    14: 503,
    16: 401,
}


@dataclass(frozen=True)
class GrpcStatus:
    code:    int
    message: str = ""

    @property
    def ok(self) -> bool:
        return self.code == 0

    @property
    def http_equiv(self) -> int:
        return _GRPC_HTTP.get(self.code, 502)


class GrpcClient:
    """gRPC-Web framing helpers.

    All methods are static; instantiate only if you want a namespace handle.
    """

    # ------------------------------------------------------------------
    # Encoding
    # ------------------------------------------------------------------

    @staticmethod
    def encode_payload(data: bytes) -> bytes:
        """Wrap *data* in a gRPC-Web data frame (flag=0x00)."""
        return b"\x00" + struct.pack(">I", len(data)) + data

    # ------------------------------------------------------------------
    # Decoding
    # ------------------------------------------------------------------

    @staticmethod
    def _maybe_decode_base64(body: bytes, content_type: Optional[str]) -> bytes:
        ct = (content_type or "").lower()
        if "grpc-web-text" in ct:
            return base64.b64decode(b"".join(body.split()), validate=False)
        head = body[: min(len(body), 2048)]
        if head and _B64_RE.fullmatch(head):
            compact = b"".join(body.split())
            try:
                return base64.b64decode(compact, validate=True)
            except ValueError:
                pass
        return body

    @staticmethod
    def _parse_trailers(payload: bytes) -> Dict[str, str]:
        text   = payload.decode("utf-8", errors="replace")
        result: Dict[str, str] = {}
        for line in re.split(r"\r\n|\n", text):
            if ":" not in line:
                continue
            k, v = line.split(":", 1)
            result[k.strip().lower()] = v.strip()
        if "grpc-message" in result:
            result["grpc-message"] = unquote(result["grpc-message"])
        return result

    @classmethod
    def parse_response(
        cls,
        body:         bytes,
        content_type: Optional[str]             = None,
        headers:      Optional[Mapping[str, str]] = None,
    ) -> Tuple[List[bytes], Dict[str, str]]:
        """Parse a gRPC-Web response body.

        Returns ``(messages, trailers)`` where *messages* is a list of
        raw protobuf payloads and *trailers* contains gRPC metadata
        (including ``grpc-status`` and ``grpc-message``).
        """
        decoded = cls._maybe_decode_base64(body, content_type)

        messages: List[bytes]    = []
        trailers: Dict[str, str] = {}

        i, n = 0, len(decoded)
        while i < n:
            if n - i < 5:
                break
            flag   = decoded[i]
            length = int.from_bytes(decoded[i + 1: i + 5], "big")
            i += 5
            if n - i < length:
                break
            payload = decoded[i: i + length]
            i += length

            if flag & 0x80:
                trailers.update(cls._parse_trailers(payload))
            elif flag & 0x01:
                raise ValueError("grpc-web compressed frame is not supported")
            else:
                messages.append(payload)

        # Supplement from HTTP headers (some servers send trailers as headers).
        if headers:
            lower = {k.lower(): v for k, v in headers.items()}
            for key in ("grpc-status", "grpc-message"):
                if key in lower and key not in trailers:
                    val = str(lower[key]).strip()
                    trailers[key] = unquote(val) if key == "grpc-message" else val

        raw_code = str(trailers.get("grpc-status", "")).strip()
        try:
            code = int(raw_code)
        except ValueError:
            code = -1

        if code not in (0, -1):
            logger.error(
                "grpc response reported error: grpc_status={} grpc_message={} content_type={}",
                code,
                trailers.get("grpc-message", ""),
                content_type or "",
            )

        return messages, trailers

    @staticmethod
    def get_status(trailers: Mapping[str, str]) -> GrpcStatus:
        """Extract ``GrpcStatus`` from parsed trailers."""
        raw = str(trailers.get("grpc-status", "")).strip()
        msg = str(trailers.get("grpc-message", "")).strip()
        try:
            code = int(raw)
        except ValueError:
            code = -1
        return GrpcStatus(code=code, message=msg)


__all__ = ["GrpcClient", "GrpcStatus"]
