"""CLI safety valves — without touching the network or Redis."""

from __future__ import annotations

import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

import build_canonical_dict as cli  # noqa: E402


def write_doc(tmp_path: Path, aliases: dict[str, str]) -> Path:
    p = tmp_path / "dict.json"
    p.write_text(
        json.dumps({"meta": {"alias_count": len(aliases)}, "entities": {}, "aliases": aliases}),
        encoding="utf-8",
    )
    return p


def test_empty_dictionary_is_rejected(tmp_path):
    src = write_doc(tmp_path, {})
    assert cli.main(["--from-json", str(src)]) == 4


def test_empty_dictionary_passes_with_allow_empty(tmp_path):
    src = write_doc(tmp_path, {})
    assert cli.main(["--from-json", str(src), "--allow-empty"]) == 0


def test_exceeding_ceiling_is_rejected(tmp_path):
    src = write_doc(tmp_path, {"a1": "A", "a2": "B", "a3": "C"})
    assert cli.main(["--from-json", str(src), "--max-aliases", "2"]) == 3


def test_unknown_category_returns_error():
    assert cli.main(["--category", "boyle_bir_kategori_yok", "--dry-run"]) == 2


def test_json_output_is_written(tmp_path):
    src = write_doc(tmp_path, {"fb": "Fenerbahçe"})
    out = tmp_path / "out.json"
    assert cli.main(["--from-json", str(src), "--out", str(out)]) == 0
    assert json.loads(out.read_text(encoding="utf-8"))["aliases"]["fb"] == "Fenerbahçe"


# ── C1: partial publish ──────────────────────────────────────


def test_category_and_redis_url_together_are_rejected():
    # --category produces a PARTIAL document; redis_sink.write swaps it with the
    # WHOLE live dictionary. It must stop at the argument level, without ever
    # touching the network.
    rc = cli.main(["--category", "football_club_tr", "--redis-url", "redis://127.0.0.1:1/0"])
    assert rc == 8


def test_category_and_redis_url_pass_with_partial_ok(monkeypatch, tmp_path):
    # Once the gate is passed the flow must continue (up to SPARQL). We fake the
    # client to avoid touching the network.
    calls = {"n": 0}

    class FakeClient:
        def __init__(self, *_a, **_kw):
            pass

        def __enter__(self):
            return self

        def __exit__(self, *_exc):
            return None

        def select(self, _query):
            calls["n"] += 1
            return []

    monkeypatch.setattr(cli, "SPARQLClient", FakeClient)
    monkeypatch.setattr(cli.SPARQLConfig, "from_env", staticmethod(lambda: cli.SPARQLConfig()))
    rc = cli.main(
        ["--category", "football_club_tr", "--redis-url", "redis://127.0.0.1:1/0", "--partial-ok"]
    )
    # NOT 8: the gate was passed. (It stops at 4 because the category returned 0 rows.)
    assert rc != 8
    assert calls["n"] >= 1


# ── C1: empty category gate ──────────────────────────────────


def build_report(empty=(), limit_hit=()):
    r = cli.build_mod.BuildReport()
    r.empty_categories = list(empty)
    r.limit_hit_categories = list(limit_hit)
    r.pre_override_alias_count = 5
    return r


def stub_run(monkeypatch, report):
    """Replaces `build_mod.run` with a fake result, without touching the network."""
    doc = {
        "meta": {"alias_count": 6, "pre_override_alias_count": report.pre_override_alias_count},
        "entities": {},
        "aliases": {f"a{i}": "A" for i in range(6)},
    }

    class FakeClient:
        def __enter__(self):
            return self

        def __exit__(self, *_exc):
            return None

    monkeypatch.setattr(cli, "SPARQLClient", lambda *_a, **_kw: FakeClient())
    monkeypatch.setattr(cli.SPARQLConfig, "from_env", staticmethod(lambda: cli.SPARQLConfig()))
    monkeypatch.setattr(cli.build_mod, "run", lambda *_a, **_kw: (doc, report))
    return doc


def test_publish_rejected_when_category_is_empty(monkeypatch):
    stub_run(monkeypatch, build_report(empty=["district_tr"]))
    assert cli.main(["--dry-run"]) == 5


