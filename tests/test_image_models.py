import unittest

from app.api.v1.image import ImageGenerationRequest, validate_generation_request


class ImageModelTests(unittest.TestCase):
    def test_fast_image_model_is_allowed(self):
        request = ImageGenerationRequest(
            prompt="A red square",
            model="grok-imagine-1.0-fast",
            n=1,
            size="1024x1024",
            response_format="b64_json",
            stream=False,
        )

        validate_generation_request(request)


if __name__ == "__main__":
    unittest.main()
