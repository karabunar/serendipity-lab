#!/usr/bin/env python3
"""Validates the generated `canonical_dict.json` against the expected behavior.

Makes no network or Redis access; only reads files. Run BEFORE loading into
Redis. Exit code 1 on error.

Usage (from the repo root):

    python3 python/validate_canonical_dict.py canonical_dict.json \\
        --report canonical_report.json [--previous eski_canonical_dict.json]

If `--previous` is given, aliases that disappeared or changed target relative
to the old dictionary are listed; it is an error if the disappeared ratio
exceeds the threshold. Expectations:
python/canonical_dict/testdata/validation.json.
"""

from __future__ import annotations

import argparse
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

from canonical_dict.validate import GOLDEN_PATH, load, validate  # noqa: E402


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Canonical dictionary behavior check")
    parser.add_argument("dictionary", type=Path)
    parser.add_argument("--report", type=Path, help="the build's --report output")
    parser.add_argument("--previous", type=Path, help="previous canonical_dict.json to compare against")
    parser.add_argument("--golden", type=Path, default=GOLDEN_PATH)
    args = parser.parse_args(argv)

    result = validate(
        load(args.dictionary),
        load(args.golden),
        load(args.report) if args.report else None,
        load(args.previous) if args.previous else None,
    )
    for msg in result.infos:
        print("  ", msg)
    for msg in result.warnings:
        print("WARN ", msg)
    for msg in result.errors:
        print("ERROR", msg)
    print("RESULT:", "PASSED" if result.passed else f"FAILED ({len(result.errors)} errors)", f"| {len(result.warnings)} warnings")
    return 0 if result.passed else 1


if __name__ == "__main__":
    sys.exit(main())
