import unittest

from app.services.grok.utils.process import _collect_images


class ProcessImageTests(unittest.TestCase):
    def test_collect_images_reads_card_attachments_json(self):
        payload = {
            "cardAttachmentsJson": [
                '{"id":"abc","image_chunk":{"imageUrl":"users/demo/generated/test/image.jpg"}}'
            ]
        }

        images = _collect_images(payload)

        self.assertEqual(images, ["users/demo/generated/test/image.jpg"])


if __name__ == "__main__":
    unittest.main()
