"""Building the canonical dictionary: fetch → cleanup → collision resolution → output.

The dictionary consists of two maps:

    aliases:  folded alias  → canonical name     ("fb" → "Fenerbahçe")
    entities: canonical name → {qid, category, sitelinks}

The canonical name itself is also an alias (maps to itself); otherwise an
entity appearing in text under its full name could not be found in the
dictionary.

**Collision policy.** The same alias can point to several entities:
"Fenerbahçe" is both the club and the neighborhood in Kadıköy, "Ankara" is
both the city and a film. Silently picking one leads to wrong
personalization, and keeping both breaks the contract (alias → a single
canonical). The rule:

1. If EXACTLY ONE of the candidates has this key as its canonical name, it
   wins (the "own name" rule). Being an entity's NAME is stronger than merely
   carrying that name as an alternative — even stronger than category priority.
2. Candidates are sorted by (priority, sitelinks) descending; the top
   candidate wins if its priority is GREATER than the second's.
3. If priority is tied, the top one wins if it has at least
   `AMBIGUITY_RATIO` times more sitelinks.
4. Otherwise the alias is **DROPPED** and written to the collision report.

Manual maintenance (`overrides.json`) always wins: Wikidata has no "Sarı
Lacivert" or "Altılı Masa"; social media jargon does not come from there.
"""

from __future__ import annotations

import json
import logging
import re
from collections import defaultdict
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path

from .fold import fold
from .queries import CATEGORIES, PAGE_SIZE, Category, build_query
from .sparql import SPARQLClient, qid_of

log = logging.getLogger(__name__)

# Sitelink advantage a candidate needs in order to win at the same priority.
AMBIGUITY_RATIO = 3.0

# Minimum length of an alias DERIVED from Wikidata.
#
# A measurement over a real post sample showed that a threshold of 2 is
# expensive: 2-letter surfaces produced many matches that an independent NER
# did not support. The entire top was ISO 3166 country
# codes — `na`→Namibia, `tl`→East Timor, `ni`→Nicaragua,
# `kg`→Kyrgyzstan, `gt`→Guatemala. These were poisoning profiles.
#
# Measuring the other side came up empty: the genuine abbreviations the
# threshold protects were effectively dead in the same corpus — `fb`, `gs`
# and `ak` had almost no matches. The valuable abbreviations are 3 letters: `chp` 1,424,
# `mhp` 276, `akp` 29, `bjk` 12.
MIN_ALIAS_LEN = 3

# Minimum length of a curated (`overrides.json`) alias.
#
# Overrides may fall BELOW the automatic threshold: a hand-touched
# abbreviation ("fb" → Fenerbahçe) is not subject to the same gate as derived
# noise. 1 character is still noise.
#
# ⚠️ `canonicaldict.minAliasRunes` on the Go side matches this value (2), NOT
# MIN_ALIAS_LEN — since the dictionary can still carry curated 2-letter keys,
# the scan floor must stay at 2.
MIN_OVERRIDE_ALIAS_LEN = 2

# Trailing parenthetical qualifier: "Fenerbahçe (spor kulübü)" → "Fenerbahçe".
_PAREN_SUFFIX = re.compile(r"\s*\([^)]*\)\s*$")
_WS = re.compile(r"\s+")
# An alias that contains no letter at all (pure number, punctuation) is not accepted.
_HAS_LETTER = re.compile(r"[^\W\d_]", re.UNICODE)


@dataclass
class EntityRecord:
    qid: str
    canonical: str
    category: str
    priority: int
    sitelinks: int
    raw_aliases: set[str] = field(default_factory=set)


