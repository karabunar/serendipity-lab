"""Tests locking that the local LLM configuration is validated AT STARTUP.
Repo pattern: with a missing required config the pod never starts."""

import pytest

from main import load_llm_config


def test_load_llm_config_returns_all_values(monkeypatch):
    monkeypatch.setenv("LOCAL_LLM_BASE_URL", "http://llm.local:8000")
    monkeypatch.setenv("LOCAL_LLM_MODEL", "m1")
    monkeypatch.setenv("LOCAL_LLM_API_KEY", "secret-token")

    assert load_llm_config() == ("http://llm.local:8000", "m1", "secret-token")


def test_load_llm_config_strips_whitespace(monkeypatch):
    monkeypatch.setenv("LOCAL_LLM_BASE_URL", "  http://llm.local:8000  ")
    monkeypatch.setenv("LOCAL_LLM_MODEL", " m1 ")
    monkeypatch.setenv("LOCAL_LLM_API_KEY", "  secret-token  ")

    assert load_llm_config() == ("http://llm.local:8000", "m1", "secret-token")


@pytest.mark.parametrize("api_key_env", [None, "", "   "])
def test_load_llm_config_api_key_absent_or_blank_is_valid(monkeypatch, api_key_env):
    """LOCAL_LLM_API_KEY, unlike LOCAL_LLM_BASE_URL/LOCAL_LLM_MODEL, is
    OPTIONAL — being missing/empty must not raise RuntimeError."""
    monkeypatch.setenv("LOCAL_LLM_BASE_URL", "http://llm.local:8000")
    monkeypatch.setenv("LOCAL_LLM_MODEL", "m1")
    if api_key_env is None:
        monkeypatch.delenv("LOCAL_LLM_API_KEY", raising=False)
    else:
        monkeypatch.setenv("LOCAL_LLM_API_KEY", api_key_env)

    assert load_llm_config() == ("http://llm.local:8000", "m1", "")


@pytest.mark.parametrize("missing", ["LOCAL_LLM_BASE_URL", "LOCAL_LLM_MODEL"])
def test_load_llm_config_raises_when_env_missing(monkeypatch, missing):
    monkeypatch.setenv("LOCAL_LLM_BASE_URL", "http://llm.local:8000")
    monkeypatch.setenv("LOCAL_LLM_MODEL", "m1")
    monkeypatch.delenv(missing, raising=False)

    with pytest.raises(RuntimeError, match=missing):
        load_llm_config()


def test_load_llm_config_raises_when_env_blank(monkeypatch):
    """Setting an empty string must be treated the same as 'unset'."""
    monkeypatch.setenv("LOCAL_LLM_BASE_URL", "http://llm.local:8000")
    monkeypatch.setenv("LOCAL_LLM_MODEL", "   ")

    with pytest.raises(RuntimeError, match="LOCAL_LLM_MODEL"):
        load_llm_config()
