import unittest

from app.platform.errors import ValidationError
from app.products.openai import video


class VideoReferenceHelperTests(unittest.TestCase):
    def test_replace_reference_placeholders_supports_cn_and_en_aliases(self) -> None:
        prompt = "先展示@图1里的角色，再切到@image2的场景，最后回到@img1"
        replaced = video._replace_reference_placeholders(
            prompt,
            ["asset_one", "asset_two"],
        )
        self.assertEqual(
            replaced,
            "先展示@asset_one里的角色，再切到@asset_two的场景，最后回到@asset_one",
        )

    def test_replace_reference_placeholders_rejects_missing_index(self) -> None:
        with self.assertRaises(ValidationError):
            video._replace_reference_placeholders("参考@图2", ["asset_one"])

    def test_extract_video_prompt_and_references_collects_all_images(self) -> None:
        prompt, refs = video._extract_video_prompt_and_references(
            [
                {
                    "role": "user",
                    "content": [
                        {"type": "text", "text": "第一段提示"},
                        {"type": "image_url", "image_url": {"url": "https://example.com/ref-1.png"}},
                    ],
                },
                {
                    "role": "user",
                    "content": [
                        {"type": "text", "text": "最终提示词"},
                        {"type": "image_url", "image_url": {"url": "https://example.com/ref-2.png"}},
                        {"type": "image_url", "image_url": {"url": "https://example.com/ref-3.png"}},
                    ],
                },
            ]
        )

        self.assertEqual(prompt, "最终提示词")
        self.assertEqual(
            refs,
            [
                {"image_url": "https://example.com/ref-1.png"},
                {"image_url": "https://example.com/ref-2.png"},
                {"image_url": "https://example.com/ref-3.png"},
            ],
        )

    def test_video_create_payload_includes_multi_reference_fields(self) -> None:
        payload = video._video_create_payload(
            prompt="test prompt",
            parent_post_id="post_123",
            aspect_ratio="16:9",
            resolution_name="720p",
            video_length=10,
            preset="normal",
            image_references=["https://assets.grok.com/users/u/ref-1/content"],
            file_attachments=["asset_ref_1"],
        )

        config = payload["responseMetadata"]["modelConfigOverride"]["modelMap"]["videoGenModelConfig"]
        self.assertEqual(config["imageReferences"], ["https://assets.grok.com/users/u/ref-1/content"])
        self.assertTrue(config["isReferenceToVideo"])
        self.assertEqual(payload["fileAttachments"], ["asset_ref_1"])

    def test_extract_asset_id_from_content_url(self) -> None:
        asset_id = video._extract_asset_id_from_content_url(
            "https://assets.grok.com/users/user-123/asset-456/content"
        )
        self.assertEqual(asset_id, "asset-456")


if __name__ == "__main__":
    unittest.main()
