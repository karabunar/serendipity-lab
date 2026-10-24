"""Tests for the dictionary build logic — without ever touching the network."""

from __future__ import annotations

import json
import re
from pathlib import Path

from canonical_dict.build import (
    MIN_OVERRIDE_ALIAS_LEN,
    AMBIGUITY_RATIO,
    BuildReport,
    EntityRecord,
    add_district_bare_aliases,
    apply_overrides,
    build_alias_index,
    fetch_bindings,
    fetch_category,
    load_blocklist,
    load_overrides,
    merge_entities,
    normalize_alias,
    run,
    to_document,
)
from canonical_dict.queries import Category, build_query


class FakeClient:
    """Stands in for SPARQLClient; returns bindings based on the query text."""

    def __init__(self, by_selector: dict[str, list[dict]]):
        self.by_selector = by_selector
        self.queries: list[str] = []

    def select(self, query: str) -> list[dict]:
        self.queries.append(query)
        for frag, rows in self.by_selector.items():
            if frag in query:
                return rows
        return []


def binding(qid: str, tr: str = "", en: str = "", alias: str = "", sitelinks: int = 10) -> dict:
    b: dict = {
        "item": {"value": f"http://www.wikidata.org/entity/{qid}"},
        "sitelinks": {"value": str(sitelinks)},
    }
    if tr:
        b["lblTr"] = {"value": tr}
    if en:
        b["lblEn"] = {"value": en}
    if alias:
        b["alias"] = {"value": alias}
    return b


def cat(name: str, selector: str, priority: int = 50) -> Category:
    return Category(name=name, selector=selector, min_sitelinks=0, priority=priority)


# ── normalize_alias ──────────────────────────────────────────


def test_normalize_alias_strips_qualifier():
    assert normalize_alias("Fenerbahçe (spor kulübü)") == "fenerbahce"


def test_normalize_alias_collapses_whitespace():
    assert normalize_alias("  Sarı   Lacivert  ") == "sari lacivert"


def test_normalize_alias_rejects_single_character():
    assert normalize_alias("F") is None


def test_normalize_alias_rejects_two_character_DERIVED_name():
    # On a real post sample, 2-letter surfaces derived from Wikidata produced many matches that an
    # independent NER did not back; the whole top was ISO country codes (na→Namibia).
    # The other side was nearly empty: fb, gs, ak almost never matched.
    assert normalize_alias("FB") is None


def test_normalize_alias_accepts_curated_two_character():
    # The override path may fall BELOW the automatic threshold: a hand-touched abbreviation is not
    # subject to the same gate as derived noise.
    assert normalize_alias("FB", min_len=MIN_OVERRIDE_ALIAS_LEN) == "fb"


def test_normalize_alias_rejects_letterless_input():
    assert normalize_alias("1907") is None
    assert normalize_alias("--") is None


def test_normalize_alias_rejects_empty_input():
    assert normalize_alias("   ") is None
    assert normalize_alias("(sadece aciklama)") is None


# ── label selection and fetch ────────────────────────────────


def test_fetch_category_prefers_tr_label():
    client = FakeClient({"SELECTOR_A": [binding("Q1", tr="Fenerbahçe", en="Fenerbahce S.K.")]})
    rec = fetch_category(client, cat("c1", "SELECTOR_A"), BuildReport())
    assert rec["Q1"].canonical == "Fenerbahçe"


def test_fetch_category_uses_en_when_tr_missing():
    client = FakeClient({"SELECTOR_A": [binding("Q1", en="OpenAI")]})
    rec = fetch_category(client, cat("c1", "SELECTOR_A"), BuildReport())
    assert rec["Q1"].canonical == "OpenAI"


def test_fetch_category_also_makes_english_label_an_alias():
    # Even WHEN the tr label exists, the en label must be a search key: the "Barcelona"/"Munich"
    # form is common in Turkish posts, but the canonical name is "Barselona"/"Münih". Regression:
    # previously lblEn was used as the canonical name only when tr was missing, and was NEVER added
    # as an alias.
    client = FakeClient({"SELECTOR_A": [binding("Q1", tr="Barselona", en="Barcelona")]})
    rec = fetch_category(client, cat("c1", "SELECTOR_A"), BuildReport())
    assert rec["Q1"].canonical == "Barselona"
    assert "Barcelona" in rec["Q1"].raw_aliases


