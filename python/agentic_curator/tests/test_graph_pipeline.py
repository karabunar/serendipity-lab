"""Verifies that the LangGraph graph carries the local LLM parameters end to end."""

from graph import quality_analyst
from graph.pipeline import enrich_post


def test_enrich_post_passes_llm_params_through_state(monkeypatch):
    captured = {}

    def fake_generate_json(system_instruction, user_text, schema, *, base_url, model, api_key):
        captured["base_url"] = base_url
        captured["model"] = model
        captured["api_key"] = api_key
        return {"quality_score": 0.6}

    monkeypatch.setattr(quality_analyst, "generate_json", fake_generate_json)

    result = enrich_post(
        "p1", "metin", False, base_url="http://llm.local:8000", model="m1", api_key="secret-token"
    )

    assert result == {"post_id": "p1", "quality_score": 0.6}
    assert captured["base_url"] == "http://llm.local:8000"
    assert captured["model"] == "m1"
    assert captured["api_key"] == "secret-token"
