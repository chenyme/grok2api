import asyncio
import base64
import tempfile
import unittest
from pathlib import Path
from urllib.parse import parse_qs, urlparse
from unittest.mock import patch

from app.products.openai import images


class _StubConfig:
    def __init__(self, *, image_format: str, app_url: str) -> None:
        self.image_format = image_format
        self.app_url = app_url

    def get_str(self, key: str, default: str = "") -> str:
        if key == "features.image_format":
            return self.image_format
        if key == "app.app_url":
            return self.app_url
        return default


class ImageOutputFormatTests(unittest.TestCase):
    def test_resolve_image_output_uses_local_proxy_when_configured(self) -> None:
        blob_b64 = base64.b64encode(b"fake-image-bytes").decode("ascii")
        asset_id = "12345678-1234-1234-1234-123456789abc"
        with tempfile.TemporaryDirectory() as tmpdir:
            config = _StubConfig(
                image_format="local_url",
                app_url="https://app.example.com",
            )
            with patch.object(images, "get_config", return_value=config):
                with patch.object(images, "image_files_dir", return_value=Path(tmpdir)):
                    result = asyncio.run(
                        images._resolve_image_output(
                            token="unused",
                            url=f"https://assets.grok.com/users/user-1/{asset_id}/content.png",
                            response_format="url",
                            blob_b64=blob_b64,
                        )
                    )
            file_id = parse_qs(urlparse(result.api_value).query)["id"][0]
            self.assertTrue((Path(tmpdir) / f"{file_id}.png").exists())

        self.assertEqual(
            result.api_value,
            f"https://app.example.com/v1/files/image?id={asset_id}",
        )
        self.assertEqual(
            result.markdown_value,
            f"![image](https://app.example.com/v1/files/image?id={asset_id})",
        )

    def test_resolve_image_output_keeps_upstream_url_when_configured(self) -> None:
        config = _StubConfig(
            image_format="grok_url",
            app_url="https://app.example.com",
        )
        with patch.object(images, "get_config", return_value=config):
            result = asyncio.run(
                images._resolve_image_output(
                    token="unused",
                    url="https://assets.grok.com/users/user-1/file-abc123/content.png",
                    response_format="url",
                )
            )

        self.assertEqual(
            result.api_value,
            "https://assets.grok.com/users/user-1/file-abc123/content.png",
        )
        self.assertEqual(
            result.markdown_value,
            "![image](https://assets.grok.com/users/user-1/file-abc123/content.png)",
        )


if __name__ == "__main__":
    unittest.main()