def test_alias_index_entity_does_not_compete_with_own_alias():
    # A canonical name and an altLabel of one entity can fold to the SAME key ("Fenerbahçe" +
    # "Fenerbahçe (futbol takımı)" → `fenerbahce`). Regression: when the bucket was a list, the
    # record entered twice, the runner-up became THE ENTITY ITSELF, the sitelink ratio dropped to
    # 1.0 and the alias was dropped even though the real rival had only 8 sitelinks.
    buyuk = EntityRecord(
        "Q1", "Fenerbahçe", "football_club_tr", 90, 92, {"Fenerbahçe (futbol takımı)"}
    )
    kadin = EntityRecord("Q2", "Fenerbahçe (kadın futbol takımı)", "football_club_tr", 90, 8)
    report = BuildReport()
    aliases = build_alias_index({"Q1": buyuk, "Q2": kadin}, set(), report)
    assert aliases["fenerbahce"] == "Fenerbahçe"
    assert report.dropped_ambiguous == []


def test_fetch_category_collects_aliases_for_same_qid():
    client = FakeClient(
        {
            "SELECTOR_A": [
                binding("Q1", tr="Fenerbahçe", alias="Fener"),
                binding("Q1", tr="Fenerbahçe", alias="FB"),
            ]
        }
    )
    rec = fetch_category(client, cat("c1", "SELECTOR_A"), BuildReport())
    assert rec["Q1"].raw_aliases == {"Fener", "FB"}


def test_fetch_category_empty_result_is_reported():
    # A wrong class QID raises no error, it returns 0 rows — must not stay silent.
    report = BuildReport()
    client = FakeClient({})
    out = fetch_category(client, cat("district_tr", "SELECTOR_YOK"), report)
    assert out == {}
    assert "district_tr" in report.empty_categories


def test_fetch_category_hitting_limit_is_reported():
    report = BuildReport()
    small = Category(name="c1", selector="SELECTOR_A", min_sitelinks=0, priority=50, limit=2)
    client = FakeClient({"SELECTOR_A": [binding("Q1", tr="A"), binding("Q2", tr="B")]})
    fetch_category(client, small, report)
    assert "c1" in report.limit_hit_categories


# ── merge ────────────────────────────────────────────────────


def test_merge_higher_priority_wins_aliases_are_merged():
    low = EntityRecord("Q1", "Kadıköy", "district_tr", 52, 20, {"Kadikoy"})
    high = EntityRecord("Q1", "Kadıköy", "city_tr", 66, 20, {"Kadıköy ilçesi"})
    merged = merge_entities([{"Q1": low}, {"Q1": high}])
    assert merged["Q1"].category == "city_tr"
    assert merged["Q1"].raw_aliases == {"Kadikoy", "Kadıköy ilçesi"}


# ── collision resolution ─────────────────────────────────────


def test_alias_index_also_makes_canonical_name_an_alias():
    ents = {"Q1": EntityRecord("Q1", "Fenerbahçe", "football_club_tr", 90, 40)}
    aliases = build_alias_index(ents, set(), BuildReport())
    assert aliases["fenerbahce"] == "Fenerbahçe"


def test_collision_resolved_by_higher_priority():
    club = EntityRecord("Q1", "Fenerbahçe", "football_club_tr", 90, 40, {"Fenerbahçe"})
    hood = EntityRecord("Q2", "Fenerbahçe, Kadıköy", "district_tr", 52, 38, {"Fenerbahçe"})
    report = BuildReport()
    aliases = build_alias_index({"Q1": club, "Q2": hood}, set(), report)
    assert aliases["fenerbahce"] == "Fenerbahçe"
    assert report.dropped_ambiguous == []


def test_equal_priority_resolved_when_popularity_gap_is_large():
    a = EntityRecord("Q1", "Ankara", "city_world", 60, 300, {"Ankara"})
    b = EntityRecord("Q2", "Ankara (film)", "city_world", 60, 5, {"Ankara"})
    report = BuildReport()
    aliases = build_alias_index({"Q1": a, "Q2": b}, set(), report)
    assert aliases["ankara"] == "Ankara"
    assert report.dropped_ambiguous == []


