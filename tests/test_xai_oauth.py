"""Tests for the xAI OAuth provider auth logic.

The project ships no pytest dependency, so this file doubles as a stdlib script:

    uv run python tests/test_xai_oauth.py

Each ``test_*`` function uses plain ``assert`` and can also be collected by
pytest if it is ever added.
"""

import base64
import hashlib
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

import orjson

from app.control.xai import oauth
from app.control.xai.callback_parse import parse_oauth_callback
from app.control.xai.constants import OAUTH_REDIRECT_URI, XAI_OAUTH_MODEL_IDS
from app.control.xai.responses_chat import (
    messages_to_responses_body,
    responses_to_chat_completion,
    uses_responses_api,
)
from app.products.openai import xai_chat
from app.control.xai.pkce import generate_pkce


def test_pkce_challenge_matches_verifier():
    verifier, challenge = generate_pkce()
    expected = (
        base64.urlsafe_b64encode(hashlib.sha256(verifier.encode("ascii")).digest())
        .rstrip(b"=")
        .decode("ascii")
    )
    assert challenge == expected
    # verifier/challenge are URL-safe base64 (no padding)
    assert "=" not in verifier and "=" not in challenge


def test_pkce_codes_are_unique():
    a, _ = generate_pkce()
    b, _ = generate_pkce()
    assert a != b


def test_build_authorize_url_has_required_params():
    url = oauth.build_authorize_url(
        "https://auth.x.ai/oauth/authorize",
        redirect_uri=OAUTH_REDIRECT_URI,
        code_challenge="CHAL",
        state="STATE",
        nonce="NONCE",
    )
    for must in (
        "response_type=code",
        "code_challenge=CHAL",
        "code_challenge_method=S256",
        "state=STATE",
        "nonce=NONCE",
        "plan=generic",
        "referrer=grok2api",
        "scope=openid",
        f"redirect_uri={OAUTH_REDIRECT_URI.replace(':', '%3A').replace('/', '%2F')}",
    ):
        assert must in url, f"missing {must} in {url}"
    assert url.startswith("https://auth.x.ai/oauth/authorize?")


def test_parse_oauth_callback_full_url():
    code, state = parse_oauth_callback(
        "http://127.0.0.1:56121/callback?code=ABC&state=XYZ",
    )
    assert code == "ABC"
    assert state == "XYZ"


def test_parse_oauth_callback_bare_code():
    code, state = parse_oauth_callback("ONLYCODE", expected_state="MYSTATE")
    assert code == "ONLYCODE"
    assert state == "MYSTATE"


def test_xai_oauth_model_allowlist():
    assert "grok-build-0.1" in XAI_OAUTH_MODEL_IDS
    assert "grok-composer-2.5-fast" in XAI_OAUTH_MODEL_IDS
    assert "grok-build" not in XAI_OAUTH_MODEL_IDS
    assert "grok-4" not in XAI_OAUTH_MODEL_IDS


def test_composer_uses_responses_api():
    assert uses_responses_api("grok-composer-2.5-fast")
    assert not uses_responses_api("grok-build-0.1")


def test_messages_to_responses_body_simple_user():
    body = messages_to_responses_body(
        model="grok-composer-2.5-fast",
        messages=[{"role": "user", "content": "hello"}],
        stream=False,
        temperature=0.8,
        top_p=0.95,
    )
    assert body["input"] == "hello"
    assert body["model"] == "grok-composer-2.5-fast"


def test_responses_to_chat_completion_extracts_message_text():
    out = responses_to_chat_completion(
        {
            "id": "r1",
            "created_at": 1,
            "output": [
                {
                    "type": "message",
                    "content": [{"type": "output_text", "text": "Hi"}],
                }
            ],
            "usage": {"input_tokens": 3, "output_tokens": 1, "total_tokens": 4},
        },
        model="grok-composer-2.5-fast",
    )
    assert out["choices"][0]["message"]["content"] == "Hi"


def test_xai_chat_rejects_non_oauth_model():
    try:
        xai_chat._validate_model("grok-4")
    except Exception:
        return
    raise AssertionError("grok-4 should be rejected for OAuth path")


def test_parse_oauth_callback_rejects_empty():
    try:
        parse_oauth_callback("  ")
    except ValueError:
        return
    raise AssertionError("empty callback should be rejected")


def test_url_validation_rejects_http():
    try:
        oauth._validate_xai_url("http://auth.x.ai/x", field="t")
    except Exception:
        return
    raise AssertionError("http:// should be rejected")


def test_url_validation_rejects_non_xai_domain():
    try:
        oauth._validate_xai_url("https://evil.example.com/x", field="t")
    except Exception:
        return
    raise AssertionError("non-x.ai domain should be rejected")


def test_url_validation_accepts_xai_subdomain():
    assert oauth._validate_xai_url("https://auth.x.ai/oauth", field="t")


def test_parse_id_token_extracts_email_and_sub():
    payload = (
        base64.urlsafe_b64encode(orjson.dumps({"email": "a@b.com", "sub": "xyz-1"}))
        .rstrip(b"=")
        .decode("ascii")
    )
    email, sub = oauth.parse_id_token(f"header.{payload}.sig")
    assert email == "a@b.com"
    assert sub == "xyz-1"


def test_parse_id_token_handles_garbage():
    assert oauth.parse_id_token("") == (None, None)
    assert oauth.parse_id_token("not-a-jwt") == (None, None)


def _main() -> int:
    failures = 0
    for name, fn in sorted(globals().items()):
        if name.startswith("test_") and callable(fn):
            try:
                fn()
                print(f"PASS {name}")
            except Exception as exc:  # noqa: BLE001
                failures += 1
                print(f"FAIL {name}: {exc}")
    print(f"\n{'OK' if not failures else f'{failures} FAILED'}")
    return 1 if failures else 0


if __name__ == "__main__":
    import sys

    sys.exit(_main())
