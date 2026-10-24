#!/usr/bin/env python3
"""Builds the Wikidata-based canonical entity dictionary.

An offline script that runs infrequently (weekly/monthly). It is NOT a
deployed service: its output is `canonical_dict.json` and the
`serendipity:canonical:*` keys in Redis; both the content-side entity
normalization and the Go matcher read them.

Usage:

    # Write to file only (network yes, Redis no)
    python python/build_canonical_dict.py --out canonical_dict.json

    # Write to Redis too
    python python/build_canonical_dict.py --out canonical_dict.json \
        --redis-url redis://localhost:6379/0

    # Try a single category (QID verification / fast iteration)
    python python/build_canonical_dict.py --category football_club_tr --dry-run

    # Build from a local Wikidata JSON dump (no network, no User-Agent)
    python python/build_canonical_dict.py --dump latest-all.json.bz2 \
        --out canonical_dict.json

    # Load an existing JSON into Redis without touching the network
    python python/build_canonical_dict.py --from-json canonical_dict.json \
        --redis-url redis://localhost:6379/0

Environment variables:

    WIKIDATA_USER_AGENT      UA containing contact info (Wikimedia policy)
    WIKIDATA_SPARQL_ENDPOINT Default https://query.wikidata.org/sparql

Risk: entity normalization inconsistency.
"""

from __future__ import annotations

import argparse
import json
import logging
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

from canonical_dict import build as build_mod  # noqa: E402
from canonical_dict import redis_sink  # noqa: E402
from canonical_dict.dump import DumpClient  # noqa: E402
from canonical_dict.queries import CATEGORIES, category_by_name  # noqa: E402
from canonical_dict.sparql import SPARQLClient, SPARQLConfig  # noqa: E402

HERE = Path(__file__).resolve().parent / "canonical_dict"
OVERRIDES_PATH = HERE / "overrides.json"
BLOCKLIST_PATH = HERE / "blocklist.txt"
DISTRICT_ALLOW_PATH = HERE / "district_bare_allow.txt"


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    p = argparse.ArgumentParser(description="Wikidata canonical entity dictionary builder")
    p.add_argument("--out", type=Path, help="canonical_dict.json output path")
    p.add_argument("--report", type=Path, help="conflict/dropped-record report (JSON) path")
    p.add_argument("--redis-url", help="Redis to write to (e.g. redis://localhost:6379/0)")
    p.add_argument(
        "--from-json",
        type=Path,
        help="Load an existing JSON without network access (for Redis writes only)",
    )
    p.add_argument(
        "--dump",
        type=Path,
        help="Build from a local Wikidata JSON dump (latest-all.json[.bz2|.gz]) instead of "
        "querying WDQS. Same categories, same build rules; needs no network or User-Agent",
    )
    p.add_argument(
        "--category",
        action="append",
        default=[],
        help="Fetch only this category (repeatable). All if empty.",
    )
    p.add_argument(
        "--max-aliases",
        type=int,
        default=200_000,
        help="Safety valve: a dictionary above this count is not written (memory safety valve)",
    )
    p.add_argument(
        "--allow-empty",
        action="store_true",
        help="Allow publishing a dictionary with no Wikidata-sourced aliases "
        "(default: reject — so a WDQS outage cannot silently wipe a healthy dictionary). "
        "The gate checks the count BEFORE overrides are added",
    )
    p.add_argument(
        "--allow-empty-categories",
        action="store_true",
        help="Publish even when a category returned 0 rows (default: reject — "
        "if a class QID changed the dictionary silently shrinks)",
    )
    p.add_argument(
        "--allow-truncated-categories",
        action="store_true",
        help="Publish even when a category hit its LIMIT cap (default: reject)",
    )
    p.add_argument(
        "--partial-ok",
        action="store_true",
        help="Allow writing a PARTIAL dictionary built with --category to Redis",
    )
    p.add_argument(
        "--force",
        action="store_true",
        help="Bypass the shrink guard (new dictionary more than %%25 smaller than live)",
    )
    p.add_argument("--dry-run", action="store_true", help="Do not write to Redis, only count")
    p.add_argument("-v", "--verbose", action="store_true")
    return p.parse_args(argv)