@dataclass
class BuildReport:
    """Summary meant for human eyes. Exists to avoid leaving silent loss."""

    per_category_rows: dict[str, int] = field(default_factory=dict)
    per_category_entities: dict[str, int] = field(default_factory=dict)
    empty_categories: list[str] = field(default_factory=list)
    limit_hit_categories: list[str] = field(default_factory=list)
    dropped_ambiguous: list[dict] = field(default_factory=list)
    dropped_blocklist: list[str] = field(default_factory=list)
    dropped_invalid: int = 0
    override_count: int = 0
    override_conflicts: list[dict] = field(default_factory=list)
    # Overrides that target a key in the blocklist and were REJECTED because
    # they carry no `force` (see apply_overrides).
    rejected_overrides: list[str] = field(default_factory=list)
    # Alias count BEFORE overrides are applied. The empty-dictionary gate looks
    # at this: since `overrides.json` adds ~35 entries, the total count is never
    # 0, and the disaster scenario where WDQS returns 0 rows for all categories
    # would pass the gate.
    pre_override_alias_count: int | None = None
    # Different QIDs with the SAME FULL LABEL (build_alias_index does not count
    # these as collisions, it gathers their aliases under a single canonical).
    # Canonical → {qids, categories, aliases}. Harmless for two records of the
    # same place (province/district), not for homonymous organizations/people:
    # Hizbullah (Lebanese/Turkish) and Demokratik Birlik Partisi (PYD/Northern
    # Ireland DUP) had been merged this way.
    same_label_merges: dict[str, dict] = field(default_factory=dict)
    # Groups of same-named districts (in different provinces, or two records of
    # the same place): neutral canonical → merged canonicals
    # (collapse_district_homonyms).
    district_homonym_groups: dict[str, list[str]] = field(default_factory=dict)


# ── Fetch ────────────────────────────────────────────────────


def fetch_bindings(client: SPARQLClient, cat: Category) -> tuple[list[dict], bool]:
    """Fetches the category page by page. Returns `(rows, hit_the_cap)`.

    The template produces an (item × altLabel) product; `politician_world`,
    `city_world`, `software`, `football_club_world` realistically exceed a
    single page. We advance with `OFFSET` until a short page arrives or the
    `cat.limit` cap is reached.

    The politeness delay is not set up separately: `SPARQLClient.select`
    already applies it on every consecutive request, and pages are
    consecutive requests.
    """
    page = min(PAGE_SIZE, cat.limit)
    rows: list[dict] = []
    offset = 0
    while True:
        chunk = client.select(build_query(cat, limit=page, offset=offset))
        rows.extend(chunk)
        if len(chunk) < page:
            return rows, False  # short page = data exhausted
        offset += page
        if offset >= cat.limit:
            log.warning(
                "category %s hit LIMIT %d — result truncated, split the query",
                cat.name,
                cat.limit,
            )
            return rows, True


def fetch_category(client: SPARQLClient, cat: Category, report: BuildReport) -> dict[str, EntityRecord]:
    """Fetches a single category and returns a QID → EntityRecord map."""
    bindings, limit_hit = fetch_bindings(client, cat)
    report.per_category_rows[cat.name] = len(bindings)

    if not bindings:
        # A wrong QID raises no error, it returns 0 rows. Do not stay silent.
        log.warning("category %s returned 0 rows — verify the class QID", cat.name)
        report.empty_categories.append(cat.name)
        return {}

    if limit_hit:
        report.limit_hit_categories.append(cat.name)

    out: dict[str, EntityRecord] = {}
    for b in bindings:
        item = b.get("item", {}).get("value")
        if not item:
            continue
        qid = qid_of(item)

        canonical = _pick_label(b)
        if not canonical:
            continue

        rec = out.get(qid)
        if rec is None:
            rec = EntityRecord(
                qid=qid,
                canonical=canonical,
                category=cat.name,
                priority=cat.priority,
                sitelinks=_int_of(b.get("sitelinks")),
            )
            out[qid] = rec

        # The English label is also added as a SEARCH KEY. `_pick_label` makes it
        # the canonical name only when the tr label is MISSING; if tr exists,
        # lblEn would vanish entirely. The result was measured: "Barcelona",
        # "Munich", "Vienna", "Moscow", "Athens" — none were in the dictionary,
        # because all of them have a tr label (Barselona, Münih, Viyana...).
        # Yet the WDQS row already returns `lblEn`, and the English form is
        # common in Turkish posts. The altLabels ("Barcelona City") were
        # coming, but the plain English NAME itself was not.
        en_label = b.get("lblEn", {}).get("value", "").strip()
        if en_label:
            rec.raw_aliases.add(en_label)

        # Surname alias (only in categories with cat.surname_alias on). The
        # generated name goes through the NORMAL path: the blocklist and the
        # collision policy apply as-is. This is deliberate — "Gore" is
        # generated but dropped because `gore` is in the blocklist, and
        # "Bayraktar", if generated, would be dropped as ambiguous since it
        # points to 6 people.
        if cat.surname_alias:
            for lbl in (canonical, en_label):
                if soyad := _surname_of(lbl):
                    rec.raw_aliases.add(soyad)

        alias = b.get("alias", {}).get("value")
        if alias:
            rec.raw_aliases.add(alias)

    report.per_category_entities[cat.name] = len(out)
    log.info("category %s: %d rows → %d entities", cat.name, len(bindings), len(out))
    return out


