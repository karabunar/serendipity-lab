"""Builds the dictionary from a local Wikidata JSON dump instead of WDQS.

`DumpClient` has the same `select(query)` contract as `sparql.SPARQLClient`, so
`build.run` works unchanged: the category queries are matched back to their
`Category` and answered from an index built over the dump. No network, no
User-Agent, no rate limit, and the same dump always gives the same dictionary.

Input format: the standard Wikidata entity dump (`latest-all.json[.bz2|.gz]`),
one JSON entity per line inside a JSON array (`[`, `{...},`, `{...},`, `]`).
Only `type == "item"` lines are used.

How a category is evaluated. `Category.selector` is the SPARQL pattern the
WDQS path uses. To keep the two paths from drifting, the same selector text is
parsed (`parse_selector`) into a small predicate instead of being re-written by
hand. Supported grammar (everything `queries.CATEGORIES` uses):

    ?item wdt:P31 wd:Qn .                      direct class
    ?item wdt:P31/wdt:P279* wd:Qn ; wdt:P17 wd:Qm .   subclass walk + extra claim
    { ... } UNION { ... }                      alternatives
    VALUES ?cls { wd:Q1 wd:Q2 } ... ?cls       class sets
    FILTER NOT EXISTS { ?item wdt:P31/wdt:P279* ?bad . VALUES ?bad { ... } }

Anything else raises `DumpError` instead of silently matching nothing.

Two passes over the dump:
  1. the subclass graph (`P279` edges of every item that has them);
  2. every item is tested against every selected category and only matching
     items are kept, so memory is bounded by the dictionary, not the dump.

Differences from WDQS worth knowing: `wdt:` truthiness is reproduced (deprecated
statements ignored, preferred rank wins over normal), `wikibase:sitelinks` is
the number of entries in the entity's `sitelinks`, and an item contributes one
row per alias (or one row when it has none), exactly like the OPTIONAL join in
the query template.
"""

from __future__ import annotations

import bz2
import gzip
import json
import logging
import re
from collections import defaultdict
from dataclasses import dataclass, field
from pathlib import Path
from typing import Iterable, Iterator

from .queries import Category, build_query

log = logging.getLogger(__name__)

ENTITY_PREFIX = "http://www.wikidata.org/entity/"
SUBCLASS = "P279"
INSTANCE_OF = "P31"


class DumpError(RuntimeError):
    pass


# ── Selector parsing ─────────────────────────────────────────


@dataclass(frozen=True)
class Triple:
    """`?item wdt:<pid>[/wdt:P279*] <targets>`"""

    pid: str
    closure: bool
    targets: frozenset[str]


@dataclass(frozen=True)
class Selector:
    # AND of blocks; a block is an OR of triple-lists (each list is an AND).
    blocks: tuple[tuple[tuple[Triple, ...], ...], ...]
    # Item must have NONE of these.
    exclusions: tuple[Triple, ...] = ()

    def triples(self) -> Iterator[Triple]:
        for block in self.blocks:
            for alt in block:
                yield from alt
        yield from self.exclusions


_QID = r"wd:(Q\d+)"
_TRIPLE = re.compile(
    r"\?item\s+wdt:(P\d+)(/wdt:P279\*)?\s+(wd:Q\d+|\?[A-Za-z]+)(?:\s*;\s*|\s*\.?\s*$)"
)
_VALUES = re.compile(r"VALUES\s+\?([A-Za-z]+)\s*\{([^}]*)\}")


def _matching_brace(text: str, open_at: int) -> int:
    depth = 0
    for i in range(open_at, len(text)):
        if text[i] == "{":
            depth += 1
        elif text[i] == "}":
            depth -= 1
            if depth == 0:
                return i
    raise DumpError(f"unbalanced braces in selector: {text!r}")


def _values(text: str) -> tuple[str, dict[str, frozenset[str]]]:
    """Strips `VALUES ?v { wd:.. }` clauses and returns the variable bindings."""
    env: dict[str, frozenset[str]] = {}
    for m in _VALUES.finditer(text):
        env[m.group(1)] = frozenset(re.findall(_QID, m.group(2)))
    return _VALUES.sub(" ", text), env