# If the new dictionary shrinks from the live one by more than this ratio, it
# is not published without --force. A partial failure on the WDQS side (a few
# categories empty) does not produce a fully empty dictionary, only a
# seriously incomplete one — the empty gate cannot catch it.
SHRINK_LIMIT = 0.25


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv)
    logging.basicConfig(
        level=logging.DEBUG if args.verbose else logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )
    log = logging.getLogger("build_canonical_dict")

    # Partial-publish gate BEFORE touching the network: a document produced with
    # --category contains only that category, but redis_sink.write swaps the
    # WHOLE live dictionary with it. So adding --redis-url to the "quickly try a
    # single category" command reduces 17 categories to 1.
    if args.category and args.redis_url and not args.partial_ok:
        log.error(
            "A PARTIAL dictionary is being built with --category; --redis-url would SWAP it for the live dictionary "
            "and delete the other categories. Pass --partial-ok if intended."
        )
        return 8

    if args.dump and args.from_json:
        log.error("--dump and --from-json are mutually exclusive")
        return 2

    if args.from_json:
        doc = json.loads(args.from_json.read_text(encoding="utf-8"))
        report = None
        log.info("loaded from JSON: %s", args.from_json)
    else:
        cats = list(CATEGORIES)
        if args.category:
            cats = []
            for name in args.category:
                cat = category_by_name(name)
                if cat is None:
                    log.error("unknown category: %s", name)
                    return 2
                cats.append(cat)

        overrides = build_mod.load_overrides(OVERRIDES_PATH)
        blocklist = build_mod.load_blocklist(BLOCKLIST_PATH)
        # Same format as the blocklist (one name per line, `#` comment) → same loader.
        district_allow = build_mod.load_blocklist(DISTRICT_ALLOW_PATH)
        log.info("%d categories, %d overrides, %d blocklist entries", len(cats), len(overrides), len(blocklist))

        if args.dump:
            if not args.dump.is_file():
                log.error("dump not found: %s", args.dump)
                return 2
            client_cm = DumpClient.from_dump(args.dump, cats)
        else:
            client_cm = SPARQLClient(SPARQLConfig.from_env())
        with client_cm as client:
            doc, report = build_mod.run(client, overrides, blocklist, cats, district_allow)

    alias_count = len(doc["aliases"])
    # The empty gate looks at the count BEFORE overrides: since overrides.json
    # adds ~35 entries, the total count is never 0 and the "all 17 categories
    # returned 0 rows" disaster would pass the gate. Old documents lack the
    # field; in that case the old behavior (alias_count) is preserved.
    gate_count = doc.get("meta", {}).get("pre_override_alias_count", alias_count)
    log.info(
        "dictionary ready: %d aliases (%d wikidata), %d entities, ~%d bytes",
        alias_count,
        gate_count,
        len(doc["entities"]),
        redis_sink.estimate_bytes(doc),
    )

    if gate_count == 0 and not args.allow_empty:
        # A WDQS outage / wrong category filter can produce an empty dictionary.
        # Publishing it means deleting the live dictionary and the normalization
        # layer shuts down without any warning.
        log.error(
            "dictionary has NO Wikidata-sourced aliases (excluding %d override entries) — "
            "publish aborted. Pass --allow-empty if intended.",
            alias_count,
        )
        return 4

    if report is not None and report.empty_categories and not args.allow_empty_categories:
        # A wrong class QID raises no error, it silently returns 0 rows (per the
        # README this happened twice). The dictionary looks "working" but that
        # category vanishes entirely.
        log.error(
            "%d categories returned 0 rows (%s) — publish aborted. Verify the class QIDs "
            "or pass --allow-empty-categories if intended.",
            len(report.empty_categories),
            ", ".join(report.empty_categories),
        )
        return 5

    if report is not None and report.limit_hit_categories and not args.allow_truncated_categories:
        # A separate flag, not shared with --allow-empty-categories: the two
        # failures call for different interventions (one a QID fix, the other
        # splitting the query) and an operator who deliberately accepts a
        # truncated category must not also silence a newly broken QID.
        log.error(
            "%d categories hit the LIMIT cap (%s) — result truncated, publish aborted. "
            "Split the query / raise the cap, or pass --allow-truncated-categories if intended.",
            len(report.limit_hit_categories),
            ", ".join(report.limit_hit_categories),
        )
        return 6

    if alias_count > args.max_aliases:
        log.error(
            "alias count exceeds the cap (%d > %d) — write aborted. Raise the sitelink floors "
            "or raise --max-aliases if intended.",
            alias_count,
            args.max_aliases,
        )
        return 3

    if args.out:
        args.out.write_text(json.dumps(doc, ensure_ascii=False, indent=2), encoding="utf-8")
        log.info("written: %s", args.out)

    if report is not None and args.report:
        args.report.write_text(
            json.dumps(_report_document(report), ensure_ascii=False, indent=2),
            encoding="utf-8",
        )
        log.info("report written: %s", args.report)

    if report is not None:
        _log_report_highlights(log, report)

    if args.redis_url:
        import redis  # needed only in this branch

        rdb = redis.from_url(args.redis_url, decode_responses=True)
        if not _shrink_ok(log, rdb, alias_count, force=args.force):
            return 7
        redis_sink.write(rdb, doc, dry_run=args.dry_run)
    elif args.dry_run:
        redis_sink.write(None, doc, dry_run=True)

    return 0