def test_equal_priority_alias_dropped_when_popularity_is_close():
    a = EntityRecord("Q1", "Alfa", "city_world", 60, 30, {"Ortak"})
    b = EntityRecord("Q2", "Beta", "city_world", 60, 25, {"Ortak"})
    report = BuildReport()
    aliases = build_alias_index({"Q1": a, "Q2": b}, set(), report)
    assert "ortak" not in aliases
    assert len(report.dropped_ambiguous) == 1
    assert report.dropped_ambiguous[0]["alias"] == "ortak"


def test_at_ambiguity_ratio_boundary_top_candidate_wins():
    runner = 10
    top = int(AMBIGUITY_RATIO * runner)
    a = EntityRecord("Q1", "Alfa", "city_world", 60, top, {"Ortak"})
    b = EntityRecord("Q2", "Beta", "city_world", 60, runner, {"Ortak"})
    aliases = build_alias_index({"Q1": a, "Q2": b}, set(), BuildReport())
    assert aliases["ortak"] == "Alfa"


def test_collision_pointing_to_same_canonical_name_is_not_a_collision():
    a = EntityRecord("Q1", "Fenerbahçe", "football_club_tr", 90, 40, {"Fener"})
    b = EntityRecord("Q2", "Fenerbahçe", "football_club_world", 78, 39, {"Fener"})
    report = BuildReport()
    aliases = build_alias_index({"Q1": a, "Q2": b}, set(), report)
    assert aliases["fener"] == "Fenerbahçe"
    assert report.dropped_ambiguous == []


def test_blocklist_drops_alias_and_reports():
    rec = EntityRecord("Q1", "Spor Toto", "tech_company", 85, 10, {"Spor"})
    report = BuildReport()
    aliases = build_alias_index({"Q1": rec}, {"spor"}, report)
    assert "spor" not in aliases
    assert "spor" in report.dropped_blocklist
    # The canonical name itself is not in the blocklist, it must stay.
    assert aliases["spor toto"] == "Spor Toto"


# ── overrides ────────────────────────────────────────────────


def test_override_overrides_wikidata_mapping_and_reports():
    rec = EntityRecord("Q1", "Fenerbahçe SK", "football_club_tr", 90, 40, {"Fener"})
    report = BuildReport()
    ents = {"Q1": rec}
    aliases = build_alias_index(ents, set(), report)
    assert aliases["fener"] == "Fenerbahçe SK"

    apply_overrides(aliases, ents, {"Fener": "Fenerbahçe"}, report)
    assert aliases["fener"] == "Fenerbahçe"
    assert report.override_conflicts == [
        {"alias": "fener", "was": "Fenerbahçe SK", "now": "Fenerbahçe"}
    ]


def test_override_can_point_to_canonical_name_not_in_wikidata():
    # "Sari Lacivert" is not in Wikidata; local jargon is a valid target.
    report = BuildReport()
    aliases: dict[str, str] = {}
    apply_overrides(aliases, {}, {"Sarı Lacivert": "Fenerbahçe"}, report)
    assert aliases["sari lacivert"] == "Fenerbahçe"
    assert report.override_count == 1


def test_invalid_override_is_skipped():
    report = BuildReport()
    aliases: dict[str, str] = {}
    apply_overrides(aliases, {}, {"F": "Fenerbahçe", "fener": "   "}, report)
    assert aliases == {}
    assert report.override_count == 0


# ── document ─────────────────────────────────────────────────


def test_to_document_keeps_popular_qid_for_same_canonical_name():
    a = EntityRecord("Q1", "Fenerbahçe", "football_club_tr", 90, 40)
    b = EntityRecord("Q2", "Fenerbahçe", "football_club_world", 78, 5)
    doc = to_document({"fb": "Fenerbahçe"}, {"Q1": a, "Q2": b}, BuildReport(), "2026-08-11T00:00:00+00:00")
    assert doc["entities"]["Fenerbahçe"]["qid"] == "Q1"
    assert doc["meta"]["alias_count"] == 1
    assert doc["meta"]["entity_count"] == 1