def _pick_label(binding: dict) -> str:
    """Prefers the tr label, else en. Empty if neither exists.

    ⚠️ The parenthetical qualifier is NOT stripped HERE. The canonical identity
    is the **full label**: "Ankara" and "Ankara (film)" are two separate
    entities, and if the qualifier suffix is stripped both collapse into a
    single canonical name. Then `build_alias_index` takes them for "the same
    name" and never runs the collision policy — no `dropped_ambiguous` entry,
    no sitelink tiebreak, and `to_document` merges the two QIDs into one row.
    The result: a single interest token that cannot be told apart downstream.

    The qualifier suffix is stripped only when deriving the SEARCH KEY
    (`normalize_alias`) — nobody writes "Ankara (film)" in text.
    """
    tr = binding.get("lblTr", {}).get("value", "").strip()
    if tr:
        return _normalize_ws(tr)
    en = binding.get("lblEn", {}).get("value", "").strip()
    return _normalize_ws(en) if en else ""


def _normalize_ws(s: str) -> str:
    return _WS.sub(" ", s).strip()


# Trailing words skipped in surname generation: suffixes, not surnames.
_NAME_SUFFIXES = {"jr", "jr.", "sr", "sr.", "i", "ii", "iii", "iv", "v"}
# Minimum surname length. Short fragments ("Li", "Wu") risk colliding with
# everyday Turkish syllables and are not meaningful referents on their own.
_MIN_SURNAME_LEN = 4
# Roman numerals indicating regnal order. Appearing ANYWHERE in the name
# cancels surname generation.
_REGNAL_NUMERALS = {"I", "II", "III", "IV", "V", "VI", "VII", "VIII", "IX", "X",
                    "XI", "XII", "XIII", "XIV", "XV", "XVI", "XVII", "XVIII"}


def _surname_of(full_name: str) -> str | None:
    """Extracts the surname from a full name; None if it cannot be produced.

    "Donald J. Trump" → "Trump", "Al Gore" → "Gore".
    Single-word names (Ronaldo, Pelé) return None — the canonical name is
    already an alias itself, so there is nothing to add.
    """
    parts = _normalize_ws(_PAREN_SUFFIX.sub("", full_name)).split()

    # REGNAL NAME GATE: "VI. Muhammed", "II. Abdülhamid", "I. Selim".
    # In these the last word is NOT a surname but a given name — generating it
    # would attach the King of Morocco to every post containing "Muhammed".
    # Measurement: `muhammed` → VI. Muhammed, frequently; one
    # of the most harmful false positives in a corpus heavy with religious
    # content.
    if any(p.strip(".").upper() in _REGNAL_NUMERALS for p in parts):
        return None

    while parts and parts[-1].lower().strip(",") in _NAME_SUFFIXES:
        parts.pop()
    if len(parts) < 2:
        return None
    son = parts[-1].strip(",")
    if len(son) < _MIN_SURNAME_LEN:
        return None
    return son


def _alias_surface(s: str) -> str:
    """Raw surface of the search key: qualifier stripped, whitespace plain."""
    return _normalize_ws(_PAREN_SUFFIX.sub("", s))