def _shrink_ok(log: logging.Logger, rdb, alias_count: int, force: bool) -> bool:
    """Stops publishing if the new dictionary is much smaller than the live one.

    The empty gate catches only TOTAL loss. A partial WDQS failure (a few
    categories empty, one category truncated) can cut a 50k dictionary to 8k
    and pass every gate; `RefreshOnce` on the read side keeps the old snapshot
    ONLY on error, not on shrinkage.

    If the meta is missing/unreadable the check is SKIPPED — there is no meta
    on first install.
    """
    meta = redis_sink.read_live_meta(rdb)
    if not meta:
        return True
    try:
        live = int(meta.get("alias_count", 0))
    except (TypeError, ValueError):
        return True
    if live <= 0:
        return True
    if alias_count >= live * (1 - SHRINK_LIMIT):
        return True
    if force:
        log.warning(
            "dictionary shrinking %d → %d (%%%.0f) but --force given, publishing",
            live,
            alias_count,
            100 * (1 - alias_count / live),
        )
        return True
    log.error(
        "dictionary shrinking %d → %d (%%%.0f > %%%.0f limit) — publish aborted. "
        "Check the empty/limit_hit categories in the report; pass --force if intended.",
        live,
        alias_count,
        100 * (1 - alias_count / live),
        100 * SHRINK_LIMIT,
    )
    return False


def _report_document(report: build_mod.BuildReport) -> dict:
    return {
        "per_category_rows": report.per_category_rows,
        "per_category_entities": report.per_category_entities,
        "empty_categories": report.empty_categories,
        "limit_hit_categories": report.limit_hit_categories,
        "dropped_invalid": report.dropped_invalid,
        "dropped_blocklist": report.dropped_blocklist,
        "pre_override_alias_count": report.pre_override_alias_count,
        "override_count": report.override_count,
        "rejected_overrides": report.rejected_overrides,
        "override_conflicts": report.override_conflicts,
        "dropped_ambiguous": report.dropped_ambiguous,
        "district_homonym_groups": report.district_homonym_groups,
        "same_label_merges": {
            c: {"qids": sorted(m["qids"]), "categories": sorted(m["categories"]), "aliases": m["aliases"]}
            for c, m in sorted(report.same_label_merges.items())
        },
    }


def _log_report_highlights(log: logging.Logger, report: build_mod.BuildReport) -> None:
    """Surface silent losses to the terminal — nobody may open the report file."""
    if report.empty_categories:
        log.warning(
            "categories that returned 0 rows (class QID may be wrong): %s",
            ", ".join(report.empty_categories),
        )
    if report.limit_hit_categories:
        log.warning("categories that hit LIMIT (result truncated): %s", ", ".join(report.limit_hit_categories))
    if report.dropped_ambiguous:
        log.warning("%d aliases dropped as ambiguous", len(report.dropped_ambiguous))
        for item in report.dropped_ambiguous[:10]:
            names = " | ".join(f"{c['canonical']}({c['category']},{c['sitelinks']})" for c in item["candidates"])
            log.warning("  ambiguous %-24s → %s", item["alias"], names)
    if report.same_label_merges:
        # Those with the SAME category are homonym suspects (a province/district
        # merge happens across different categories): they are shown first.
        ayni_kat = {c: m for c, m in report.same_label_merges.items() if len(m["categories"]) == 1}
        log.warning(
            "%d canonicals merged more than one QID (%d in the same category — homonym suspects, see same_label_merges in the report)",
            len(report.same_label_merges),
            len(ayni_kat),
        )
        for c, m in sorted(ayni_kat.items(), key=lambda x: -len(x[1]["qids"]))[:10]:
            log.warning("  same label %-28s → %s (%s)", c, ", ".join(sorted(m["qids"])), ", ".join(sorted(m["categories"])))
    if report.override_conflicts:
        log.info("%d overrides overrode the mapping coming from Wikidata", len(report.override_conflicts))
    if report.rejected_overrides:
        log.warning(
            "%d overrides REJECTED because they are in the blocklist: %s",
            len(report.rejected_overrides),
            ", ".join(report.rejected_overrides),
        )


if __name__ == "__main__":
    raise SystemExit(main())