def _triples(text: str, env: dict[str, frozenset[str]]) -> tuple[Triple, ...]:
    """Parses `?item wdt:P31 X ; wdt:P17 Y .` chains (the subject is shared)."""
    text = text.strip()
    if not text:
        return ()
    # `;` chains omit the subject for every triple but the first.
    parts = [p.strip() for p in re.split(r"\s;\s|;\s*(?=wdt:)", text.rstrip(". ").strip())]
    out: list[Triple] = []
    for i, part in enumerate(parts):
        piece = part if i == 0 else f"?item {part}"
        m = _TRIPLE.match(piece.strip() + " .")
        if not m:
            raise DumpError(f"unsupported selector triple: {part!r}")
        pid, walk, target = m.group(1), m.group(2), m.group(3)
        if walk and pid != INSTANCE_OF:
            raise DumpError(f"subclass walk is only supported on P31: {part!r}")
        if target.startswith("?"):
            qids = env.get(target[1:])
            if qids is None:
                raise DumpError(f"unbound variable {target} in {part!r}")
        else:
            qids = frozenset({target[3:]})
        out.append(Triple(pid, bool(walk), qids))
    return tuple(out)


def parse_selector(selector: str) -> Selector:
    text = selector.strip()

    # FILTER NOT EXISTS { ... }
    exclusions: list[Triple] = []
    while (m := re.search(r"FILTER\s+NOT\s+EXISTS\s*\{", text)):
        close = _matching_brace(text, m.end() - 1)
        inner = text[m.end() : close]
        text = text[: m.start()] + " " + text[close + 1 :]
        inner, env = _values(inner)
        exclusions.extend(_triples(inner, env))

    text, env = _values(text)

    blocks: list[tuple[tuple[Triple, ...], ...]] = []
    rest: list[str] = []
    pos = 0
    while pos < len(text):
        if text[pos] == "{":
            alts: list[tuple[Triple, ...]] = []
            while True:
                close = _matching_brace(text, pos)
                alts.append(_triples(text[pos + 1 : close], env))
                after = text[close + 1 :].lstrip()
                if after.startswith("UNION"):
                    pos = text.index("{", close)
                    continue
                pos = len(text) - len(after)
                break
            blocks.append(tuple(alts))
        else:
            nxt = text.find("{", pos)
            seg = text[pos:] if nxt == -1 else text[pos:nxt]
            rest.append(seg)
            pos = len(text) if nxt == -1 else nxt
    tail = " ".join(s.strip() for s in rest).strip()
    for chunk in filter(None, (c.strip() for c in re.split(r"\s\.\s", tail + " "))):
        triples = _triples(chunk, env)
        if triples:
            blocks.append((triples,))
    if not blocks:
        raise DumpError(f"selector has no patterns: {selector!r}")
    return Selector(tuple(blocks), tuple(exclusions))


# ── Dump reading ─────────────────────────────────────────────


def _open(path: Path):
    suffix = path.suffix.lower()
    if suffix == ".bz2":
        return bz2.open(path, "rt", encoding="utf-8")
    if suffix == ".gz":
        return gzip.open(path, "rt", encoding="utf-8")
    return path.open("rt", encoding="utf-8")


def iter_items(path: Path) -> Iterator[dict]:
    """Yields the `item` entities of a Wikidata JSON dump, one line at a time."""
    with _open(path) as fh:
        for line in fh:
            line = line.strip()
            if line in ("", "[", "]"):
                continue
            if line.endswith(","):
                line = line[:-1]
            try:
                ent = json.loads(line)
            except ValueError as exc:
                raise DumpError(f"{path}: malformed entity line: {exc}") from exc
            if ent.get("type") == "item":
                yield ent


def truthy_ids(entity: dict, pid: str) -> frozenset[str]:
    """QIDs of the statements WDQS exposes through `wdt:` (best rank, no deprecated)."""
    best: dict[str, list[str]] = {"preferred": [], "normal": []}
    for st in entity.get("claims", {}).get(pid, ()):
        rank = st.get("rank", "normal")
        if rank not in best:
            continue
        snak = st.get("mainsnak", {})
        if snak.get("snaktype") != "value":
            continue
        value = snak.get("datavalue", {}).get("value")
        if isinstance(value, dict) and "id" in value:
            best[rank].append(value["id"])
    return frozenset(best["preferred"] or best["normal"])


@dataclass
class Hit:
    qid: str
    lbl_tr: str
    lbl_en: str
    aliases: tuple[str, ...]
    sitelinks: int


