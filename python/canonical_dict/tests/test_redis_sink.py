"""Redis writing — with fakeredis, without a real Redis."""

from __future__ import annotations

import json

import fakeredis
import pytest

from canonical_dict import redis_sink


@pytest.fixture
def rdb():
    return fakeredis.FakeRedis(decode_responses=True)


def doc(aliases: dict[str, str], entities: dict[str, dict] | None = None) -> dict:
    return {
        "meta": {"generated_at": "2026-08-11T00:00:00+00:00", "alias_count": len(aliases)},
        "entities": entities if entities is not None else {},
        "aliases": aliases,
    }


def test_write_leaves_two_hashes_and_meta(rdb):
    redis_sink.write(rdb, doc({"fb": "Fenerbahçe"}, {"Fenerbahçe": {"qid": "Q1", "category": "c", "sitelinks": 40}}))

    assert rdb.hget(redis_sink.KEY_ALIAS, "fb") == "Fenerbahçe"
    entity = json.loads(rdb.hget(redis_sink.KEY_ENTITY, "Fenerbahçe"))
    assert entity["qid"] == "Q1"
    assert json.loads(rdb.get(redis_sink.KEY_META))["alias_count"] == 1


def test_write_leaves_no_build_keys(rdb):
    redis_sink.write(rdb, doc({"fb": "Fenerbahçe"}))
    leftovers = [k for k in rdb.keys("serendipity:canonical:*") if k.endswith(":build")]
    assert leftovers == []


def test_rewrite_clears_old_aliases(rdb):
    # With an in-place HSET the "eski" alias would stay forever — this is the
    # main reason for the RENAME swap.
    redis_sink.write(rdb, doc({"eski": "Eski"}))
    redis_sink.write(rdb, doc({"yeni": "Yeni"}))
    assert rdb.hget(redis_sink.KEY_ALIAS, "eski") is None
    assert rdb.hget(redis_sink.KEY_ALIAS, "yeni") == "Yeni"


def test_half_finished_previous_build_is_cleaned(rdb):
    rdb.hset(redis_sink.KEY_ALIAS + ":build", mapping={"cop": "Cop"})
    redis_sink.write(rdb, doc({"fb": "Fenerbahçe"}))
    assert rdb.hget(redis_sink.KEY_ALIAS, "cop") is None
    assert rdb.hget(redis_sink.KEY_ALIAS, "fb") == "Fenerbahçe"


def test_no_ttl_is_set(rdb):
    # Deliberate: the dictionary is a managed artifact, and a TTL would silently
    # delete it on a missed run (see the redis_sink module docstring).
    redis_sink.write(rdb, doc({"fb": "Fenerbahçe"}))
    assert rdb.ttl(redis_sink.KEY_ALIAS) == -1


def test_chunked_write_works_on_large_dictionary(rdb):
    big = {f"a{i}": f"Kanonik{i}" for i in range(redis_sink._CHUNK * 2 + 7)}
    redis_sink.write(rdb, doc(big))
    assert rdb.hlen(redis_sink.KEY_ALIAS) == len(big)


def test_empty_dictionary_clears_previous_generation_COMPLETELY(rdb):
    # The old version asserted hlen == 0 on an empty DB; it would pass even if
    # write() were a no-op. The meaningful form: swap a FULL generation with an
    # empty one.
    redis_sink.write(rdb, doc({"eski": "Eski"}, {"Eski": {"qid": "Q1", "category": "c", "sitelinks": 1}}))
    assert rdb.hlen(redis_sink.KEY_ALIAS) == 1

    redis_sink.write(rdb, doc({}))
    # An empty HASH does not exist in Redis at all → the keys must have been
    # DELETED, the old generation must not leak through.
    assert rdb.exists(redis_sink.KEY_ALIAS) == 0
    assert rdb.exists(redis_sink.KEY_ENTITY) == 0
    # the meta is still written: "an empty generation was published" must be traceable.
    assert json.loads(rdb.get(redis_sink.KEY_META))["alias_count"] == 0


# ── atomicity ────────────────────────────────────────────────


def gen(n: str) -> dict:
    return doc({n: n.upper()}, {n.upper(): {"qid": "Q" + n, "category": "c", "sitelinks": 1}})


def live_pair(rdb) -> tuple[list[str], list[str]]:
    return sorted(rdb.hkeys(redis_sink.KEY_ALIAS)), sorted(rdb.hkeys(redis_sink.KEY_ENTITY))


def test_swap_cannot_degrade_to_plain_rename_pair(rdb, monkeypatch):
    # Replacing the Lua EVAL with two plain rename() calls would pass the whole
    # file. Here we blow up the entity rename: the real (atomic) impl never
    # touches rename, so the swap completes and the live pair stays CONSISTENT;
    # a two-rename impl would leave the alias new and the entity old.
    redis_sink.write(rdb, gen("a"))

    def exploding_rename(src, dst):
        if str(src).startswith(redis_sink.KEY_ENTITY):
            raise RuntimeError("takas ortasinda cokme")
        return rdb_rename(src, dst)

    rdb_rename = rdb.rename
    monkeypatch.setattr(rdb, "rename", exploding_rename)

    redis_sink.write(rdb, gen("b"))

    aliases, entities = live_pair(rdb)
    assert aliases == ["b"], "alias tarafi takas edilmedi"
    assert entities == ["B"], "entity tarafi alias'tan AYRISTI — takas atomik degil"


def test_if_swap_blows_up_live_dictionary_stays_previous_generation(rdb, monkeypatch):
    redis_sink.write(rdb, gen("a"))

    def exploding_eval(*_a, **_kw):
        raise RuntimeError("eval coktu")

    monkeypatch.setattr(rdb, "eval", exploding_eval)
    with pytest.raises(RuntimeError):
        redis_sink.write(rdb, gen("b"))

    # The new generation waits in :build; the LIVE pair must be the complete old generation.
    assert live_pair(rdb) == (["a"], ["A"])


# ── TTL of the build keys ────────────────────────────────────


def test_build_keys_get_ttl_live_keys_do_not(rdb, monkeypatch):
    # The TTL cleans up the ~8.5 MB of orphaned data from a crash before the
    # swap; but RENAME CARRIES the TTL, so Lua must call PERSIST after the
    # rename — otherwise the live dictionary silently evaporates an hour later.
    seen: dict[str, int] = {}
    orig_eval = rdb.eval

    def spying_eval(script, numkeys, *keys):
        for k in keys:
            ttl = rdb.ttl(k)
            if ttl > 0:
                seen[k] = ttl
        return orig_eval(script, numkeys, *keys)

    monkeypatch.setattr(rdb, "eval", spying_eval)
    redis_sink.write(rdb, gen("a"))

    assert redis_sink.KEY_ALIAS + ":build" in seen, ":build anahtarlari TTL'siz yazildi"
    for key in (redis_sink.KEY_ALIAS, redis_sink.KEY_ENTITY, redis_sink.KEY_META):
        assert rdb.ttl(key) == -1, f"{key} TTL aldi — PERSIST yapilmamis"


def test_dry_run_does_not_touch_redis(rdb):
    redis_sink.write(rdb, doc({"fb": "Fenerbahçe"}), dry_run=True)
    assert rdb.exists(redis_sink.KEY_ALIAS) == 0


def test_estimate_bytes_is_reasonable():
    d = doc({"fb": "Fenerbahçe"}, {"Fenerbahçe": {"qid": "Q1", "category": "c", "sitelinks": 40}})
    assert redis_sink.estimate_bytes(d) > 0
