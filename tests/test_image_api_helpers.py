import unittest

from app.platform.errors import ValidationError
from app.products.openai.images import (
    _extract_image_file_id,
    _normalize_response_format,
    normalize_image_aspect_ratio,
)
from app.products.openai.responses import _build_image_response_payload


class ImageApiHelperTests(unittest.TestCase):
    def test_size_maps_to_aspect_ratio(self):
        self.assertEqual(normalize_image_aspect_ratio("1792x1024"), "3:2")
        self.assertEqual(normalize_image_aspect_ratio("1024x1024"), "1:1")

    def test_aspect_ratio_overrides_size(self):
        self.assertEqual(normalize_image_aspect_ratio("1024x1024", "16:9"), "16:9")

    def test_invalid_shape_and_response_format_raise_validation_errors(self):
        with self.assertRaises(ValidationError):
            normalize_image_aspect_ratio("2048x2048")
        with self.assertRaises(ValidationError):
            _normalize_response_format("base64")

    def test_file_id_falls_back_to_hex_hash_for_non_routeable_names(self):
        file_id = _extract_image_file_id("https://assets.grok.com/images/generated-image.jpg")
        self.assertRegex(file_id, r"^[0-9a-f]{32}$")

    def test_responses_url_payload_contains_image_generation_item(self):
        payload = _build_image_response_payload(
            response_id="resp_test",
            model="grok-imagine-image-lite",
            prompt="draw a cat",
            image_response={"data": [{"url": "/v1/files/image?id=abcdef1234567890"}]},
            response_format="url",
        )
        self.assertEqual(payload["status"], "completed")
        self.assertEqual(payload["output"][0]["type"], "image_generation_call")
        self.assertEqual(payload["output"][0]["url"], "/v1/files/image?id=abcdef1234567890")


if __name__ == "__main__":
    unittest.main()
