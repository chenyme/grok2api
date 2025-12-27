#!/usr/bin/env python3
import asyncio
import sys
from pathlib import Path
from typing import Any, Dict

sys.path.insert(0, str(Path(__file__).parents[1]))

from app.core.config import ConfigManager
from app.core.storage import BaseStorage


class FakeStorageOk(BaseStorage):
    def __init__(self) -> None:
        self._config: Dict[str, Any] = {"global": {}, "grok": {}}

    async def init_db(self) -> None:
        return None

    async def load_tokens(self) -> Dict[str, Any]:
        return {"sso": {}, "ssoSuper": {}}

    async def save_tokens(self, data: Dict[str, Any]) -> None:
        return None

    async def load_config(self) -> Dict[str, Any]:
        return self._config

    async def save_config(self, data: Dict[str, Any]) -> None:
        self._config = data


class FakeStorageBroken(FakeStorageOk):
    async def save_config(self, data: Dict[str, Any]) -> None:
        # 模拟“返回成功但没真正保存/丢字段”的存储实现
        self._config = {"global": data.get("global", {})}


async def main() -> None:
    cm = ConfigManager()

    cm.set_storage(FakeStorageOk())
    await cm.save(grok_config={"bypass_server": True, "bypass_baseurl": "http://127.0.0.1:8080"})
    print("OK: FakeStorageOk 持久化校验通过")

    cm.set_storage(FakeStorageBroken())
    try:
        await cm.save(grok_config={"bypass_server": True, "bypass_baseurl": "http://127.0.0.1:8080"})
    except Exception as e:
        print(f"OK: FakeStorageBroken 触发校验失败: {e}")
        return

    raise SystemExit("ERROR: 预期应触发持久化校验失败，但未失败")


if __name__ == "__main__":
    asyncio.run(main())