def _int_of(cell: dict | None) -> int:
    if not cell:
        return 0
    try:
        return int(cell.get("value", 0))
    except (TypeError, ValueError):
        return 0


def merge_entities(chunks: list[dict[str, EntityRecord]]) -> dict[str, EntityRecord]:
    """The same QID can come from several categories (e.g. both city and district).

    The record with the higher priority is kept and the aliases are MERGED —
    discarding an alias from the lower-priority category would narrow coverage
    for free.
    """
    merged: dict[str, EntityRecord] = {}
    for chunk in chunks:
        for qid, rec in chunk.items():
            cur = merged.get(qid)
            if cur is None:
                merged[qid] = rec
                continue
            if rec.priority > cur.priority:
                rec.raw_aliases |= cur.raw_aliases
                merged[qid] = rec
            else:
                cur.raw_aliases |= rec.raw_aliases
    return merged


# ── Alias normalization ──────────────────────────────────────


def normalize_alias(raw: str, min_len: int = MIN_ALIAS_LEN) -> str | None:
    """Converts an alias into a dictionary key; None if it cannot be accepted.

    Key generation uses `fold()` — and so does the lookup on the Go side. If
    the two diverge, lookups silently miss (see fold.py).

    The parenthetical qualifier ("Ankara (film)" → "ankara") is stripped ONLY
    here; the canonical name itself stays as the full label (see `_pick_label`).
    """
    s = _alias_surface(raw)
    if not s:
        return None
    key = fold(s)
    if len(key) < min_len:
        return None
    if not _HAS_LETTER.search(key):
        return None
    return key


# ── Collision resolution ─────────────────────────────────────


