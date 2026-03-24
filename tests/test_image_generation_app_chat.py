import json

from app.services.grok.services.image import ImageGenerationService
from app.services.grok.utils.process import _collect_images


def test_build_app_chat_message_prefixes_plain_prompt():
    assert (
        ImageGenerationService._build_app_chat_message("a red apple on a white table")
        == "Generate an image: a red apple on a white table"
    )


def test_build_app_chat_message_keeps_existing_generate_prefix():
    assert (
        ImageGenerationService._build_app_chat_message(
            "Generate an image: a red apple on a white table"
        )
        == "Generate an image: a red apple on a white table"
    )


def test_collect_images_reads_final_generated_image_card_path():
    partial = {
        "id": "abc",
        "type": "render_generated_image",
        "cardType": "generated_image_card",
        "image_chunk": {
            "imageUuid": "uuid-1",
            "imageUrl": "users/example/generated/uuid-1-part-0/image.jpg",
            "seq": 0,
            "progress": 50,
        },
    }
    final = {
        "id": "abc",
        "type": "render_generated_image",
        "cardType": "generated_image_card",
        "image_chunk": {
            "imageUuid": "uuid-1",
            "imageUrl": "users/example/generated/uuid-1/image.jpg",
            "seq": 1,
            "progress": 100,
        },
    }

    urls = _collect_images(
        {
            "generatedImageUrls": [],
            "cardAttachmentsJson": [json.dumps(partial), json.dumps(final)],
        }
    )

    assert urls == ["users/example/generated/uuid-1/image.jpg"]


def test_collect_images_ignores_search_result_cards():
    searched = {
        "id": "xyz",
        "type": "render_searched_image",
        "cardType": "image_card",
        "image": {
            "thumbnail": "https://encrypted-tbn0.gstatic.com/images?q=tbn:ANd9GcQ"
        },
    }

    urls = _collect_images({"cardAttachmentsJson": [json.dumps(searched)]})

    assert urls == []