def test_run_end_to_end():
    client = FakeClient(
        {
            "SELECTOR_CLUB": [
                binding("Q1", tr="Fenerbahçe", alias="Fener", sitelinks=40),
                binding("Q1", tr="Fenerbahçe", alias="Spor", sitelinks=40),
            ]
        }
    )
    doc, report = run(
        client,
        overrides={"Sarı Lacivert": "Fenerbahçe"},
        blocklist={"spor"},
        categories=[cat("club", "SELECTOR_CLUB", priority=90)],
    )
    assert doc["aliases"]["fenerbahce"] == "Fenerbahçe"
    assert doc["aliases"]["fener"] == "Fenerbahçe"
    assert doc["aliases"]["sari lacivert"] == "Fenerbahçe"
    assert "spor" not in doc["aliases"]
    assert doc["entities"]["Fenerbahçe"]["qid"] == "Q1"
    assert report.per_category_entities["club"] == 1


# ── I5: collision detection looks at the QID ─────────────────


def test_two_qids_differing_by_qualifier_count_as_collision():
    # The shape the real pipeline produces: two SEPARATE QIDs whose labels differ only by the
    # trailing qualifier. The old code cleaned both labels to "Ankara", so it saw len(distinct)==1
    # and NEVER ran the collision policy.
    report = BuildReport()
    client = FakeClient(
        {
            "SELECTOR_A": [
                binding("Q1", tr="Ankara", sitelinks=30),
                binding("Q2", tr="Ankara (film)", sitelinks=25),
            ]
        }
    )
    ents = fetch_category(client, cat("c1", "SELECTOR_A"), report)
    aliases = build_alias_index(ents, set(), report)

    assert "ankara" not in aliases
    assert [d["alias"] for d in report.dropped_ambiguous] == ["ankara"]
    # The full label is the canonical identity; the qualifier is stripped ONLY in the search key
    # ("Ankara (film)" → "ankara"), because nobody writes the qualifier in text. So the two entities
    # compete for the same key and the key is dropped — the correct behavior. Previously the two
    # collapsed into one name and silently merged.
    assert ents["Q2"].canonical == "Ankara (film)"
    assert list(aliases) == []


def test_qualified_second_qid_resolved_by_popularity_gap():
    # Same shape, but the sitelink gap exceeds AMBIGUITY_RATIO: the city must win and there must be
    # no dropped record.
    report = BuildReport()
    client = FakeClient(
        {
            "SELECTOR_A": [
                binding("Q1", tr="Ankara", sitelinks=300),
                binding("Q2", tr="Ankara (film)", sitelinks=5),
            ]
        }
    )
    ents = fetch_category(client, cat("c1", "SELECTOR_A"), report)
    aliases = build_alias_index(ents, set(), report)
    assert aliases["ankara"] == "Ankara"
    assert report.dropped_ambiguous == []


def test_two_qids_with_identical_full_label_are_not_a_collision():
    # Wikidata labels are NOT unique. If two different QIDs have exactly the same full label, the
    # alias → single canonical contract is not broken.
    a = EntityRecord("Q1", "Fenerbahçe", "football_club_tr", 60, 10, {"Fener"})
    b = EntityRecord("Q2", "Fenerbahçe", "football_club_world", 60, 10, {"Fener"})
    report = BuildReport()
    aliases = build_alias_index({"Q1": a, "Q2": b}, set(), report)
    assert aliases["fener"] == "Fenerbahçe"
    assert report.dropped_ambiguous == []


def test_equal_priority_and_sitelinks_dropped_not_chosen_randomly():
    # On ambiguity, "pick one" produces wrong personalization; the rule is to DROP. This test pins
    # that rule (and its determinism).
    a = EntityRecord("Q1", "Alfa", "city_world", 60, 42, {"Ortak"})
    b = EntityRecord("Q2", "Beta", "city_world", 60, 42, {"Ortak"})
    for _ in range(5):
        report = BuildReport()
        aliases = build_alias_index({"Q1": a, "Q2": b}, set(), report)
        assert "ortak" not in aliases
        assert len(report.dropped_ambiguous) == 1


# ── I6: paging + ORDER BY ────────────────────────────────────


class PagingClient:
    """Fake client that actually applies OFFSET/LIMIT."""

    def __init__(self, rows: list[dict]):
        self.rows = rows
        self.queries: list[str] = []

    def select(self, query: str) -> list[dict]:
        self.queries.append(query)
        limit = int(re.search(r"LIMIT (\d+)", query).group(1))
        offset = int(re.search(r"OFFSET (\d+)", query).group(1))
        return self.rows[offset : offset + limit]


