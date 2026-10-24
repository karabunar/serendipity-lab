"""Dump-backed build: selector parsing, claim ranks, subclass walk, paging, CLI.

The dump is a tiny synthetic file in the Wikidata entity-dump layout; nothing
here reads a real dump.
"""

from __future__ import annotations

import bz2
import json
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

import build_canonical_dict as cli  # noqa: E402
from canonical_dict import build as build_mod  # noqa: E402
from canonical_dict.dump import DumpClient, DumpError, DumpIndex, parse_selector, truthy_ids  # noqa: E402
from canonical_dict.queries import CATEGORIES, build_query, category_by_name  # noqa: E402


def claim(pid: str, qid: str, rank: str = "normal") -> tuple[str, dict]:
    return pid, {
        "rank": rank,
        "mainsnak": {"snaktype": "value", "datavalue": {"value": {"id": qid}}},
    }


def item(qid, tr=None, en=None, claims=(), aliases=(), sitelinks=5):
    ent: dict = {"type": "item", "id": qid, "labels": {}, "aliases": {}, "claims": {}, "sitelinks": {}}
    if tr:
        ent["labels"]["tr"] = {"language": "tr", "value": tr}
    if en:
        ent["labels"]["en"] = {"language": "en", "value": en}
    for lang, value in aliases:
        ent["aliases"].setdefault(lang, []).append({"language": lang, "value": value})
    for pid, st in claims:
        ent["claims"].setdefault(pid, []).append(st)
    for i in range(sitelinks):
        ent["sitelinks"][f"w{i}wiki"] = {"site": f"w{i}wiki", "title": qid}
    return ent


ENTITIES = [
    # class tree: Q476028 (football club) <- Q900 (Turkish top-flight club)
    item("Q476028", en="football club"),
    item("Q900", en="top-flight club", claims=[claim("P279", "Q476028")]),
    item("Q1", tr="Fenerbahçe", en="Fenerbahce SK", aliases=[("tr", "FB"), ("en", "Fener")], sitelinks=50,
         claims=[claim("P31", "Q900"), claim("P17", "Q43")]),
    item("Q2", tr="Galatasaray", sitelinks=40, claims=[claim("P31", "Q476028"), claim("P17", "Q43")]),
    item("Q3", en="FC Barcelona", sitelinks=80, claims=[claim("P31", "Q476028"), claim("P17", "Q29")]),
    # P17 pointing at Turkey only through a DEPRECATED statement → must not match.
    item("Q4", tr="Hayalet Spor", sitelinks=9,
         claims=[claim("P31", "Q476028"), claim("P17", "Q43", rank="deprecated")]),
    # preferred rank hides the normal one (wdt: truthy semantics): club is in Q29, not Q43.
    item("Q5", tr="Çifte Statü", sitelinks=9,
         claims=[claim("P31", "Q476028"), claim("P17", "Q43"), claim("P17", "Q29", rank="preferred")]),
    # no tr/en label → never a row
    item("Q6", claims=[claim("P31", "Q476028"), claim("P17", "Q43")]),
    # a person: only matches the person categories
    item("Q10", tr="Ali Veli", sitelinks=4,
         claims=[claim("P31", "Q5"), claim("P106", "Q937857"), claim("P27", "Q43")]),
    # state_tr: agency in Turkey, one excluded as an embassy
    item("Q20", tr="Bir Kurum", sitelinks=3, claims=[claim("P31", "Q327333"), claim("P17", "Q43")]),
    item("Q21", tr="Bir Büyükelçilik", sitelinks=3,
         claims=[claim("P31", "Q327333"), claim("P31", "Q3917681"), claim("P17", "Q43")]),
    # a property entity must be ignored
    {"type": "property", "id": "P31", "labels": {"en": {"value": "instance of"}}},
]


def write_dump(path: Path, entities=ENTITIES) -> Path:
    lines = ["["] + [json.dumps(e, ensure_ascii=False) + "," for e in entities] + ["]"]
    data = "\n".join(lines) + "\n"
    if path.suffix == ".bz2":
        path.write_bytes(bz2.compress(data.encode("utf-8")))
    else:
        path.write_text(data, encoding="utf-8")
    return path


def qids(rows: list[dict]) -> list[str]:
    return [r["item"]["value"].rsplit("/", 1)[1] for r in rows]


def fetch(client: DumpClient, name: str):
    cat = category_by_name(name)
    return build_mod.fetch_bindings(client, cat)


@pytest.fixture
def client(tmp_path):
    cats = [category_by_name(n) for n in ("football_club_tr", "football_club_world", "footballer_tr", "state_tr")]
    return DumpClient.from_dump(write_dump(tmp_path / "dump.json"), cats)


# ── Selector parsing ─────────────────────────────────────────


def test_every_shipped_category_selector_parses():
    for cat in CATEGORIES:
        sel = parse_selector(cat.selector)
        assert sel.blocks, cat.name


def test_union_selector_becomes_alternatives_and_shared_claim():
    sel = parse_selector(category_by_name("football_club_tr").selector)
    union, shared = sel.blocks
    assert {next(iter(a[0].targets)) for a in union} == {"Q476028", "Q103229495"}
    assert all(a[0].closure for a in union)
    assert shared[0][0].pid == "P17" and shared[0][0].targets == {"Q43"}


def test_values_and_exclusions_are_bound():
    sel = parse_selector(category_by_name("state_tr").selector)
    first = sel.blocks[0][0][0]
    assert first.closure and "Q41487" in first.targets
    assert {t.pid for t in sel.exclusions} == {"P31"} and "Q3917681" in sel.exclusions[0].targets


