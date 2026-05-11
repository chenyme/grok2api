import asyncio
import unittest

from app.platform.errors import ValidationError
from app.products.openai import router, video


def _message_with_references(count: int) -> list[dict]:
    return [
        {
            "role": "user",
            "content": [
                {"type": "text", "text": "生成一个参考图视频"},
                *[
                    {
                        "type": "image_url",
                        "image_url": {"url": f"https://example.com/ref-{idx}.png"},
                    }
                    for idx in range(count)
                ],
            ],
        }
    ]


class VideoReferenceHelperTests(unittest.TestCase):
    def test_chat_video_prompt_allows_seven_references(self) -> None:
        prompt, refs = video._extract_video_prompt_and_reference(
            _message_with_references(7)
        )

        self.assertEqual(prompt, "生成一个参考图视频")
        self.assertEqual(
            refs,
            [
                {"image_url": f"https://example.com/ref-{idx}.png"}
                for idx in range(7)
            ],
        )

    def test_chat_video_prompt_rejects_more_than_seven_references(self) -> None:
        with self.assertRaises(ValidationError):
            video._extract_video_prompt_and_reference(_message_with_references(8))

    def test_prepare_video_references_rejects_more_than_seven_references(self) -> None:
        refs = [{"image_url": f"https://example.com/ref-{idx}.png"} for idx in range(8)]

        async def _run() -> None:
            await video._prepare_video_references("token", refs)

        with self.assertRaises(ValidationError):
            asyncio.run(_run())

    def test_videos_create_rejects_more_than_seven_multipart_references(self) -> None:
        async def _run() -> None:
            await router.videos_create(
                model="grok-imagine-video",
                prompt="生成一个参考图视频",
                input_reference=[object() for _ in range(8)],  # type: ignore[list-item]
            )

        with self.assertRaises(ValidationError):
            asyncio.run(_run())

    def test_chat_video_segment_prompts_follow_user_order(self) -> None:
        prompts, refs = video._extract_video_segment_prompts_and_references(
            [
                {"role": "system", "content": "ignore"},
                {"role": "user", "content": "第一段"},
                {"role": "assistant", "content": "ignore"},
                {"role": "user", "content": "第二段"},
            ]
        )

        self.assertEqual(prompts, ["第一段", "第二段"])
        self.assertIsNone(refs)

    def test_segment_prompts_reuse_last_prompt_when_short(self) -> None:
        self.assertEqual(
            video._normalize_segment_prompts("第一段", [10, 6], ["第一段"]),
            ["第一段", "第一段"],
        )

    def test_segment_prompts_strict_rejects_missing_prompts(self) -> None:
        with self.assertRaises(ValidationError):
            video._normalize_segment_prompts(
                "第一段",
                [10, 6],
                ["第一段"],
                strict=True,
            )

    def test_segment_prompts_reject_extra_prompts(self) -> None:
        with self.assertRaises(ValidationError):
            video._normalize_segment_prompts("一", [6], ["一", "二"])

    def test_video_segment_lengths_prefer_ten_second_segments(self) -> None:
        cases = {
            6: [6],
            10: [10],
            12: [6, 6],
            16: [10, 6],
            20: [10, 10],
            22: [10, 6, 6],
            26: [10, 10, 6],
            30: [10, 10, 10],
            32: [10, 10, 6, 6],
            36: [10, 10, 10, 6],
            40: [10, 10, 10, 10],
        }

        for seconds, expected in cases.items():
            with self.subTest(seconds=seconds):
                self.assertEqual(video._build_segment_lengths(seconds), expected)

    def test_async_video_length_rejects_long_videos(self) -> None:
        video.validate_async_video_length(6)
        video.validate_async_video_length(10)
        with self.assertRaises(ValidationError):
            video.validate_async_video_length(12)

    def test_async_video_create_rejects_long_videos(self) -> None:
        async def _run() -> None:
            await video.create_video(
                model="grok-imagine-video",
                prompt="长视频",
                seconds=12,
            )

        with self.assertRaises(ValidationError):
            asyncio.run(_run())

    def test_chat_video_length_rejects_non_exact_duration(self) -> None:
        with self.assertRaises(ValidationError):
            video.validate_video_length(28)

    def test_chat_video_completions_requires_prompt_for_each_segment(self) -> None:
        async def _run() -> None:
            await video.completions(
                model="grok-imagine-video",
                messages=[{"role": "user", "content": "第一段"}],
                stream=False,
                seconds=16,
            )

        with self.assertRaises(ValidationError):
            asyncio.run(_run())


if __name__ == "__main__":
    unittest.main()