@dataclass
class DumpIndex:
    """Per-category hits, ready to be served page by page."""

    hits: dict[str, list[Hit]] = field(default_factory=dict)
    items_seen: int = 0

    @classmethod
    def build(cls, path: Path, categories: Iterable[Category]) -> "DumpIndex":
        cats = list(categories)
        selectors = {c.name: parse_selector(c.selector) for c in cats}

        # Pass 1: subclass graph.
        children: dict[str, set[str]] = defaultdict(set)
        for ent in iter_items(path):
            for parent in truthy_ids(ent, SUBCLASS):
                children[parent].add(ent["id"])
        log.info("dump pass 1: %d classes with subclasses", len(children))

        closures: dict[frozenset[str], frozenset[str]] = {}

        def descendants(roots: frozenset[str]) -> frozenset[str]:
            if roots not in closures:
                seen = set(roots)
                stack = list(roots)
                while stack:
                    for child in children.get(stack.pop(), ()):
                        if child not in seen:
                            seen.add(child)
                            stack.append(child)
                closures[roots] = frozenset(seen)
            return closures[roots]

        def holds(ent_claims: dict[str, frozenset[str]], t: Triple) -> bool:
            have = ent_claims.get(t.pid, frozenset())
            return bool(have & (descendants(t.targets) if t.closure else t.targets))

        needed_pids = {t.pid for sel in selectors.values() for t in sel.triples()}

        # Pass 2: test every item against every category.
        index = cls(hits={c.name: [] for c in cats})
        for ent in iter_items(path):
            index.items_seen += 1
            claims = {pid: truthy_ids(ent, pid) for pid in needed_pids}
            sitelinks = len(ent.get("sitelinks", {}))
            labels = ent.get("labels", {})
            lbl_tr = labels.get("tr", {}).get("value", "")
            lbl_en = labels.get("en", {}).get("value", "")
            if not (lbl_tr or lbl_en):
                continue
            aliases: tuple[str, ...] | None = None
            for cat in cats:
                if sitelinks < cat.min_sitelinks:
                    continue
                sel = selectors[cat.name]
                if not all(any(all(holds(claims, t) for t in alt) for alt in block) for block in sel.blocks):
                    continue
                if any(holds(claims, t) for t in sel.exclusions):
                    continue
                if aliases is None:
                    found = {
                        a["value"]
                        for lang in ("tr", "en")
                        for a in ent.get("aliases", {}).get(lang, ())
                        if a.get("value")
                    }
                    aliases = tuple(sorted(found))
                index.hits[cat.name].append(Hit(ent["id"], lbl_tr, lbl_en, aliases, sitelinks))
        log.info("dump pass 2: %d items, %s", index.items_seen, {k: len(v) for k, v in index.hits.items()})
        return index


# ── SPARQLClient-compatible facade ───────────────────────────

_PAGING = re.compile(r"LIMIT\s+(\d+)\s+OFFSET\s+(\d+)\s*$")


class DumpClient:
    """Answers the category queries `build.fetch_bindings` issues, from a dump."""

    def __init__(self, index: DumpIndex, categories: Iterable[Category]):
        self._index = index
        self._cats = list(categories)
        self._rows: dict[str, list[dict]] = {}

    @classmethod
    def from_dump(cls, path: Path, categories: Iterable[Category]) -> "DumpClient":
        cats = list(categories)
        return cls(DumpIndex.build(path, cats), cats)

    def __enter__(self) -> "DumpClient":
        return self

    def __exit__(self, *_exc) -> None:
        return None

    def close(self) -> None:
        return None

    def select(self, query: str) -> list[dict]:
        m = _PAGING.search(query.strip())
        if not m:
            raise DumpError("query has no LIMIT/OFFSET page")
        limit, offset = int(m.group(1)), int(m.group(2))
        for cat in self._cats:
            if build_query(cat, limit=limit, offset=offset) == query:
                return self._category_rows(cat)[offset : offset + limit]
        raise DumpError("query does not belong to any selected category")

    def _category_rows(self, cat: Category) -> list[dict]:
        rows = self._rows.get(cat.name)
        if rows is None:
            # ORDER BY DESC(?sitelinks) ?item — the item compares as its URI string.
            hits = sorted(self._index.hits[cat.name], key=lambda h: (-h.sitelinks, ENTITY_PREFIX + h.qid))
            rows = []
            for h in hits:
                base: dict = {"item": {"value": ENTITY_PREFIX + h.qid}, "sitelinks": {"value": str(h.sitelinks)}}
                if h.lbl_tr:
                    base["lblTr"] = {"value": h.lbl_tr}
                if h.lbl_en:
                    base["lblEn"] = {"value": h.lbl_en}
                if not h.aliases:
                    rows.append(base)
                for alias in h.aliases:
                    rows.append({**base, "alias": {"value": alias}})
            self._rows[cat.name] = rows
        return rows