def test_query_carries_order_by():
    # Without ORDER BY, LIMIT/OFFSET is unstable: which entities survive changes from run to run.
    q = build_query(cat("c1", "SELECTOR_A"))
    assert "ORDER BY DESC(?sitelinks) ?item" in q


def test_fetch_bindings_paginates(monkeypatch):
    monkeypatch.setattr("canonical_dict.build.PAGE_SIZE", 2)
    rows = [binding(f"Q{i}", tr=f"E{i}") for i in range(5)]
    client = PagingClient(rows)
    got, limit_hit = fetch_bindings(client, cat("c1", "SELECTOR_A"))
    assert len(got) == 5
    assert limit_hit is False
    # 2 + 2 + 1 (short page) = 3 requests
    assert len(client.queries) == 3


def test_fetch_bindings_hitting_ceiling_is_reported(monkeypatch):
    monkeypatch.setattr("canonical_dict.build.PAGE_SIZE", 2)
    rows = [binding(f"Q{i}", tr=f"E{i}") for i in range(10)]
    small = Category(name="c1", selector="SELECTOR_A", min_sitelinks=0, priority=50, limit=4)
    got, limit_hit = fetch_bindings(PagingClient(rows), small)
    assert len(got) == 4
    assert limit_hit is True

    report = BuildReport()
    fetch_category(PagingClient(rows), small, report)
    assert "c1" in report.limit_hit_categories


# ── I7: overrides are subject to the blocklist ───────────────


def test_override_in_blocklist_is_rejected():
    # The substring-grounding decision relied on the rationale "the blocklist + MIN_ALIAS_LEN reduce
    # the risk"; if overrides were exempt, that rationale would be left hanging.
    report = BuildReport()
    aliases: dict[str, str] = {}
    apply_overrides(aliases, {}, {"aslan": "Galatasaray"}, report, {"aslan"})
    assert aliases == {}
    assert report.rejected_overrides == ["aslan"]
    assert report.override_count == 0


def test_override_in_blocklist_passes_with_force():
    report = BuildReport()
    aliases: dict[str, str] = {}
    apply_overrides(
        aliases,
        {},
        {"aslan": {"value": "Galatasaray", "force": True}},
        report,
        {"aslan"},
    )
    assert aliases["aslan"] == "Galatasaray"
    assert report.rejected_overrides == []
    assert report.override_count == 1


def test_plain_string_override_format_keeps_working():
    report = BuildReport()
    aliases: dict[str, str] = {}
    apply_overrides(aliases, {}, {"cimbom": "Galatasaray"}, report, set())
    assert aliases["cimbom"] == "Galatasaray"


def test_overrides_json_does_not_conflict_with_blocklist():
    # Keep the two files in the repo from drifting apart: every override key must be outside the
    # blocklist (or carry force).
    here = Path(__file__).resolve().parent.parent
    overrides = load_overrides(here / "overrides.json")
    blocklist = load_blocklist(here / "blocklist.txt")
    for raw, value in overrides.items():
        key = normalize_alias(raw, min_len=MIN_OVERRIDE_ALIAS_LEN)
        forced = isinstance(value, dict) and value.get("force")
        assert key is not None, raw
        assert key not in blocklist or forced, f"{key} is on the blocklist and has no force"


# ── C1: alias count before overrides ─────────────────────────


def test_pre_override_alias_count_excludes_overrides():
    # apply_overrides ran unconditionally and added ~35 entries, so the total alias_count was NEVER
    # 0 and the "dictionary empty" gate could never fire.
    client = FakeClient({})  # all categories 0 rows → the WDQS disaster
    doc, report = run(
        client,
        overrides={"Sarı Lacivert": "Fenerbahçe"},
        blocklist=set(),
        categories=[cat("club", "SELECTOR_YOK", priority=90)],
    )
    assert report.pre_override_alias_count == 0
    assert doc["meta"]["pre_override_alias_count"] == 0
    # The total count is NOT 0 — this is exactly why the old gate could not fire.
    assert doc["meta"]["alias_count"] == 1


def test_to_document_omits_pre_override_field_when_unknown():
    doc = to_document({"fb": "F"}, {}, BuildReport(), "2026-08-11T00:00:00+00:00")
    assert "pre_override_alias_count" not in doc["meta"]


# ── determinism ──────────────────────────────────────────────


