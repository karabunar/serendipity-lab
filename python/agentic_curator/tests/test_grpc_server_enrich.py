"""Verifies that the EnrichPosts servicer uses the LLM settings coming from the
constructor. The servicer does NOT read env — validation happens at startup (main.load_llm_config)."""

import curator_pb2
import grpc_server
from grpc_server import PostCuratorServicer


def test_enrich_posts_uses_injected_llm_config(monkeypatch):
    captured = {}

    def fake_enrich_post(post_id, text, has_media, *, base_url, model, api_key):
        captured["base_url"] = base_url
        captured["model"] = model
        captured["api_key"] = api_key
        return {"post_id": post_id, "quality_score": 0.9}

    monkeypatch.setattr(grpc_server, "enrich_post", fake_enrich_post)

    servicer = PostCuratorServicer(
        base_url="http://llm.local:8000", model="m1", api_key="secret-token"
    )
    request = curator_pb2.EnrichPostsRequest(
        candidates=[curator_pb2.PostCandidate(post_id="p1", text="metin", has_media=False)]
    )
    response = servicer.EnrichPosts(request, None)

    assert len(response.results) == 1
    assert response.results[0].post_id == "p1"
    assert captured["base_url"] == "http://llm.local:8000"
    assert captured["model"] == "m1"
    assert captured["api_key"] == "secret-token"


def test_enrich_posts_default_api_key_is_blank(monkeypatch):
    """If api_key is not given the servicer runs with an empty string (optional)."""
    captured = {}

    def fake_enrich_post(post_id, text, has_media, *, base_url, model, api_key):
        captured["api_key"] = api_key
        return {"post_id": post_id, "quality_score": 0.9}

    monkeypatch.setattr(grpc_server, "enrich_post", fake_enrich_post)

    servicer = PostCuratorServicer(base_url="http://llm.local:8000", model="m1")
    request = curator_pb2.EnrichPostsRequest(
        candidates=[curator_pb2.PostCandidate(post_id="p1", text="metin", has_media=False)]
    )
    servicer.EnrichPosts(request, None)

    assert captured["api_key"] == ""


def test_enrich_posts_does_not_read_env(monkeypatch):
    """This test catches an accidental return to reading env."""
    monkeypatch.delenv("LOCAL_LLM_BASE_URL", raising=False)
    monkeypatch.delenv("LOCAL_LLM_MODEL", raising=False)
    monkeypatch.delenv("LOCAL_LLM_API_KEY", raising=False)
    monkeypatch.setattr(
        grpc_server,
        "enrich_post",
        lambda post_id, text, has_media, *, base_url, model, api_key: {
            "post_id": post_id, "quality_score": 0.1
        },
    )

    servicer = PostCuratorServicer(base_url="http://llm.local:8000", model="m1")
    request = curator_pb2.EnrichPostsRequest(
        candidates=[curator_pb2.PostCandidate(post_id="p1", text="m", has_media=False)]
    )

    assert len(servicer.EnrichPosts(request, None).results) == 1
