"""配置管理核心单元测试 — _deep_merge / Config.get / register_defaults"""

from app.core.config import _deep_merge, Config

# ==================== _deep_merge ====================


def test_deep_merge_flat():
    base = {"a": 1, "b": 2}
    override = {"b": 3, "c": 4}
    result = _deep_merge(base, override)
    assert result == {"a": 1, "b": 3, "c": 4}
    # 不修改原始
    assert base == {"a": 1, "b": 2}


def test_deep_merge_nested():
    base = {"app": {"key": "old", "port": 8000}}
    override = {"app": {"key": "new"}}
    result = _deep_merge(base, override)
    assert result["app"]["key"] == "new"
    assert result["app"]["port"] == 8000


def test_deep_merge_deeply_nested():
    base = {"a": {"b": {"c": 1, "d": 2}}}
    override = {"a": {"b": {"c": 99}}}
    result = _deep_merge(base, override)
    assert result["a"]["b"]["c"] == 99
    assert result["a"]["b"]["d"] == 2


def test_deep_merge_override_replaces_non_dict():
    base = {"app": {"key": "old"}}
    override = {"app": "replaced"}
    result = _deep_merge(base, override)
    assert result["app"] == "replaced"


def test_deep_merge_empty_base():
    result = _deep_merge({}, {"a": 1})
    assert result == {"a": 1}


def test_deep_merge_empty_override():
    result = _deep_merge({"a": 1}, {})
    assert result == {"a": 1}


def test_deep_merge_both_empty():
    result = _deep_merge({}, {})
    assert result == {}


def test_deep_merge_returns_copy():
    """确保返回深拷贝，不共享引用"""
    base = {"x": {"y": [1, 2]}}
    override = {}
    result = _deep_merge(base, override)
    result["x"]["y"].append(3)
    assert base["x"]["y"] == [1, 2]  # 原始未被修改


# ==================== Config.get ====================


def test_config_get_flat_key():
    cfg = Config()
    cfg._config = {"app": {"key": "val"}, "debug": True}
    assert cfg.get("debug") is True


def test_config_get_dotted_key():
    cfg = Config()
    cfg._config = {"app": {"key": "val", "port": 8000}}
    assert cfg.get("app.key") == "val"
    assert cfg.get("app.port") == 8000


def test_config_get_missing_returns_default():
    cfg = Config()
    cfg._config = {}
    assert cfg.get("nonexistent", "fallback") == "fallback"


def test_config_get_missing_dotted_returns_default():
    cfg = Config()
    cfg._config = {"app": {}}
    assert cfg.get("app.missing", 42) == 42


def test_config_get_missing_section_returns_default():
    cfg = Config()
    cfg._config = {}
    assert cfg.get("nosection.key", "default") == "default"


def test_config_get_no_default_returns_none():
    cfg = Config()
    cfg._config = {}
    assert cfg.get("missing") is None


# ==================== register_defaults ====================


def test_register_defaults_merges():
    cfg = Config()
    cfg.register_defaults({"retry": {"max_retry": 3}})
    cfg.register_defaults({"retry": {"backoff": 1.0}, "cache": {"ttl": 60}})

    assert cfg._code_defaults["retry"]["max_retry"] == 3
    assert cfg._code_defaults["retry"]["backoff"] == 1.0
    assert cfg._code_defaults["cache"]["ttl"] == 60


def test_register_defaults_override_wins():
    """后注册的同键覆盖先注册的"""
    cfg = Config()
    cfg.register_defaults({"retry": {"max_retry": 3}})
    cfg.register_defaults({"retry": {"max_retry": 5}})
    assert cfg._code_defaults["retry"]["max_retry"] == 5