@pytest.mark.parametrize(
    "bad",
    ["", "?item wdt:P31 ?unbound .", "?item wdt:P17/wdt:P279* wd:Q1 .", "?item rdfs:label ?l ."],
)
def test_unsupported_selector_fails_loudly(bad):
    with pytest.raises(DumpError):
        parse_selector(bad)


def test_truthy_ids_prefers_preferred_and_drops_deprecated():
    ent = item("Q1", claims=[claim("P17", "Q1"), claim("P17", "Q2", "preferred"), claim("P17", "Q3", "deprecated")])
    assert truthy_ids(ent, "P17") == {"Q2"}
    ent = item("Q1", claims=[claim("P17", "Q1"), claim("P17", "Q3", "deprecated")])
    assert truthy_ids(ent, "P17") == {"Q1"}


# ── Matching ─────────────────────────────────────────────────


def test_subclass_walk_and_country_claim(client):
    rows, hit = fetch(client, "football_club_tr")
    assert not hit
    # Q1 via the subclass (Q900), Q2 direct. Not: Q3 (other country), Q4 (deprecated
    # P17), Q5 (preferred P17 elsewhere), Q6 (no label).
    assert sorted(set(qids(rows))) == ["Q1", "Q2"]


def test_world_category_has_no_country_filter_but_respects_floor(client):
    rows, _ = fetch(client, "football_club_world")  # min_sitelinks keeps only well-known ones
    assert all(int(r["sitelinks"]["value"]) >= category_by_name("football_club_world").min_sitelinks for r in rows)


def test_exclusion_removes_the_embassy(client):
    rows, _ = fetch(client, "state_tr")
    assert qids(rows) == ["Q20"]


def test_person_does_not_leak_into_club_categories(client):
    for name in ("football_club_tr", "football_club_world", "state_tr"):
        rows, _ = fetch(client, name)
        assert "Q10" not in qids(rows)
    rows, _ = fetch(client, "footballer_tr")
    assert qids(rows) == ["Q10"]


def test_rows_follow_the_sparql_binding_shape(client):
    rows, _ = fetch(client, "football_club_tr")
    fener = [r for r in rows if r["item"]["value"].endswith("/Q1")]
    assert sorted(r["alias"]["value"] for r in fener) == ["FB", "Fener"]
    assert fener[0]["lblTr"]["value"] == "Fenerbahçe" and fener[0]["lblEn"]["value"] == "Fenerbahce SK"
    assert fener[0]["sitelinks"]["value"] == "50"
    galata = [r for r in rows if r["item"]["value"].endswith("/Q2")]
    assert len(galata) == 1 and "alias" not in galata[0] and "lblEn" not in galata[0]


def test_order_is_sitelinks_desc_then_item(client):
    rows, _ = fetch(client, "football_club_tr")
    assert qids(rows)[0] == "Q1"  # 50 sitelinks before 40


def test_paging_matches_the_query_contract(client):
    cat = category_by_name("football_club_tr")
    page1 = client.select(build_query(cat, limit=2, offset=0))
    page2 = client.select(build_query(cat, limit=2, offset=2))
    assert len(page1) == 2 and len(page2) == 1
    assert client.select(build_query(cat, limit=2, offset=4)) == []


def test_foreign_query_is_rejected(client):
    with pytest.raises(DumpError):
        client.select(build_query(category_by_name("film"), limit=10, offset=0))
    with pytest.raises(DumpError):
        client.select("SELECT * WHERE {}")


def test_bz2_dump_gives_the_same_index(tmp_path):
    cats = [category_by_name("football_club_tr")]
    plain = DumpIndex.build(write_dump(tmp_path / "d.json"), cats)
    packed = DumpIndex.build(write_dump(tmp_path / "d.json.bz2"), cats)
    assert plain.hits == packed.hits and plain.items_seen == 11  # the property entity is skipped


def test_malformed_line_names_the_file(tmp_path):
    bad = tmp_path / "bad.json"
    bad.write_text("[\n{not json},\n]\n", encoding="utf-8")
    with pytest.raises(DumpError, match="bad.json"):
        DumpIndex.build(bad, [category_by_name("film")])


# ── End to end ───────────────────────────────────────────────


def test_full_build_through_the_unchanged_pipeline(tmp_path):
    cats = [category_by_name("football_club_tr")]
    client = DumpClient.from_dump(write_dump(tmp_path / "d.json"), cats)
    doc, report = build_mod.run(client, {}, set(), cats, set())
    assert doc["entities"]  # Q1 and Q2 both made it
    assert report.per_category_rows["football_club_tr"] == 3  # Fenerbahçe x2 aliases + Galatasaray
    flat = set(doc["aliases"].values())
    assert {"Fenerbahçe", "Galatasaray"} <= flat


def test_cli_dump_flag_writes_the_dictionary(tmp_path):
    out = tmp_path / "dict.json"
    dump = write_dump(tmp_path / "d.json")
    rc = cli.main(["--dump", str(dump), "--category", "football_club_tr", "--out", str(out), "--allow-empty-categories"])
    assert rc == 0
    doc = json.loads(out.read_text(encoding="utf-8"))
    assert "fb" in doc["aliases"]


def test_cli_dump_and_from_json_are_exclusive(tmp_path):
    assert cli.main(["--dump", str(tmp_path / "x"), "--from-json", str(tmp_path / "y")]) == 2


def test_cli_missing_dump_file(tmp_path):
    assert cli.main(["--dump", str(tmp_path / "nope.json.bz2")]) == 2
