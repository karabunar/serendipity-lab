"""Tests for the dictionary validator — fake document, no network."""

from __future__ import annotations

from canonical_dict.validate import GOLDEN_PATH, validate, load

GOLDEN = {
    "alias": {"nato": "NATO"},
    "category": {"NATO": "intl_org"},
    "absent_alias": {"_reason": "x", "organization": ["pkk"]},
    "forbidden_qid_in_new_category": {"_reason": "x", "Q1": "Bad"},
    "warn_qid": {"_reason": "x", "Q2": "Tartışmalı"},
    "new_categories": ["intl_org"],
    "report_min_entities": {"intl_org": 1},
    "lost_alias_ratio_max": 0.5,
}


def _doc(aliases, entities):
    return {"aliases": aliases, "entities": entities}


def _report(**kw):
    rep = {"empty_categories": [], "limit_hit_categories": [], "per_category_entities": {"intl_org": 1}, "override_conflicts": []}
    rep.update(kw)
    return rep


def _good():
    return _doc({"nato": "NATO"}, {"NATO": {"qid": "Q7184", "category": "intl_org"}})


def test_clean_dictionary_passes():
    result = validate(_good(), GOLDEN, _report())
    assert result.passed, result.errors


def test_wrong_target_and_category_are_caught():
    doc = _doc({"nato": "Nato Kulübü"}, {"NATO": {"qid": "Q7184", "category": "company_tr"}})
    result = validate(doc, GOLDEN, _report())
    assert any("'nato'" in e for e in result.errors)
    assert any("category 'NATO'" in e for e in result.errors)


def test_forbidden_alias_and_forbidden_qid_in_new_category():
    doc = _good()
    doc["aliases"]["pkk"] = "PKK"
    doc["entities"]["Bad"] = {"qid": "Q1", "category": "intl_org"}
    result = validate(doc, GOLDEN, _report())
    assert any("'pkk' must NOT exist" in e for e in result.errors)
    assert any("Q1" in e for e in result.errors)


def test_forbidden_qid_in_old_category_is_not_an_error():
    # The forbidden list binds only NEW categories; a record in an old category
    # is unrelated to the query exclusion of 1b.6.
    doc = _good()
    doc["entities"]["Bad"] = {"qid": "Q1", "category": "political_party_world"}
    assert validate(doc, GOLDEN, _report()).passed


def test_warning_qid_is_a_warning_not_an_error():
    doc = _good()
    doc["entities"]["Tartışmalı"] = {"qid": "Q2", "category": "political_party_world"}
    result = validate(doc, GOLDEN, _report())
    assert result.passed
    assert any("Q2" in w for w in result.warnings)


def test_report_empty_truncated_and_too_few_entities():
    result = validate(_good(), GOLDEN, _report(empty_categories=["x"], limit_hit_categories=["y"], per_category_entities={"intl_org": 0}))
    assert len(result.errors) == 3


def test_error_when_override_diverges_from_wd_canonical():
    # In 1b.6, 12 overrides were in exactly this situation: the same institution was split into two interest keys.
    rep = _report(override_conflicts=[{"alias": "nato", "was": "NATO", "now": "Nato"}])
    result = validate(_good(), GOLDEN, rep)
    assert any("align the value" in e for e in result.errors)


def test_lost_ratio_relative_to_previous_dictionary():
    previous = _doc({"nato": "NATO", "a": "A", "i": "B"}, {})
    result = validate(_good(), GOLDEN, _report(), previous)
    assert any("lost alias ratio" in e for e in result.errors)  # 2/3 > 0.5


def test_repo_golden_is_readable_and_self_consistent():
    golden = load(GOLDEN_PATH)
    absent = {a for k, v in golden["absent_alias"].items() if not k.startswith("_") for a in v}
    assert not (set(golden["alias"]) & absent), "the same alias is both expected and forbidden"
    assert set(golden["new_categories"]) <= set(golden["report_min_entities"])


def test_allowed_override_divergence_passes_other_errors():
    golden = dict(GOLDEN, allowed_override_divergence={"_reason": "x", "nato": "bilinçli"})
    rep = _report(override_conflicts=[
        {"alias": "nato", "was": "NATO", "now": "Nato"},
        {"alias": "otan", "was": "NATO", "now": "Otan"},
    ])
    result = validate(_good(), golden, rep)
    assert len(result.errors) == 1 and "'otan'" in result.errors[0]


def test_kept_qid_is_printed_as_info_not_warning():
    golden = dict(GOLDEN, kept_qid={"_reason": "x", "Q3": "Bilinçli"})
    doc = _good()
    doc["entities"]["Bilinçli"] = {"qid": "Q3", "category": "political_party_world"}
    result = validate(doc, golden, _report())
    assert result.passed and not result.warnings
    assert any("Q3" in i for i in result.infos)
