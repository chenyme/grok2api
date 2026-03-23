import os
import re

from fastapi import APIRouter, Depends, HTTPException

from app.core.auth import verify_app_key
from app.core.config import config
from app.core.storage import (
    LocalStorage,
    RedisStorage,
    SQLStorage,
    get_storage as resolve_storage,
)
from app.core.logger import logger

router = APIRouter()

_CFG_CHAR_REPLACEMENTS = str.maketrans(
    {
        "\u2010": "-",
        "\u2011": "-",
        "\u2012": "-",
        "\u2013": "-",
        "\u2014": "-",
        "\u2212": "-",
        "\u2018": "'",
        "\u2019": "'",
        "\u201c": '"',
        "\u201d": '"',
        "\u00a0": " ",
        "\u2007": " ",
        "\u202f": " ",
        "\u200b": "",
        "\u200c": "",
        "\u200d": "",
        "\ufeff": "",
    }
)


def _sanitize_proxy_text(value, *, remove_all_spaces: bool = False) -> str:
    text = "" if value is None else str(value)
    text = text.translate(_CFG_CHAR_REPLACEMENTS)
    if remove_all_spaces:
        text = re.sub(r"\s+", "", text)
    else:
        text = text.strip()
    return text.encode("latin-1", errors="ignore").decode("latin-1")


def _sanitize_proxy_config_payload(data: dict) -> dict:
    if not isinstance(data, dict):
        return data
    payload = dict(data)
    proxy = payload.get("proxy")
    if not isinstance(proxy, dict):
        return payload

    sanitized_proxy = dict(proxy)
    changed = False

    if "user_agent" in sanitized_proxy:
        raw = sanitized_proxy.get("user_agent")
        val = _sanitize_proxy_text(raw, remove_all_spaces=False)
        if val != raw:
            sanitized_proxy["user_agent"] = val
            changed = True

    if "cf_cookies" in sanitized_proxy:
        raw = sanitized_proxy.get("cf_cookies")
        val = _sanitize_proxy_text(raw, remove_all_spaces=False)
        if val != raw:
            sanitized_proxy["cf_cookies"] = val
            changed = True

    if "cf_clearance" in sanitized_proxy:
        raw = sanitized_proxy.get("cf_clearance")
        val = _sanitize_proxy_text(raw, remove_all_spaces=True)
        if val != raw:
            sanitized_proxy["cf_clearance"] = val
            changed = True

    if changed:
        logger.warning("Sanitized proxy config fields before saving")
        payload["proxy"] = sanitized_proxy
    return payload


def _current_config_payload() -> dict:
    return config.snapshot()


@router.get("/verify", dependencies=[Depends(verify_app_key)])
async def admin_verify():
    """验证后台访问密钥（app_key）"""
    return {"status": "success"}


@router.get("/config", dependencies=[Depends(verify_app_key)])
async def get_config():
    """获取当前配置"""
    return _current_config_payload()


@router.post("/config", dependencies=[Depends(verify_app_key)])
async def update_config(data: dict):
    """更新配置"""
    try:
        await config.update(_sanitize_proxy_config_payload(data))
        return {
            "status": "success",
            "message": "配置已更新",
            "config": _current_config_payload(),
        }
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))


@router.get("/storage", dependencies=[Depends(verify_app_key)])
async def get_storage_mode():
    """获取当前存储模式"""
    storage_type = os.getenv("SERVER_STORAGE_TYPE", "").lower()
    if not storage_type:
        storage = resolve_storage()
        if isinstance(storage, LocalStorage):
            storage_type = "local"
        elif isinstance(storage, RedisStorage):
            storage_type = "redis"
        elif isinstance(storage, SQLStorage):
            storage_type = {
                "mysql": "mysql",
                "mariadb": "mysql",
                "postgres": "pgsql",
                "postgresql": "pgsql",
                "pgsql": "pgsql",
            }.get(storage.dialect, storage.dialect)
    return {"type": storage_type or "local"}