def build_alias_index(
    entities: dict[str, EntityRecord],
    blocklist: set[str],
    report: BuildReport,
) -> dict[str, str]:
    """Alias → canonical name map. Ambiguous names are dropped."""
    # The bucket is KEYED by QID, NOT a list. Several raw strings of a single
    # entity can fold to the same key — the canonical name "Fenerbahçe" and the
    # altLabel "Fenerbahçe (futbol takımı)" both become `fenerbahce` once
    # `normalize_alias` strips the qualifier suffix. In the old list-based
    # version the same record entered the bucket TWICE and the `ranked[1]`
    # runner-up below became THE ENTITY ITSELF; the sitelink ratio dropped to
    # 1.0 and could never pass AMBIGUITY_RATIO.
    #
    # Measured result: `fenerbahce` → [Q6601875(92), Q6601875(92),
    # Q108465112(8)] → 92 >= 3×92 false → DROPPED. Same for `besiktas`. So the
    # league's two biggest clubs were missing from the dictionary and the bug
    # hit exactly the MOST POPULAR entities: the entity with the most aliases
    # is the most likely to collide with its own canonical name.
    # After dedup the runner-up is Q108465112(8), 92 >= 24 → it stays.
    candidates: dict[str, dict[str, EntityRecord]] = defaultdict(dict)

    for rec in entities.values():
        for raw in {rec.canonical, *rec.raw_aliases}:
            key = normalize_alias(raw)
            if key is None:
                report.dropped_invalid += 1
                continue
            if key in blocklist:
                report.dropped_blocklist.append(key)
                continue
            candidates[key].setdefault(rec.qid, rec)

    aliases: dict[str, str] = {}
    for key, bucket in candidates.items():
        recs = list(bucket.values())
        # Collision detection looks at the QID, not the cleaned NAME. Looking at
        # the name (the old behavior) collapsed "Ankara" and "Ankara (film)"
        # into one name and never ran the policy; they are separate entities.
        #
        # The second condition is deliberate: if two different QIDs have
        # EXACTLY the same full label (Wikidata labels are not unique), the
        # alias → single canonical contract is not broken anyway, the target
        # name is the same. It is not a collision, just two records of the same
        # name.
        if len({r.qid for r in recs}) == 1 or len({r.canonical for r in recs}) == 1:
            aliases[key] = recs[0].canonical
            if len({r.qid for r in recs}) > 1:
                m = report.same_label_merges.setdefault(
                    recs[0].canonical, {"qids": set(), "categories": set(), "aliases": 0}
                )
                m["qids"].update(r.qid for r in recs)
                m["categories"].update(r.category for r in recs)
                m["aliases"] += 1
            continue

        # OWN NAME rule: if EXACTLY ONE of the candidates has this key as its
        # canonical name, it wins. Being an entity's NAME is a stronger referent
        # than merely carrying that name as an alternative.
        #
        # ⚠️ ORDER: this rule comes even BEFORE the priority test. When priority
        # was checked first, a SMALL entity carrying someone else's name as an
        # alias could win merely because its category was higher. Measured
        # example: `turkiye` resolved to "Türkiye Gazetesi" (newspaper_tr,
        # priority 86, 11 sitelinks) while the country "Türkiye" (country,
        # priority 70, 424 sitelinks) lost. Many wrong matches on its own. Category priority is a TYPE preference; it must not
        # override the question of which entity actually bears that NAME.
        #
        # The rule also comes before the sitelink ratio because the ratio was
        # systematically giving the wrong answer here: every place in Anatolia
        # also has an ancient name and the two fall into the SAME category as
        # separate QIDs. Measurement: 103 of the 286 ambiguity drops involved TR
        # cities/districts and most were exactly this pattern —
        #
        #   kadikoy → Kadıköy(61) vs Kalkedon(44)  ratio 1.39 → DROPPED
        #   esenyurt → Esenyurt(42) vs Büyükçekmece(41)  ratio 1.02 → DROPPED
        #
        # So Istanbul's most frequently mentioned district was missing from the
        # dictionary. The rule also fixes the DIRECTION: "bergama" used to
        # resolve to Pergamon (ancient name) by ratio, now it resolves to
        # Bergama; "nikaia" correctly stays at Nikaia and does not slide to İznik.
        #
        # Since the qualifier suffix is stripped in normalize_alias, BOTH
        # "Ankara" and "Ankara (film)" pass this test → there is no single
        # candidate → the rule does not kick in and the decision is left to
        # priority/ratio (as intended).
        #
        # ⚠️ The "single candidate" condition is DELIBERATE. Loosening it to
        # "the most popular among those bearing their own name" was tried and it
        # rescued 40 more names, but was reverted because measurement showed
        # wrong answers: looking at popularity disables priority (`demokrat
        # parti` → Demokrat Parti (Thailand)), and looking at priority makes the
        # "those in Turkey first" rule break global names (`philadelphia` →
        # Philadelphia (Lydia)). The rest it rescued were ancient places like
        # Aianteion/Argyria/Flaviopolis — no counterpart in the Turkish stream.
        # DROPPING when ambiguous is the right default here.
        # Several candidates may bear their own name and this is usually TWO
        # records of the SAME place: a province/town and the administrative
        # district are separate QIDs and the qualifier suffix is stripped in
        # normalize_alias ("Van" + "Van (ilçe)"). Among them we choose by
        # PRIORITY, then popularity; a candidate that does not bear its own name
        # does not compete at all.
        #
        # Measured example: `van` was resolving to Vanuatu because the two
        # self-named records cancelled each other and left the decision to the
        # country category (priority 70), and Vanuatu carries the ISO code "VAN".
        # The same pattern: `bergama` → Pergamon and `eregli` → Karadeniz Ereğli.
        #
        # The choice is made by PRIORITY, not POPULARITY: pure popularity was
        # tried and gave `demokrat parti` → Demokrat Parti (Thailand) — the
        # Turkish party has fewer sitelinks.
        # ⚠️ If several self-named candidates are in the SAME category the rule
        # does NOT KICK IN. The distinction: being in the same category means
        # being a real rival (like two separate "Ereğli" districts) and staying
        # silent is correct there; being in different categories is mostly an
        # ADMINISTRATIVE DIVISION (province record + district record) and the
        # type preference (priority) we already encoded gives the right answer.
        self_named = [r for r in recs if normalize_alias(r.canonical) == key]
        if self_named and len({r.category for r in self_named}) == len(self_named):
            best = max(self_named, key=lambda r: (r.priority, r.sitelinks, r.qid))
            aliases[key] = best.canonical
            continue

        ranked = sorted(recs, key=lambda r: (r.priority, r.sitelinks), reverse=True)
        top, runner = ranked[0], ranked[1]

        if top.priority > runner.priority:
            aliases[key] = top.canonical
            continue

        if top.sitelinks >= AMBIGUITY_RATIO * max(runner.sitelinks, 1):
            aliases[key] = top.canonical
            continue

        report.dropped_ambiguous.append(
            {
                "alias": key,
                "candidates": [
                    {
                        "canonical": r.canonical,
                        "qid": r.qid,
                        "category": r.category,
                        "priority": r.priority,
                        "sitelinks": r.sitelinks,
                    }
                    for r in ranked[:4]
                ],
            }
        )

    # Deduplicate the blocklist report (the same word can come from many entities).
    report.dropped_blocklist = sorted(set(report.dropped_blocklist))
    return aliases


