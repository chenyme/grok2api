"""
Grok 模型管理服务
"""

from enum import Enum
from typing import Optional, Tuple
from pydantic import BaseModel, Field

from app.core.exceptions import ValidationException


class Tier(str, Enum):
    """模型档位"""
    BASIC = "basic"
    SUPER = "super"


class Cost(str, Enum):
    """计费类型"""
    LOW = "low"
    HIGH = "high"


class ModelInfo(BaseModel):
    """模型信息"""
    model_id: str
    grok_model: str
    model_mode: str
    tier: Tier = Field(default=Tier.BASIC)
    cost: Cost = Field(default=Cost.LOW)
    display_name: str
    description: str = ""
    is_video: bool = False
    is_image: bool = False


class ModelService:
    """模型管理服务"""
    
    MODELS = [
        ModelInfo(
            model_id="grok-3",
            grok_model="grok-3",
            model_mode="MODEL_MODE_AUTO",
            cost=Cost.LOW,
            display_name="Grok 3"
        ),
        ModelInfo(
            model_id="grok-3-fast",
            grok_model="grok-3",
            cost=Cost.LOW,
            model_mode="MODEL_MODE_FAST",
            display_name="Grok 3 Fast"
        ),
        ModelInfo(
            model_id="grok-4",
            grok_model="grok-4",
            model_mode="MODEL_MODE_AUTO",
            cost=Cost.LOW,
            display_name="Grok 4"
        ),
        ModelInfo(
            model_id="grok-4-mini",
            grok_model="grok-4-mini-thinking-tahoe",
            model_mode="MODEL_MODE_GROK_4_MINI_THINKING",
            cost=Cost.LOW,
            display_name="Grok 4 Mini"
        ),
        ModelInfo(
            model_id="grok-4-fast",
            grok_model="grok-4",
            model_mode="MODEL_MODE_FAST",
            cost=Cost.LOW,
            display_name="Grok 4 Fast"
        ),
        ModelInfo(
            model_id="grok-4-heavy",
            grok_model="grok-4",
            model_mode="MODEL_MODE_HEAVY",
            cost=Cost.HIGH,
            tier=Tier.SUPER,
            display_name="Grok 4 Heavy"
        ),
        ModelInfo(
            model_id="grok-4.1",
            grok_model="grok-4-1-thinking-1129",
            model_mode="MODEL_MODE_AUTO",
            cost=Cost.LOW,
            display_name="Grok 4.1"
        ),
        ModelInfo(
            model_id="grok-4.1-thinking",
            grok_model="grok-4-1-thinking-1129",
            model_mode="MODEL_MODE_GROK_4_1_THINKING",
            cost=Cost.HIGH, 
            display_name="Grok 4.1 Thinking"
        ),
        ModelInfo(
            model_id="grok-imagine-1.0",
            grok_model="grok-3",
            model_mode="MODEL_MODE_FAST",
            cost=Cost.HIGH,
            display_name="Grok Image",
            description="Image generation model",
            is_image=True
        ),
        ModelInfo(
            model_id="grok-imagine-1.0-video",
            grok_model="grok-3",
            model_mode="MODEL_MODE_FAST",
            cost=Cost.HIGH,
            display_name="Grok Video",
            description="Video generation model",
            is_video=True
        ),
    ]
    
    _map = {m.model_id: m for m in MODELS}
    
    @classmethod
    def get(cls, model_id: str) -> Optional[ModelInfo]:
        """获取模型信息"""
        return cls._map.get(model_id)
    
    @classmethod
    def list(cls) -> list[ModelInfo]:
        """获取所有模型"""
        return list(cls._map.values())
    
    @classmethod
    def valid(cls, model_id: str) -> bool:
        """模型是否有效"""
        return model_id in cls._map

    @classmethod
    def to_grok(cls, model_id: str) -> Tuple[str, str]:
        """转换为 Grok 参数"""
        model = cls.get(model_id)
        if not model:
            raise ValidationException(f"Invalid model ID: {model_id}")
        return model.grok_model, model.model_mode

    @classmethod
    def pool_for_model(cls, model_id: str) -> str:
        """根据模型选择 Token 池"""
        model = cls.get(model_id)
        if model and model.tier == Tier.SUPER:
            return "ssoSuper"
        return "ssoBasic"


__all__ = ["ModelService"]
