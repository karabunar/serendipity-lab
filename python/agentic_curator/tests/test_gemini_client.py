"""Tests verifying that gemini_client matches the Gemini REST contract
(endpoints, request body shape and response guards).
No real network call is made, thanks to httpx.MockTransport.
"""

import json

import httpx
import pytest

from gemini_client import embed_text, generate_json


def test_embed_text_sends_correct_request_shape():
    captured = {}

    def handler(request: httpx.Request) -> httpx.Response:
        captured["url"] = str(request.url)
        captured["body"] = json.loads(request.content)
        return httpx.Response(
            200,
            json={"embeddings": [{"values": [0.1, 0.2, 0.3]}]},
        )

    transport = httpx.MockTransport(handler)
    client = httpx.Client(transport=transport)

    result = embed_text(
        "Fenerbahçe transfer haberleri",
        api_key="test-key",
        model="gemini-embedding-001",
        output_dim=3072,
        task_type="SEMANTIC_SIMILARITY",
        base_url="https://generativelanguage.googleapis.com",
        http_client=client,
    )

    assert result == [0.1, 0.2, 0.3]
    assert captured["url"].endswith("/v1beta/models/gemini-embedding-001:batchEmbedContents")
    req = captured["body"]["requests"][0]
    assert req["model"] == "models/gemini-embedding-001"
    assert req["content"]["parts"][0]["text"] == "Fenerbahçe transfer haberleri"
    assert req["taskType"] == "SEMANTIC_SIMILARITY"
    assert req["outputDimensionality"] == 3072


def test_embed_text_api_error_raises():
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(500, json={"error": {"message": "server error"}})

    client = httpx.Client(transport=httpx.MockTransport(handler))
    with pytest.raises(RuntimeError):
        embed_text("test", api_key="k", http_client=client)


def test_generate_json_sends_correct_request_shape():
    captured = {}
    schema = {"type": "OBJECT", "properties": {"category": {"type": "STRING"}}, "required": ["category"]}

    def handler(request: httpx.Request) -> httpx.Response:
        captured["url"] = str(request.url)
        captured["body"] = json.loads(request.content)
        return httpx.Response(
            200,
            json={
                "candidates": [
                    {"content": {"parts": [{"text": '{"category": "Fenerbahçe"}'}]}}
                ]
            },
        )

    client = httpx.Client(transport=httpx.MockTransport(handler))
    result = generate_json(
        system_instruction="Sen bir kategori uzmanısın.",
        user_text="Bu postu kategorize et: ...",
        response_schema=schema,
        api_key="test-key",
        model="gemini-2.0-flash",
        base_url="https://generativelanguage.googleapis.com",
        http_client=client,
    )

    assert result == {"category": "Fenerbahçe"}
    assert captured["url"].endswith("/v1beta/models/gemini-2.0-flash:generateContent")
    body = captured["body"]
    assert body["systemInstruction"]["parts"][0]["text"] == "Sen bir kategori uzmanısın."
    assert body["contents"][0]["parts"][0]["text"] == "Bu postu kategorize et: ..."
    assert body["generationConfig"]["responseMimeType"] == "application/json"
    assert body["generationConfig"]["responseSchema"] == schema


def test_generate_json_empty_candidates_raises():
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, json={"candidates": []})

    client = httpx.Client(transport=httpx.MockTransport(handler))
    with pytest.raises(RuntimeError):
        generate_json("sys", "user", {}, api_key="k", http_client=client)


def test_generate_json_empty_parts_raises():
    """Guard: candidates may be non-empty but parts
    empty (e.g. a safety-filtered response) — RuntimeError is expected, not
    IndexError.
    """

    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, json={"candidates": [{"content": {"parts": []}}]})

    client = httpx.Client(transport=httpx.MockTransport(handler))
    with pytest.raises(RuntimeError):
        generate_json("sys", "user", {}, api_key="k", http_client=client)