def test_same_input_produces_same_document():
    # Both maps are sorted in to_document; otherwise the "did the dictionary change" question goes
    # unanswered and every run writes a new generation to Redis.
    rows = {
        "SELECTOR_CLUB": [
            binding("Q1", tr="Fenerbahçe", alias="Fener", sitelinks=40),
            binding("Q2", tr="Galatasaray", alias="Cimbom", sitelinks=38),
            binding("Q3", tr="Beşiktaş", alias="BJK", sitelinks=36),
        ]
    }
    cats = [cat("club", "SELECTOR_CLUB", priority=90)]
    docs = []
    for _ in range(3):
        doc, _ = run(FakeClient(rows), overrides={}, blocklist=set(), categories=cats)
        doc["meta"].pop("generated_at")
        docs.append(json.dumps(doc, ensure_ascii=False, sort_keys=False))
    assert docs[0] == docs[1] == docs[2]
    assert list(json.loads(docs[0])["aliases"]) == sorted(json.loads(docs[0])["aliases"])


def test_candidate_with_own_name_wins():
    # Regression: every place in Anatolia also has an ancient name and, since the two fall into the
    # same category, the sitelink ratio (61/44=1.39) could not pass the threshold and Istanbul's
    # most frequently mentioned district was dropped from the dictionary.
    modern = EntityRecord("Q932886", "Kadıköy", "city_tr", 66, 61, {"Kalkedon"})
    ancient = EntityRecord("Q337381", "Kalkedon", "city_tr", 66, 44, {"Kadıköy"})
    report = BuildReport()
    aliases = build_alias_index({"Q1": modern, "Q2": ancient}, set(), report)
    assert aliases["kadikoy"] == "Kadıköy"
    assert aliases["kalkedon"] == "Kalkedon"
    assert report.dropped_ambiguous == []


def test_own_name_in_different_categories_is_chosen_by_PRIORITY():
    # "Ankara" and "Ankara (film)" — since the qualifier suffix is stripped in normalize_alias, BOTH
    # fold to "ankara"; since they are in separate categories, PRIORITY decides.
    city = EntityRecord("Q1", "Ankara", "city_world", 60, 300)
    film = EntityRecord("Q2", "Ankara (film)", "film", 50, 280)
    aliases = build_alias_index({"Q1": city, "Q2": film}, set(), BuildReport())
    assert aliases["ankara"] == "Ankara"


def test_own_name_rule_does_not_apply_in_SAME_category():
    # Two self-named candidates in the same category are REAL rivals; staying silent is correct
    # there, not choosing by popularity.
    a = EntityRecord("Q1", "Ereğli", "district_tr", 52, 30)
    b = EntityRecord("Q2", "Ereğli (Konya)", "district_tr", 52, 25)
    report = BuildReport()
    aliases = build_alias_index({"Q1": a, "Q2": b}, set(), report)
    assert "eregli" not in aliases
    assert [d["alias"] for d in report.dropped_ambiguous] == ["eregli"]


def test_province_and_district_records_do_NOT_cancel_each_other():
    # Example: "Van" (province) + "Van (ilçe)" as two self-named records cancelled each
    # other, leaving the decision to the country category, and Vanuatu ("VAN" ISO code) won.
    country = EntityRecord("Q686", "Vanuatu", "country", 70, 250, {"VAN"})
    province = EntityRecord("Q80089", "Van", "city_tr", 66, 60)
    district = EntityRecord("Q123", "Van (ilçe)", "district_tr", 52, 3)
    report = BuildReport()
    aliases = build_alias_index({"Q1": country, "Q2": province, "Q3": district}, set(), report)
    assert aliases["van"] == "Van"
    assert report.dropped_ambiguous == []


def test_own_name_choice_is_PRIORITY_NOT_POPULARITY():
    # Pure popularity gave `demokrat parti` → Demokrat Parti (Thailand).
    tr = EntityRecord("Q2067217", "Demokrat Parti", "political_party_tr", 95, 17)
    th = EntityRecord("Q212", "Demokrat Parti (Tayland)", "political_party_world", 72, 45)
    aliases = build_alias_index({"Q1": tr, "Q2": th}, set(), BuildReport())
    assert aliases["demokrat parti"] == "Demokrat Parti"


