"""
严格 JSON 响应类

确保 API 响应遵循标准 JSON（RFC 8259），避免出现 NaN/Infinity 等非标准值。
"""

import math
from typing import Any

import orjson
from fastapi.encoders import jsonable_encoder
from fastapi.responses import JSONResponse


class StrictJSONResponse(JSONResponse):
    """严格 JSONResponse：统一输出标准 JSON。"""

    @staticmethod
    def _normalize_non_finite(value: Any) -> Any:
        """
        递归规范化非有限浮点数，避免输出 NaN/Infinity。

        - NaN/Infinity/-Infinity -> None（JSON null）
        - list/tuple -> list
        - dict -> dict（递归处理 value）
        """
        if isinstance(value, float) and not math.isfinite(value):
            return None

        if isinstance(value, dict):
            return {
                key: StrictJSONResponse._normalize_non_finite(item)
                for key, item in value.items()
            }

        if isinstance(value, (list, tuple)):
            return [StrictJSONResponse._normalize_non_finite(item) for item in value]

        return value

    def render(self, content: Any) -> bytes:
        """渲染为严格标准 JSON。"""
        # 先执行 FastAPI 的通用编码，处理 datetime/Pydantic 等对象
        encoded = jsonable_encoder(content)
        # 再规范化非标准浮点值，确保 JSON 标准兼容
        normalized = self._normalize_non_finite(encoded)
        # orjson 输出 UTF-8 bytes，默认是 RFC 兼容 JSON
        return orjson.dumps(normalized)
