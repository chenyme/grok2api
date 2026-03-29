import asyncio

import orjson

from app.services.grok.services.image import ImageAppChatCollectProcessor
from app.services.grok.utils.process import _collect_images


def _json_line(payload: dict) -> bytes:
    return orjson.dumps(payload)


async def _iter_lines(lines):
    for line in lines:
        yield line


def test_collect_images_reads_card_attachments_json():
    payload = {
        "cardAttachmentsJson": [
            '{"id":"abc","image_chunk":{"imageUrl":"users/demo/generated/test/image.jpg"}}'
        ]
    }

    images = _collect_images(payload)

    assert images == ["users/demo/generated/test/image.jpg"]


def test_collect_images_reads_card_attachment_json_data():
    payload = {
        "cardAttachment": {
            "jsonData": '{"image":{"original":"https://assets.grok.com/users/demo/generated/test/image.jpg"}}'
        }
    }

    images = _collect_images(payload)

    assert images == ["https://assets.grok.com/users/demo/generated/test/image.jpg"]


def test_collect_images_prefers_original_over_preview_for_same_image():
    image_id = "11111111-2222-3333-4444-555555555555"
    payload = {
        "cardAttachmentsJson": [
            orjson.dumps(
                {
                    "id": "abc",
                    "image_chunk": {
                        "imageUrl": f"users/demo/generated/{image_id}-part-0/image.jpg"
                    },
                    "image": {
                        "original": f"https://assets.grok.com/users/demo/generated/{image_id}/image.jpg"
                    },
                }
            ).decode()
        ]
    }

    images = _collect_images(payload)

    assert images == [
        f"https://assets.grok.com/users/demo/generated/{image_id}/image.jpg"
    ]


def test_image_app_chat_collect_processor_prefers_final_across_events(monkeypatch):
    image_id = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
    preview = f"users/demo/generated/{image_id}-part-0/image.jpg"
    original = f"https://assets.grok.com/users/demo/generated/{image_id}/image.jpg"

    monkeypatch.setattr(
        "app.services.grok.services.image.get_config",
        lambda key, default=None: 0 if key == "image.stream_timeout" else default,
    )

    async def fake_process_image_url(self, url):
        return f"rendered:{url}"

    monkeypatch.setattr(
        ImageAppChatCollectProcessor,
        "_process_image_url",
        fake_process_image_url,
    )

    async def _run():
        processor = ImageAppChatCollectProcessor(
            "grok-imagine-1.0-fast",
            token="token-demo",
            response_format="url",
        )
        payload = await processor.process(
            _iter_lines(
                [
                    _json_line(
                        {
                            "result": {
                                "response": {
                                    "modelResponse": {
                                        "cardAttachmentsJson": [
                                            orjson.dumps(
                                                {
                                                    "id": "card-demo",
                                                    "image_chunk": {
                                                        "imageUrl": preview
                                                    },
                                                }
                                            ).decode()
                                        ]
                                    }
                                }
                            }
                        }
                    ),
                    _json_line(
                        {
                            "result": {
                                "response": {
                                    "modelResponse": {
                                        "cardAttachmentsJson": [
                                            orjson.dumps(
                                                {
                                                    "id": "card-demo",
                                                    "image": {
                                                        "original": original
                                                    },
                                                }
                                            ).decode()
                                        ]
                                    }
                                }
                            }
                        }
                    ),
                ]
            )
        )

        assert payload.images == [f"rendered:{original}"]

    asyncio.run(_run())


def test_image_app_chat_collect_processor_falls_back_to_preview(monkeypatch):
    preview = (
        "users/demo/generated/ffffffff-1111-2222-3333-444444444444-part-0/image.jpg"
    )

    monkeypatch.setattr(
        "app.services.grok.services.image.get_config",
        lambda key, default=None: 0 if key == "image.stream_timeout" else default,
    )

    async def fake_process_image_url(self, url):
        return f"rendered:{url}"

    monkeypatch.setattr(
        ImageAppChatCollectProcessor,
        "_process_image_url",
        fake_process_image_url,
    )

    async def _run():
        processor = ImageAppChatCollectProcessor(
            "grok-imagine-1.0-fast",
            token="token-demo",
            response_format="url",
        )
        payload = await processor.process(
            _iter_lines(
                [
                    _json_line(
                        {
                            "result": {
                                "response": {
                                    "modelResponse": {
                                        "cardAttachmentsJson": [
                                            orjson.dumps(
                                                {
                                                    "id": "card-demo",
                                                    "image_chunk": {
                                                        "imageUrl": preview
                                                    },
                                                }
                                            ).decode()
                                        ]
                                    }
                                }
                            }
                        }
                    )
                ]
            )
        )

        assert payload.images == [f"rendered:{preview}"]

    asyncio.run(_run())