def test_empty_category_passes_with_flag(monkeypatch):
    stub_run(monkeypatch, build_report(empty=["district_tr"]))
    assert cli.main(["--dry-run", "--allow-empty-categories"]) == 0


def test_publish_rejected_when_category_is_truncated(monkeypatch):
    stub_run(monkeypatch, build_report(limit_hit=["city_world"]))
    assert cli.main(["--dry-run"]) == 6


def test_truncated_category_passes_with_its_own_flag(monkeypatch):
    stub_run(monkeypatch, build_report(limit_hit=["city_world"]))
    assert cli.main(["--dry-run", "--allow-truncated-categories"]) == 0


def test_empty_category_flag_does_not_silence_truncated_category(monkeypatch):
    # The two failures are under separate flags: saying "I knowingly accept
    # truncation" must not also silence a newly broken class QID.
    stub_run(monkeypatch, build_report(empty=["district_tr"], limit_hit=["city_world"]))
    assert cli.main(["--dry-run", "--allow-empty-categories"]) == 6
    assert cli.main(["--dry-run", "--allow-truncated-categories"]) == 5


# ── C1: empty-dictionary gate before overrides ───────────────


def test_rejected_when_overrides_filled_but_wikidata_empty(tmp_path):
    # The disaster where WDQS returns 0 rows for all categories: the document
    # consists only of overrides and the old gate (alias_count > 0) would have
    # PASSED it.
    p = tmp_path / "dict.json"
    p.write_text(
        json.dumps(
            {
                "meta": {"alias_count": 2, "pre_override_alias_count": 0},
                "entities": {},
                "aliases": {"fb": "Fenerbahçe", "gs": "Galatasaray"},
            }
        ),
        encoding="utf-8",
    )
    assert cli.main(["--from-json", str(p)]) == 4
    assert cli.main(["--from-json", str(p), "--allow-empty"]) == 0


def test_existing_behavior_kept_when_field_missing_in_old_document(tmp_path):
    # An old document loaded with `--from-json` carries no
    # `pre_override_alias_count`; the gate must fall back to alias_count.
    src = write_doc(tmp_path, {"fb": "Fenerbahçe"})
    assert cli.main(["--from-json", str(src)]) == 0


# ── C1: shrink guard ─────────────────────────────────────────


class FakeRedis:
    def __init__(self, meta):
        self._meta = meta
        self.written = False

    def get(self, _key):
        return self._meta


def shrink_rc(monkeypatch, live_alias_count, new_aliases, extra=()):
    fake = FakeRedis(json.dumps({"alias_count": live_alias_count}) if live_alias_count is not None else None)
    monkeypatch.setattr(cli.redis_sink, "write", lambda *a, **kw: setattr(fake, "written", True))
    import redis as redis_mod

    monkeypatch.setattr(redis_mod, "from_url", lambda *_a, **_kw: fake)
    doc = {"meta": {"alias_count": new_aliases}, "entities": {}, "aliases": {f"a{i}": "A" for i in range(new_aliases)}}

    import tempfile

    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False, encoding="utf-8") as fh:
        json.dump(doc, fh)
        path = fh.name
    rc = cli.main(["--from-json", path, "--redis-url", "redis://x/0", *extra])
    return rc, fake


def test_shrinkage_is_rejected(monkeypatch):
    rc, fake = shrink_rc(monkeypatch, live_alias_count=100, new_aliases=50)
    assert rc == 7
    assert not fake.written


def test_shrinkage_passes_with_force(monkeypatch):
    rc, fake = shrink_rc(monkeypatch, live_alias_count=100, new_aliases=50, extra=["--force"])
    assert rc == 0
    assert fake.written


def test_small_shrinkage_below_threshold_passes(monkeypatch):
    rc, fake = shrink_rc(monkeypatch, live_alias_count=100, new_aliases=80)
    assert rc == 0
    assert fake.written


def test_check_skipped_when_meta_missing(monkeypatch):
    # On first install there is no live meta; that run must not be rejected.
    rc, fake = shrink_rc(monkeypatch, live_alias_count=None, new_aliases=5)
    assert rc == 0
    assert fake.written
