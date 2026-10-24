"""Quality Analyst's contract with the local LLM client."""

import json
import logging

import pytest

from graph import quality_analyst
from graph.quality_analyst import score_quality


def test_score_quality_returns_llm_score(monkeypatch):
    captured = {}

    def fake_generate_json(system_instruction, user_text, schema, *, base_url, model, api_key):
        captured["system"] = system_instruction
        captured["user"] = user_text
        captured["schema"] = schema
        captured["base_url"] = base_url
        captured["model"] = model
        captured["api_key"] = api_key
        return {"quality_score": 0.82}

    monkeypatch.setattr(quality_analyst, "generate_json", fake_generate_json)

    score = score_quality(
        "derin bir gonderi", base_url="http://llm.local:8000", model="m1", api_key="secret-token"
    )

    assert score == pytest.approx(0.82)
    assert captured["user"] == "derin bir gonderi"
    assert captured["base_url"] == "http://llm.local:8000"
    assert captured["model"] == "m1"
    assert captured["api_key"] == "secret-token"


def test_score_quality_schema_uses_standard_json_schema_types(monkeypatch):
    """guided_json expects standard JSON Schema — Gemini's uppercase
    OBJECT/NUMBER form is rejected by vLLM."""
    captured = {}

    def fake_generate_json(system_instruction, user_text, schema, *, base_url, model, api_key):
        captured["schema"] = schema
        return {"quality_score": 0.5}

    monkeypatch.setattr(quality_analyst, "generate_json", fake_generate_json)
    score_quality("x", base_url="http://llm.local:8000", model="m1")

    assert captured["schema"]["type"] == "object"
    assert captured["schema"]["properties"]["quality_score"]["type"] == "number"


def test_score_quality_empty_text_skips_llm(monkeypatch):
    def fail(*args, **kwargs):
        raise AssertionError("bos metinde LLM cagrilmamali")

    monkeypatch.setattr(quality_analyst, "generate_json", fail)

    assert score_quality("", base_url="http://llm.local:8000", model="m1") == 0.0


def test_score_quality_llm_error_returns_zero(monkeypatch):
    def boom(*args, **kwargs):
        raise RuntimeError("llm down")

    monkeypatch.setattr(quality_analyst, "generate_json", boom)

    assert score_quality("metin", base_url="http://llm.local:8000", model="m1") == 0.0


@pytest.mark.parametrize("raw,expected", [(1.7, 1.0), (-0.4, 0.0)])
def test_score_quality_clamps_out_of_range(monkeypatch, raw, expected):
    monkeypatch.setattr(
        quality_analyst,
        "generate_json",
        lambda *a, **k: {"quality_score": raw},
    )

    assert score_quality("metin", base_url="http://llm.local:8000", model="m1") == expected


def test_score_quality_json_decode_error_logs_exception_type(monkeypatch, caplog):
    """G2 acceptance criterion: JSONDecodeError must appear BY TYPE NAME in
    real logs. str(JSONDecodeError) does not include the type ("Expecting
    value: line 1 column 1 (char 0)") - so logging must use %r, NOT %s."""

    def boom(*args, **kwargs):
        json.loads("elbette, iste skor: 0.7")  # raises a real JSONDecodeError

    monkeypatch.setattr(quality_analyst, "generate_json", boom)

    with caplog.at_level(logging.WARNING, logger="serendipity.curator.quality_analyst"):
        score = score_quality("metin", base_url="http://llm.local:8000", model="m1")

    assert score == 0.0
    assert len(caplog.records) == 1
    assert "JSONDecodeError" in caplog.records[0].getMessage()
