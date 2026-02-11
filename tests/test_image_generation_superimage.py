from app.core.exceptions import ValidationException
from app.api.v1.image import (
    ImageGenerationRequest,
    should_use_ws_for_generation,
    validate_generation_request,
)
from app.services.grok.models.model import ModelService, Cost


def test_superimage_model_is_registered_as_high_cost_image_model():
    model = ModelService.get("grok-superimage-1.0")
    assert model is not None
    assert model.is_image is True
    assert model.cost == Cost.HIGH


def test_validate_generation_request_accepts_superimage_model():
    req = ImageGenerationRequest(
        model="grok-superimage-1.0",
        prompt="a cat in space",
        n=1,
        response_format="b64_json",
    )
    validate_generation_request(req)


def test_validate_generation_request_stream_with_superimage_allows_b64_json(
):
    response_format = "b64_json"
    req = ImageGenerationRequest(
        model="grok-superimage-1.0",
        prompt="a cat in space",
        n=1,
        stream=True,
        response_format=response_format,
    )
    validate_generation_request(req)


def test_validate_generation_request_stream_with_superimage_rejects_url():
    req = ImageGenerationRequest(
        model="grok-superimage-1.0",
        prompt="a cat in space",
        n=1,
        stream=True,
        response_format="url",
    )
    with pytest.raises(ValidationException):
        validate_generation_request(req)


def test_should_use_ws_for_generation_superimage_always_true():
    assert should_use_ws_for_generation("grok-superimage-1.0") is True
