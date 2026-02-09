from app.core.config import _migrate_deprecated_config


def test_drop_grok_section_without_auto_migration():
    config_data = {
        "grok": {
            "stream": False,
            "thinking": True,
            "base_proxy_url": "http://127.0.0.1:7890",
            "cf_clearance": "cf-token",
            "video_idle_timeout": 88,
        },
        "app": {"app_url": "http://127.0.0.1:8000"},
    }

    valid_sections = {
        "app",
        "network",
        "security",
        "chat",
        "retry",
        "timeout",
        "image",
        "token",
        "cache",
        "performance",
        "proxy",
        "stats",
    }

    migrated, deprecated = _migrate_deprecated_config(config_data, valid_sections)

    assert deprecated == {"grok"}
    assert "grok" not in migrated
    assert migrated == {"app": {"app_url": "http://127.0.0.1:8000"}}


def test_drop_unknown_deprecated_sections_without_merging():
    config_data = {
        "grok": {"stream": True},
        "legacy": {"foo": "bar"},
        "chat": {"thinking": False},
    }

    valid_sections = {"chat", "network", "security", "timeout"}

    migrated, deprecated = _migrate_deprecated_config(config_data, valid_sections)

    assert deprecated == {"grok", "legacy"}
    assert "legacy" not in migrated
    assert "grok" not in migrated
    assert migrated == {"chat": {"thinking": False}}
