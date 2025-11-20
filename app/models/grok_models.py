"""Grok 模型配置和枚举定义"""

from enum import Enum
from typing import Dict, Any, Tuple


# 模型配置
_MODEL_CONFIG: Dict[str, Dict[str, Any]] = {
    "grok-3-fast": {
        "grok_model": ("grok-3", "MODEL_MODE_FAST"),
        "rate_limit_model": "grok-3",
        "cost": {"type": "low_cost", "multiplier": 1, "description": "计1次调用"},
        "requires_super": False,
        "display_name": "Grok 3 Fast",
        "description": "Fast and efficient Grok 3 model",
        "raw_model_path": "xai/grok-3",
        "default_temperature": 1.0,
        "default_max_output_tokens": 8192,
        "supported_max_output_tokens": 131072,
        "default_top_p": 0.95
    },
    "grok-4-fast": {
        "grok_model": ("grok-4-mini-thinking-tahoe", "MODEL_MODE_GROK_4_MINI_THINKING"),
        "rate_limit_model": "grok-4-mini-thinking-tahoe",
        "cost": {"type": "low_cost", "multiplier": 1, "description": "计1次调用"},
        "requires_super": False,
        "display_name": "Grok 4 Fast",
        "description": "Fast version of Grok 4 with mini thinking capabilities",
        "raw_model_path": "xai/grok-4-mini-thinking-tahoe",
        "default_temperature": 1.0,
        "default_max_output_tokens": 8192,
        "supported_max_output_tokens": 131072,
        "default_top_p": 0.95
    },
    "grok-4-fast-expert": {
        "grok_model": ("grok-4-mini-thinking-tahoe", "MODEL_MODE_EXPERT"),
        "rate_limit_model": "grok-4-mini-thinking-tahoe",
        "cost": {"type": "high_cost", "multiplier": 4, "description": "计4次调用"},
        "requires_super": False,
        "display_name": "Grok 4 Fast Expert",
        "description": "Expert mode of Grok 4 Fast with enhanced reasoning",
        "raw_model_path": "xai/grok-4-mini-thinking-tahoe",
        "default_temperature": 1.0,
        "default_max_output_tokens": 32768,
        "supported_max_output_tokens": 131072,
        "default_top_p": 0.95
    },
    "grok-4-expert": {
        "grok_model": ("grok-4", "MODEL_MODE_EXPERT"),
        "rate_limit_model": "grok-4",
        "cost": {"type": "high_cost", "multiplier": 4, "description": "计4次调用"},
        "requires_super": False,
        "display_name": "Grok 4 Expert",
        "description": "Full Grok 4 model with expert mode capabilities",
        "raw_model_path": "xai/grok-4",
        "default_temperature": 1.0,
        "default_max_output_tokens": 32768,
        "supported_max_output_tokens": 131072,
        "default_top_p": 0.95
    },
    "grok-4-heavy": {
        "grok_model": ("grok-4-heavy", "MODEL_MODE_HEAVY"),
        "rate_limit_model": "grok-4-heavy",
        "cost": {"type": "independent", "multiplier": 1, "description": "独立计费，只有Super用户可用"},
        "requires_super": True,
        "display_name": "Grok 4 Heavy",
        "description": "Most powerful Grok 4 model with heavy computational capabilities. Requires Super Token for access.",
        "raw_model_path": "xai/grok-4-heavy",
        "default_temperature": 1.0,
        "default_max_output_tokens": 65536,
        "supported_max_output_tokens": 131072,
        "default_top_p": 0.95
    },
    "grok-4.1": {
        "grok_model": ("grok-4-1-non-thinking-w-tool", "MODEL_MODE_GROK_4_1"),
        "rate_limit_model": "grok-4-1-non-thinking-w-tool",
        "cost": {"type": "low_cost", "multiplier": 1, "description": "计1次调用"},
        "requires_super": False,
        "display_name": "Grok 4.1",
        "description": "Latest Grok 4.1 model with tool capabilities",
        "raw_model_path": "xai/grok-4-1-non-thinking-w-tool",
        "default_temperature": 1.0,
        "default_max_output_tokens": 8192,
        "supported_max_output_tokens": 131072,
        "default_top_p": 0.95
    },
    "grok-4.1-thinking": {
        "grok_model": ("grok-4-1-thinking-1108b", "MODEL_MODE_AUTO"),
        "rate_limit_model": "grok-4-1-thinking-1108b",
        "cost": {"type": "high_cost", "multiplier": 1, "description": "计1次调用"},
        "requires_super": False,
        "display_name": "Grok 4.1 Thinking",
        "description": "Grok 4.1 model with advanced thinking and tool capabilities",
        "raw_model_path": "xai/grok-4-1-thinking-1108b",
        "default_temperature": 1.0,
        "default_max_output_tokens": 32768,
        "supported_max_output_tokens": 131072,
        "default_top_p": 0.95
    },
    "grok-imagine-0.9": {
        "grok_model": ("grok-3", "MODEL_MODE_FAST"),
        "rate_limit_model": "grok-3",
        "cost": {"type": "low_cost", "multiplier": 1, "description": "计1次调用"},
        "requires_super": False,
        "display_name": "Grok Imagine 0.9",
        "description": "Video generation model powered by Grok",
        "raw_model_path": "xai/grok-imagine-0.9",
        "default_temperature": 1.0,
        "default_max_output_tokens": 8192,
        "supported_max_output_tokens": 131072,
        "default_top_p": 0.95,
        "is_video_model": True
    }
}


class TokenType(Enum):
    """Token类型"""
    NORMAL = "ssoNormal"
    SUPER = "ssoSuper"


class Models(Enum):
    """支持的模型"""
    GROK_3_FAST = "grok-3-fast"
    GROK_4_1 = "grok-4.1"
    GROK_4_1_THINKING = "grok-4.1-thinking"
    GROK_4_FAST = "grok-4-fast"
    GROK_4_FAST_EXPERT = "grok-4-fast-expert"
    GROK_4_EXPERT = "grok-4-expert"
    GROK_4_HEAVY = "grok-4-heavy"
    GROK_IMAGINE_0_9 = "grok-imagine-0.9"

    @classmethod
    def get_model_info(cls, model: str) -> Dict[str, Any]:
        """获取模型配置"""
        return _MODEL_CONFIG.get(model, {})

    @classmethod
    def is_valid_model(cls, model: str) -> bool:
        """检查模型是否有效"""
        return model in _MODEL_CONFIG
    
    @classmethod
    def to_grok(cls, model: str) -> Tuple[str, str]:
        """转换为Grok内部模型名和模式
        
        Returns:
            (模型名, 模式类型) 元组
        """
        config = _MODEL_CONFIG.get(model)
        return config["grok_model"] if config else (model, "MODEL_MODE_FAST")
    
    @classmethod
    def to_rate_limit(cls, model: str) -> str:
        """转换为速率限制模型名"""
        config = _MODEL_CONFIG.get(model)
        return config["rate_limit_model"] if config else model
    
    @classmethod
    def get_all_model_names(cls) -> list[str]:
        """获取所有模型名称"""
        return list(_MODEL_CONFIG.keys())