def test_own_name_rule_overrides_priority_too():
    # Example: "Türkiye Gazetesi" (newspaper_tr, priority 86, 11 sitelinks) overrode the
    # country "Türkiye" (country, priority 70, 424 sitelinks).
    # Category priority is a TYPE preference, it must not override which entity bears that NAME.
    gazete = EntityRecord("Q1", "Türkiye Gazetesi", "newspaper_tr", 86, 11, {"Türkiye"})
    country = EntityRecord("Q2", "Türkiye", "country", 70, 424)
    report = BuildReport()
    aliases = build_alias_index({"Q1": gazete, "Q2": country}, set(), report)
    assert aliases["turkiye"] == "Türkiye"
    assert aliases["turkiye gazetesi"] == "Türkiye Gazetesi"
    assert report.dropped_ambiguous == []


def test_surname_alias_is_produced():
    # Example: "Donald J. Trump" was in the dictionary but the `trump` key was not, so
    # "Trump" never matched.
    client = FakeClient({"SELECTOR_A": [binding("Q1", tr="Donald J. Trump", sitelinks=250)]})
    c = Category(name="pw", selector="SELECTOR_A", min_sitelinks=0, priority=58, surname_alias=True)
    rec = fetch_category(client, c, BuildReport())
    assert "Trump" in rec["Q1"].raw_aliases
    aliases = build_alias_index(rec, set(), BuildReport())
    assert aliases["trump"] == "Donald J. Trump"


def test_surname_alias_is_NOT_produced_in_TURKISH_categories():
    # Turkish surnames are everyday words; producing `yavas` from "Mansur Yavas" would attach a
    # mayor to every post containing "yavas".
    client = FakeClient({"SELECTOR_A": [binding("Q1", tr="Mansur Yavaş", sitelinks=10)]})
    c = Category(name="pt", selector="SELECTOR_A", min_sitelinks=0, priority=62)
    rec = fetch_category(client, c, BuildReport())
    assert "Yavaş" not in rec["Q1"].raw_aliases


def test_surname_alias_dropped_when_ambiguous():
    # Two people bearing the same surname → the ambiguity policy kicks in.
    client = FakeClient(
        {
            "SELECTOR_A": [
                binding("Q1", tr="Ahmet Bayraktar", sitelinks=30),
                binding("Q2", tr="Mehmet Bayraktar", sitelinks=25),
            ]
        }
    )
    c = Category(name="pw", selector="SELECTOR_A", min_sitelinks=0, priority=58, surname_alias=True)
    report = BuildReport()
    rec = fetch_category(client, c, report)
    aliases = build_alias_index(rec, set(), report)
    assert "bayraktar" not in aliases


def test_surname_of_skips_suffixes():
    from canonical_dict.build import _surname_of

    assert _surname_of("Al Gore Jr.") == "Gore"
    assert _surname_of("Ronaldo") is None       # single word
    assert _surname_of("Jack Ma") is None       # too-short surname
    assert _surname_of("Ankara (film)") is None # single word once the qualifier is stripped


def test_surname_is_NOT_produced_from_regnal_name():
    from canonical_dict.build import _surname_of

    # The last word is a GIVEN NAME, not a surname; producing it would attach the King of Morocco to
    # every post containing "Muhammed" (a very frequent given name).
    assert _surname_of("VI. Muhammed") is None
    assert _surname_of("II. Abdülhamid") is None
    assert _surname_of("I. Selim") is None
    # Normal names are not affected
    assert _surname_of("Donald J. Trump") == "Trump"


# ── bare district names (1b.5) ───────────────────────────────


def _district(qid: str, canonical: str, category: str = "district_tr") -> EntityRecord:
    return EntityRecord(qid, canonical, category, 52, 5)


def test_district_bare_name_added_only_in_allowlist():
    ents = {
        "Q1": _district("Q1", "Bafra ilçesi"),
        "Q2": _district("Q2", "Alaca İlçesi"),  # common noun: not in the list
        "Q3": _district("Q3", "Diyadin'in ilçesi"),
        "Q4": _district("Q4", "Ayvacık İlçesi (Samsun)"),
        "Q5": _district("Q5", "Merzifon ilçesi", "city_tr"),  # not a district
    }
    unused = add_district_bare_aliases(ents, {"bafra", "diyadin", "ayvacik", "merzifon", "yok"})
    report = BuildReport()
    aliases = build_alias_index(ents, set(), report)
    assert aliases["bafra"] == "Bafra ilçesi"
    assert aliases["diyadin"] == "Diyadin'in ilçesi"
    assert aliases["ayvacik"] == "Ayvacık İlçesi (Samsun)"
    assert "alaca" not in aliases
    assert "merzifon" not in aliases
    # A suffixed full name also stays in place.
    assert aliases["bafra ilcesi"] == "Bafra ilçesi"
    assert unused == ["merzifon", "yok"]