def apply_overrides(
    aliases: dict[str, str],
    entities: dict[str, EntityRecord],
    overrides: dict[str, object],
    report: BuildReport,
    blocklist: set[str] | None = None,
) -> None:
    """Applies the hand-maintained mappings — they always win.

    Wikidata does not carry social media jargon ("Sarı Lacivert", "Altılı
    Masa", "Ana Muhalefet"). This file fills that gap and is also the way to
    manually fix a wrongly resolved collision.

    **The blocklist also binds overrides.** The rationale of the
    substring-grounding decision was "the blocklist + MIN_ALIAS_LEN already
    reduce the risk"; if overrides were exempt from both, that rationale would
    collapse — indeed common nouns like "aslan"/"fener"/"gemini" were entering
    through exactly this door. For a deliberate exception the value can be
    written as `{"value": "...", "force": true}`; the plain string form is
    also valid.
    """
    blocked = blocklist or set()
    known = {r.canonical for r in entities.values()}
    for raw_alias, raw_value in overrides.items():
        canonical, forced = _override_value(raw_alias, raw_value)
        key = normalize_alias(raw_alias, min_len=MIN_OVERRIDE_ALIAS_LEN)
        if key is None:
            log.warning("invalid override alias, skipping: %r", raw_alias)
            continue
        if not canonical:
            log.warning("empty override canonical name, skipping: %r", raw_alias)
            continue
        if key in blocked and not forced:
            log.warning(
                "override key is in the blocklist, REJECTED: %r → %r "
                '(for a deliberate exception write {"value": ..., "force": true})',
                key,
                canonical,
            )
            report.rejected_overrides.append(key)
            continue
        prev = aliases.get(key)
        if prev is not None and prev != canonical:
            report.override_conflicts.append({"alias": key, "was": prev, "now": canonical})
        aliases[key] = canonical
        report.override_count += 1
        if canonical not in known:
            # A canonical name with no Wikidata counterpart: a valid case
            # (local jargon). It does not enter entities, it is only an alias target.
            log.debug("override canonical name not in the Wikidata set: %s", canonical)


def _override_value(raw_alias: str, raw_value: object) -> tuple[str, bool]:
    """`"Fenerbahçe"` or `{"value": "...", "force": true}` → (name, force)."""
    if isinstance(raw_value, dict):
        value = str(raw_value.get("value", ""))
        return _normalize_ws(value), bool(raw_value.get("force", False))
    if raw_value is None:
        log.warning("empty override value: %r", raw_alias)
        return "", False
    return _normalize_ws(str(raw_value)), False


# ── Output ───────────────────────────────────────────────────


