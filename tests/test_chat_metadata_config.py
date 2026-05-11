import unittest

from app.platform.errors import ValidationError
from app.products.openai import router
from app.products.openai.schemas import ChatCompletionRequest, ImageConfig, VideoConfig


def _request(**kwargs) -> ChatCompletionRequest:
    payload = {
        "model": "grok-imagine-video",
        "messages": [{"role": "user", "content": "hello"}],
    }
    payload.update(kwargs)
    return ChatCompletionRequest.model_validate(payload)


class ChatMetadataConfigTests(unittest.TestCase):
    def test_metadata_video_config_is_used_as_fallback(self) -> None:
        req = _request(
            metadata={
                "video_config": {
                    "seconds": 16,
                    "size": "1792x1024",
                    "resolution_name": "720p",
                    "preset": "normal",
                }
            }
        )

        cfg = router._resolve_video_config(req)

        self.assertEqual(cfg.seconds, 16)
        self.assertEqual(cfg.size, "1792x1024")
        self.assertEqual(cfg.resolution_name, "720p")
        self.assertEqual(cfg.preset, "normal")

    def test_top_level_video_config_wins_over_metadata(self) -> None:
        req = _request(
            video_config=VideoConfig(seconds=6, size="720x1280"),
            metadata={"video_config": {"seconds": 16, "size": "1792x1024"}},
        )

        cfg = router._resolve_video_config(req)

        self.assertEqual(cfg.seconds, 6)
        self.assertEqual(cfg.size, "720x1280")

    def test_metadata_image_config_is_used_as_fallback(self) -> None:
        req = _request(
            metadata={
                "image_config": {
                    "n": 3,
                    "size": "1792x1024",
                    "response_format": "url",
                }
            }
        )

        cfg = router._resolve_image_config(req)

        self.assertEqual(cfg.n, 3)
        self.assertEqual(cfg.size, "1792x1024")
        self.assertEqual(cfg.response_format, "url")

    def test_top_level_image_config_wins_over_metadata(self) -> None:
        req = _request(
            image_config=ImageConfig(n=1, size="1024x1024"),
            metadata={"image_config": {"n": 3, "size": "1792x1024"}},
        )

        cfg = router._resolve_image_config(req)

        self.assertEqual(cfg.n, 1)
        self.assertEqual(cfg.size, "1024x1024")

    def test_metadata_config_must_be_object(self) -> None:
        req = _request(metadata={"video_config": "bad"})

        with self.assertRaises(ValidationError):
            router._resolve_video_config(req)


if __name__ == "__main__":
    unittest.main()
