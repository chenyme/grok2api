import unittest

from app.products.openai.images import _normalize_response_format


class ImageResponseFormatTests(unittest.TestCase):
    def test_local_url_is_accepted_for_webui_proxy_output(self):
        self.assertEqual(_normalize_response_format("local_url"), "local_url")


if __name__ == "__main__":
    unittest.main()
