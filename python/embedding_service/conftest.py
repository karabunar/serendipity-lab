"""Shared fixtures: the tests use the REAL model, never a stand-in.

The model is NOT downloaded by the tests. If it is not already in the local
HuggingFace cache (or sentence-transformers is missing), the tests are skipped.
Warm the cache once with `python main.py` or any `E5Embedder().load()`.
"""
import pytest

from embedder import DEFAULT_MODEL, E5Embedder


def _model_cached(name: str) -> bool:
    try:
        from huggingface_hub import try_to_load_from_cache
    except ImportError:
        return False
    return isinstance(try_to_load_from_cache(name, "config.json"), str)


@pytest.fixture(scope="session")
def embedder():
    pytest.importorskip("sentence_transformers")
    if not _model_cached(DEFAULT_MODEL):
        pytest.skip(f"{DEFAULT_MODEL} is not in the local cache (tests never download it)")
    e = E5Embedder(DEFAULT_MODEL, device="cpu")
    e.load()
    return e
