import math

import pytest

from embedder import KIND_PASSAGE, KIND_QUERY, E5Embedder


def dot(a, b):
    return sum(x * y for x, y in zip(a, b))


def test_load_reports_dimension(embedder):
    assert embedder.ready
    assert embedder.dim == 384


def test_vectors_are_l2_normalized(embedder):
    for v in embedder.encode(["Fenerbahçe derbide galip geldi", "Kapadokya'da balon turu"]):
        assert len(v) == 384
        assert math.isclose(math.sqrt(dot(v, v)), 1.0, abs_tol=1e-4)


def test_query_and_passage_prefixes_change_the_vector(embedder):
    q = embedder.encode(["kedi"], KIND_QUERY)[0]
    p = embedder.encode(["kedi"], KIND_PASSAGE)[0]
    assert q != p
    assert dot(q, p) > 0.8  # still the same concept


def test_related_text_ranks_above_unrelated(embedder):
    query = embedder.encode(["futbol maçı sonucu"], KIND_QUERY)[0]
    related, unrelated = embedder.encode(
        ["Fenerbahçe derbide galip geldi", "Kapadokya'da balon turu"], KIND_PASSAGE
    )
    assert dot(query, related) > dot(query, unrelated)


def test_batch_order_is_preserved(embedder):
    texts = ["elma", "araba", "yazılım geliştirme"]
    batch = embedder.encode(texts)
    for text, vec in zip(texts, batch):
        single = embedder.encode([text])[0]
        assert dot(vec, single) > 0.999


def test_empty_input_returns_empty_list(embedder):
    assert embedder.encode([]) == []


def test_encode_before_load_raises():
    with pytest.raises(RuntimeError):
        E5Embedder().encode(["x"])


def test_invalid_batch_size_is_rejected():
    with pytest.raises(ValueError):
        E5Embedder(batch_size=0)


def test_non_e5_models_get_no_prefix():
    e = E5Embedder("some/other-model")
    assert e._prefix("kedi", KIND_QUERY) == "kedi"
    assert E5Embedder("intfloat/multilingual-e5-small")._prefix("kedi", KIND_QUERY) == "query: kedi"