def test_district_bare_name_cannot_bypass_blocklist_and_ambiguity():
    ents = {"Q1": _district("Q1", "Bafra ilçesi")}
    add_district_bare_aliases(ents, {"bafra"})
    report = BuildReport()
    aliases = build_alias_index(ents, {"bafra"}, report)
    assert "bafra" not in aliases
    assert "bafra" in report.dropped_blocklist


def test_district_bare_allow_blocklist_ile_cakismiyor():
    here = Path(__file__).resolve().parent.parent
    allow = load_blocklist(here / "district_bare_allow.txt")
    block = load_blocklist(here / "blocklist.txt")
    assert allow, "allowlist bos okunuyor"
    assert not (allow & block), sorted(allow & block)


# ── 1b.8: report of different QIDs with the same label ───────


def test_different_qids_with_same_label_are_reported():
    ents = {
        "Q41053": EntityRecord("Q41053", "Hizbullah", "political_party_world", 72, 128, {"Hezbollah"}),
        "Q1621312": EntityRecord("Q1621312", "Hizbullah", "political_party_world", 72, 20, {"Türk Hizbullahı"}),
        "Q1": EntityRecord("Q1", "Ankara", "city_tr", 66, 200),
    }
    report = BuildReport()
    aliases = build_alias_index(ents, set(), report)
    # The behavior does not change: the name goes to a single canonical.
    assert aliases["hizbullah"] == "Hizbullah"
    m = report.same_label_merges["Hizbullah"]
    assert m["qids"] == {"Q41053", "Q1621312"}
    assert m["categories"] == {"political_party_world"}
    # A canonical with a single QID does not enter the report.
    assert "Ankara" not in report.same_label_merges


# ── 1b.9: same-named districts ───────────────────────────────


def test_same_named_districts_collapse_to_neutral_canonical():
    from canonical_dict.build import collapse_district_homonyms

    ents = {
        "Q1": EntityRecord("Q1", "Ayvacık (Çanakkale ilçesi)", "district_tr", 52, 20),
        "Q2": EntityRecord("Q2", "Ayvacık İlçesi (Samsun)", "district_tr", 52, 8),
        "Q3": EntityRecord("Q3", "Merkez (Antalya)", "district_tr", 52, 5),
        "Q4": EntityRecord("Q4", "Merkez İlçesi (Samsun)", "district_tr", 52, 5),
        "Q5": EntityRecord("Q5", "Bafra ilçesi", "district_tr", 52, 9),
    }
    aliases = {
        "ayvacik": "Ayvacık (Çanakkale ilçesi)",
        "ayvacik ilcesi": "Ayvacık İlçesi (Samsun)",
        "merkez ilcesi": "Merkez İlçesi (Samsun)",
        "bafra ilcesi": "Bafra ilçesi",
        "ayvacik holding": "Ayvacık Holding",  # NOT a district, left untouched
    }
    report = BuildReport()
    moved = collapse_district_homonyms(aliases, ents, report)
    assert aliases["ayvacik"] == aliases["ayvacik ilcesi"] == "Ayvacık"
    assert aliases["merkez ilcesi"] == "Merkez İlçesi (Samsun)"  # Merkez is not merged
    assert aliases["bafra ilcesi"] == "Bafra ilçesi"  # single record, left untouched
    assert aliases["ayvacik holding"] == "Ayvacık Holding"
    assert moved == 2
    assert report.district_homonym_groups == {
        "Ayvacık": ["Ayvacık (Çanakkale ilçesi)", "Ayvacık İlçesi (Samsun)"]
    }


def test_district_base_strips_affixes():
    from canonical_dict.build import _district_base

    assert _district_base("Edremit (Balıkesir) ilçesi") == "Edremit"
    assert _district_base("Kemer district (Antalya)") == "Kemer"
    assert _district_base("Diyadin'in ilçesi") == "Diyadin"
    assert _district_base("Ağlı (ilçe)") == "Ağlı"
