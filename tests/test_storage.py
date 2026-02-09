import asyncio

from app.core import storage as storage_mod


def test_local_storage_save_load(tmp_path):
    old_config = storage_mod.CONFIG_FILE
    old_token = storage_mod.TOKEN_FILE
    old_lock = storage_mod.LOCK_DIR
    storage_mod.CONFIG_FILE = tmp_path / "config.toml"
    storage_mod.TOKEN_FILE = tmp_path / "token.json"
    storage_mod.LOCK_DIR = tmp_path / ".locks"
    try:

        async def _run():
            storage = storage_mod.LocalStorage()
            cfg = {"app": {"app_password": "k1", "api_key": "k2"}, "stats": {"enabled": True}}
            tokens = {"ssoBasic": [{"token": "t1", "quota": 5}]}
            await storage.save_config(cfg)
            await storage.save_tokens(tokens)
            loaded_cfg = await storage.load_config()
            loaded_tokens = await storage.load_tokens()
            assert loaded_cfg["app"]["app_password"] == "k1"
            assert loaded_cfg["stats"]["enabled"] is True
            assert loaded_tokens["ssoBasic"][0]["token"] == "t1"

        asyncio.run(_run())
    finally:
        storage_mod.CONFIG_FILE = old_config
        storage_mod.TOKEN_FILE = old_token
        storage_mod.LOCK_DIR = old_lock