def to_document(
    aliases: dict[str, str],
    entities: dict[str, EntityRecord],
    report: BuildReport,
    generated_at: str,
) -> dict:
    """Body of canonical_dict.json."""
    ent_out: dict[str, dict] = {}
    for rec in entities.values():
        cur = ent_out.get(rec.canonical)
        if cur is not None and cur["sitelinks"] >= rec.sitelinks:
            continue
        ent_out[rec.canonical] = {
            "qid": rec.qid,
            "category": rec.category,
            "sitelinks": rec.sitelinks,
        }
    meta = {
        "generated_at": generated_at,
        "alias_count": len(aliases),
        "entity_count": len(ent_out),
        "ambiguity_ratio": AMBIGUITY_RATIO,
        "min_alias_len": MIN_ALIAS_LEN,
        "min_override_alias_len": MIN_OVERRIDE_ALIAS_LEN,
        "categories": sorted(report.per_category_entities),
    }
    # Meaningful only in a real fetch; old documents loaded with `--from-json`
    # lack the field and the CLI falls back to the existing alias_count behavior.
    if report.pre_override_alias_count is not None:
        meta["pre_override_alias_count"] = report.pre_override_alias_count
    # Both maps are written SORTED: for the same SPARQL responses the document
    # must be byte-for-byte identical, otherwise the "did it change" question
    # goes unanswered.
    return {
        "meta": meta,
        "entities": dict(sorted(ent_out.items())),
        "aliases": dict(sorted(aliases.items())),
    }


# The "ilçesi" suffix of a district label: "Bafra ilçesi", "Merzifon İlçesi",
# "Ilgaz district", "Diyadin'in ilçesi", "Ayvacık İlçesi (Samsun)".
# The parenthesized form ("Abana (ilçe)") is NOT here — normalize_alias
# already strips that suffix and the bare name enters the dictionary on its own.
_DISTRICT_SUFFIX = re.compile(
    r"(?:'(?:in|ın|un|ün|nin|nın|nun|nün))?\s+(?:ilçesi|İlçesi|district|District)(?:\s*\(.*\))?$"
)


def add_district_bare_aliases(entities: dict[str, EntityRecord], allow: set[str]) -> list[str]:
    """Adds the bare name, with the "ilçesi" suffix stripped, to `district_tr` records.

    Only for the (folded) names in `allow` — NO bulk stripping. A measurement
    (real post sample): stripping the suffix surfaced 216 district names not
    in the dictionary, and adding them in bulk raised coverage slightly, but most of the gain was poison — some district names are common
    nouns or day names: `alaca` 85 of 87 matches inside "alacak",
    `çarşamba`/`perşembe` days, `çay`, `hani`, `han`, `kale`, `göle` ("göle
    döndü"), `köşe` ("köşe yazısı"). The hand-curated list
    (`district_bare_allow.txt`) brought a small gain; nearly all matches were in a
    correct place context. So this is NOT a coverage lever, it is about
    labeling local news (kaymakam, belediye, 📍 location) correctly.

    The bare name is added as a raw alias, so it goes through the ambiguity
    policy and the blocklist in the normal way. Returns: names that are in the
    list but found no counterpart in any record (the Wikidata label may have
    changed).
    """
    used: set[str] = set()
    for rec in entities.values():
        if rec.category != "district_tr":
            continue
        m = _DISTRICT_SUFFIX.search(rec.canonical)
        if not m:
            continue
        bare = rec.canonical[: m.start()]
        key = normalize_alias(bare)
        if key is not None and key in allow:
            rec.raw_aliases.add(bare)
            used.add(key)
    return sorted(allow - used)


# Meaningless common name: "Merkez ilçesi" exists in every province, and a
# neutral canonical would point nowhere — the group is not merged (and
# `merkez` is in the blocklist).
_HOMONYM_SKIP = frozenset({"merkez"})


def _district_base(label: str) -> str:
    """Strips province/district affixes from a district label: "Ayvacık İlçesi (Samsun)",
    "Ayvacık (Çanakkale ilçesi)", "Edremit (Balıkesir) ilçesi",
    "Kemer district (Antalya)" → "Ayvacık" / "Edremit" / "Kemer"."""
    base = re.sub(r"\s*\([^)]*\)", "", label)
    base = _DISTRICT_SUFFIX.sub("", base)
    return _normalize_ws(base)


