"""Tests locking llm_client's contract with the local vLLM server.
No real network call is made, thanks to httpx.MockTransport (same pattern
as test_gemini_client.py).
"""

import json

import httpx
import pytest

from llm_client import generate_json

_SCHEMA = {
    "type": "object",
    "properties": {"quality_score": {"type": "number"}},
    "required": ["quality_score"],
}


def _client(handler):
    return httpx.Client(transport=httpx.MockTransport(handler))


def test_generate_json_sends_correct_request_shape():
    captured = {}

    def handler(request: httpx.Request) -> httpx.Response:
        captured["url"] = str(request.url)
        captured["body"] = json.loads(request.content)
        captured["headers"] = request.headers
        return httpx.Response(
            200,
            json={"choices": [{"message": {"content": '{"quality_score": 0.75}'}}]},
        )

    result = generate_json(
        "sen bir analistsin",
        "gonderi metni",
        _SCHEMA,
        base_url="http://llm.local:8000",
        model="test-model",
        http_client=_client(handler),
    )

    assert result == {"quality_score": 0.75}
    assert captured["url"] == "http://llm.local:8000/v1/chat/completions"
    body = captured["body"]
    assert body["model"] == "test-model"
    assert body["messages"] == [
        {"role": "system", "content": "sen bir analistsin"},
        {"role": "user", "content": "gonderi metni"},
    ]
    # Schema guarantee: vLLM guided decoding enforces it at grammar level.
    assert body["guided_json"] == _SCHEMA
    # Scoring must be deterministic - the same text must give the same score.
    assert body["temperature"] == 0.0
    # If guided_json is ignored (server misconfigured), a real run should hit
    # this upper bound rather than the 30s timeout (kept small on purpose).
    assert body["max_tokens"] == 32
    # api_key not given -> the Authorization header must not be written at all.
    assert "authorization" not in {k.lower() for k in captured["headers"].keys()}


def test_generate_json_trailing_slash_in_base_url_is_normalised():
    captured = {}

    def handler(request: httpx.Request) -> httpx.Response:
        captured["url"] = str(request.url)
        return httpx.Response(
            200, json={"choices": [{"message": {"content": "{}"}}]}
        )

    generate_json(
        "s", "u", _SCHEMA,
        base_url="http://llm.local:8000/",
        model="test-model",
        http_client=_client(handler),
    )

    assert captured["url"] == "http://llm.local:8000/v1/chat/completions"


def test_generate_json_raises_on_non_200():
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(503, text="upstream down")

    with pytest.raises(RuntimeError, match="503"):
        generate_json(
            "s", "u", _SCHEMA,
            base_url="http://llm.local:8000",
            model="test-model",
            http_client=_client(handler),
        )


def test_generate_json_raises_on_empty_choices():
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, json={"choices": []})

    with pytest.raises(RuntimeError, match="empty choices"):
        generate_json(
            "s", "u", _SCHEMA,
            base_url="http://llm.local:8000",
            model="test-model",
            http_client=_client(handler),
        )


def test_generate_json_raises_on_non_json_content():
    """Guided decoding cannot leave the schema; still, if the server is
    misconfigured (guided_json ignored) this must not pass silently."""

    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(
            200, json={"choices": [{"message": {"content": "elbette, iste skor: 0.7"}}]}
        )

    with pytest.raises(json.JSONDecodeError):
        generate_json(
            "s", "u", _SCHEMA,
            base_url="http://llm.local:8000",
            model="test-model",
            http_client=_client(handler),
        )


def test_generate_json_raises_runtime_error_on_malformed_message():
    """Returned 200, choices non-empty but message/content missing -> a familiar
    RuntimeError, not a raw KeyError (completes the G2 log taxonomy)."""

    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, json={"choices": [{"finish_reason": "length"}]})

    with pytest.raises(RuntimeError, match="malformed response"):
        generate_json(
            "s", "u", _SCHEMA,
            base_url="http://llm.local:8000",
            model="test-model",
            http_client=_client(handler),
        )


def test_generate_json_sends_bearer_header_when_api_key_set():
    captured = {}

    def handler(request: httpx.Request) -> httpx.Response:
        captured["headers"] = request.headers
        return httpx.Response(200, json={"choices": [{"message": {"content": "{}"}}]})

    generate_json(
        "s", "u", _SCHEMA,
        base_url="http://llm.local:8000",
        model="test-model",
        api_key="secret-token",
        http_client=_client(handler),
    )

    assert captured["headers"]["authorization"] == "Bearer secret-token"


@pytest.mark.parametrize("api_key", ["", "   "])
def test_generate_json_omits_bearer_header_when_api_key_blank(api_key):
    """Sending an empty Bearer header yields 401 - same treatment as
    LOCAL_LLM_API_KEY unset/empty.."""
    captured = {}

    def handler(request: httpx.Request) -> httpx.Response:
        captured["headers"] = request.headers
        return httpx.Response(200, json={"choices": [{"message": {"content": "{}"}}]})

    generate_json(
        "s", "u", _SCHEMA,
        base_url="http://llm.local:8000",
        model="test-model",
        api_key=api_key,
        http_client=_client(handler),
    )

    assert "authorization" not in {k.lower() for k in captured["headers"].keys()}
