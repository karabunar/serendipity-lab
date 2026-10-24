"""Behavioral validation of the dictionary output — no network or Redis access.

The build gates (empty dictionary, shrinkage, empty/truncated category) check
that the dictionary EXISTS; this module checks its CORRECTNESS: do known
aliases go to the right canonical name, is everything that must be excluded
by decision actually excluded, are the new categories of the expected size,
do overrides diverge from the Wikidata canonical name and split the same
entity into two interest keys, and what disappeared relative to the old
dictionary.

The expectations live in `testdata/validation.json`. During 1b.6 each of
these checks was done by hand and each caught a defect — mistaking Q7210356
for a "terror organization" and dropping UN/NATO/EU, TSK vanishing due to the
armed-organization exclusion, 12 overrides diverging from the WD canonical
name.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from pathlib import Path

GOLDEN_PATH = Path(__file__).resolve().parent / "testdata" / "validation.json"


@dataclass
class Result:
    errors: list[str] = field(default_factory=list)
    warnings: list[str] = field(default_factory=list)
    infos: list[str] = field(default_factory=list)

    @property
    def passed(self) -> bool:
        return not self.errors


def _values(d: dict) -> list:
    """Skips explanation keys starting with `_` and flattens the lists."""
    out: list = []
    for k, v in d.items():
        if k.startswith("_"):
            continue
        out.extend(v if isinstance(v, list) else [k])
    return out


def validate(doc: dict, golden: dict, report: dict | None = None, previous: dict | None = None) -> Result:
    result = Result()
    aliases: dict[str, str] = doc["aliases"]
    entities: dict[str, dict] = doc["entities"]

    for key, want in golden.get("alias", {}).items():
        got = aliases.get(key)
        if got != want:
            result.errors.append(f"alias {key!r}: expected {want!r}, got {got!r}")

    for canon, want in golden.get("category", {}).items():
        got = entities.get(canon, {}).get("category")
        if got != want:
            result.errors.append(f"category {canon!r}: expected {want}, got {got}")

    for key in _values(golden.get("absent_alias", {})):
        if key in aliases:
            result.errors.append(f"alias {key!r} must NOT exist, it maps to {aliases[key]!r}")

    new_cats = set(golden.get("new_categories", []))
    forbidden = {q: name for q, name in golden.get("forbidden_qid_in_new_category", {}).items() if not q.startswith("_")}
    for canon, e in entities.items():
        if e.get("category") in new_cats and e.get("qid") in forbidden:
            result.errors.append(f"{canon} ({e['qid']}) must NOT be in new category ({e['category']})")

    kept = {q: name for q, name in golden.get("kept_qid", {}).items() if not q.startswith("_")}
    for canon, e in entities.items():
        if e.get("qid") in kept:
            result.infos.append(f"intentionally kept: {canon} ({e['qid']}, {e.get('category')})")

    warn = {q: name for q, name in golden.get("warn_qid", {}).items() if not q.startswith("_")}
    for canon, e in entities.items():
        if e.get("qid") in warn:
            result.warnings.append(f"{canon} ({e['qid']}, {e.get('category')}) in dictionary — awaiting decision")

    if report is not None:
        for c in report.get("empty_categories", []):
            result.errors.append(f"category {c} returned 0 rows")
        for c in report.get("limit_hit_categories", []):
            result.errors.append(f"category {c} hit the row limit (truncated)")
        per = report.get("per_category_entities", {})
        for cat, mn in golden.get("report_min_entities", {}).items():
            n = per.get(cat, 0)
            if n < mn:
                result.errors.append(f"category {cat}: {n} entities < expected minimum {mn}")
            else:
                result.infos.append(f"category {cat}: {n} entities")
        # If an override pulls an alias from an entity in a NEW category to a
        # different name, the same institution becomes two keys in the
        # interest profile.
        allowed = {k for k in golden.get("allowed_override_divergence", {}) if not k.startswith("_")}
        for c in report.get("override_conflicts", []):
            if c.get("alias") in allowed:
                result.infos.append(f"intentional override divergence: {c['alias']} → {c['now']} (WD: {c['was']})")
                continue
            if entities.get(c.get("was"), {}).get("category") in new_cats:
                result.errors.append(
                    f"override {c['alias']!r} → {c['now']!r} but Wikidata canonical is {c['was']!r} — "
                    "align the value in overrides.json to the WD name"
                )
    else:
        result.warnings.append("no report given (--report): category counts and override divergence not checked")

    if previous is not None:
        old = previous["aliases"]
        lost = sorted(k for k in old if k not in aliases)
        changed = sorted(k for k in old if k in aliases and old[k] != aliases[k])
        result.infos.append(
            f"vs previous dictionary: +{sum(k not in old for k in aliases)} added, "
            f"-{len(lost)} lost, {len(changed)} aliases with changed target"
        )
        for k in lost[:40]:
            result.infos.append(f"  lost: {k} (→ {old[k]})")
        for k in changed[:40]:
            result.infos.append(f"  changed: {k}: {old[k]} → {aliases[k]}")
        ratio = len(lost) / max(1, len(old))
        if ratio > golden.get("lost_alias_ratio_max", 0.01):
            result.errors.append(f"lost alias ratio %{100 * ratio:.1f} — threshold %{100 * golden['lost_alias_ratio_max']:.1f}")
    return result


def load(path: Path) -> dict:
    return json.loads(Path(path).read_text(encoding="utf-8"))