def collapse_district_homonyms(
    aliases: dict[str, str], entities: dict[str, EntityRecord], report: BuildReport
) -> int:
    """Gathers the names of district records bearing the same name into a SINGLE neutral canonical.

    Why: the most common pattern in news is "Ayvacık ilçesinde" → the
    `ayvacik ilcesi` key. Çanakkale's Ayvacık produced only `ayvacik` from its
    label "Ayvacık (Çanakkale ilçesi)", while Samsun's produced `ayvacik
    ilcesi` from "Ayvacık İlçesi (Samsun)": a Çanakkale news item was linked to
    Samsun, and the bare "Ayvacık" in the same post went to a different
    canonical (wrong province + two interest keys for the same place). Which
    province the text means cannot be resolved by the dictionary; the right
    thing is a single key at the name level. ~48 groups (some are two records
    of the same place: "Ağlı" / "Ağlı (ilçe)" — merging those is also correct).

    Only names whose target is a group member are retargeted; a NON-district
    entity with the same spelling (e.g. a company) is not affected. Returns:
    the number of retargeted names.
    """
    groups: dict[str, list[EntityRecord]] = defaultdict(list)
    for rec in entities.values():
        if rec.category != "district_tr":
            continue
        base = _district_base(rec.canonical)
        key = normalize_alias(base)
        if key and key not in _HOMONYM_SKIP:
            groups[key].append(rec)
    target: dict[str, str] = {}
    for key, recs in groups.items():
        if len({r.canonical for r in recs}) < 2:
            continue
        top = max(recs, key=lambda r: r.sitelinks)
        neutral = _district_base(top.canonical)
        report.district_homonym_groups[neutral] = sorted({r.canonical for r in recs})
        for r in recs:
            target[r.canonical] = neutral
    moved = 0
    for alias, canon in aliases.items():
        if canon in target and target[canon] != canon:
            aliases[alias] = target[canon]
            moved += 1
    return moved


def run(
    client: SPARQLClient,
    overrides: dict[str, object],
    blocklist: set[str],
    categories: list[Category] | None = None,
    district_allow: set[str] | None = None,
) -> tuple[dict, BuildReport]:
    """Full build flow. Network calls are made only through `client`."""
    cats = categories if categories is not None else CATEGORIES
    report = BuildReport()

    chunks = [fetch_category(client, cat, report) for cat in cats]
    entities = merge_entities(chunks)
    if district_allow:
        unused = add_district_bare_aliases(entities, district_allow)
        # In a partial build with --category, if district_tr was never fetched
        # the whole list looks "unused"; that information is noise.
        if unused and any(r.category == "district_tr" for r in entities.values()):
            log.warning("%d names in the district allowlist have no match: %s", len(unused), ", ".join(unused))
    aliases = build_alias_index(entities, blocklist, report)
    moved = collapse_district_homonyms(aliases, entities, report)
    if moved:
        log.info("same-name districts: %d groups, %d names collapsed to a neutral canonical", len(report.district_homonym_groups), moved)
    # The number the empty-dictionary gate will look at: BEFORE overrides are added.
    report.pre_override_alias_count = len(aliases)
    apply_overrides(aliases, entities, overrides, report, blocklist)

    generated_at = datetime.now(timezone.utc).isoformat(timespec="seconds")
    return to_document(aliases, entities, report, generated_at), report


# ── File helpers ─────────────────────────────────────────────


def load_overrides(path: Path) -> dict[str, object]:
    """`{"fb": "Fenerbahçe"}` or `{"ts": {"value": ..., "force": true}}`.

    The dict form is passed through intact; `_override_value` interprets it.
    """
    if not path.exists():
        return {}
    data = json.loads(path.read_text(encoding="utf-8"))
    return {str(k): (v if isinstance(v, dict) else str(v)) for k, v in data.items()}


def load_blocklist(path: Path) -> set[str]:
    """One word per line; lines starting with `#` are comments.

    Keys are stored folded, because comparison is done folded.
    """
    if not path.exists():
        return set()
    out: set[str] = set()
    for line in path.read_text(encoding="utf-8").splitlines():
        line = line.split("#", 1)[0].strip()
        if line:
            out.add(fold(line))
    return out
