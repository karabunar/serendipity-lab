"""Test of the fold() contract.

`testdata/fold_golden.json` is a fixture SHARED with `postentity.fold` on the
Go side. If a test here turns red, the Go side must turn red too; if it does
not, the two sides have diverged and lookups start silently missing.
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from canonical_dict.fold import fold

GOLDEN = Path(__file__).resolve().parent.parent / "testdata" / "fold_golden.json"


def _golden_cases():
    return json.loads(GOLDEN.read_text(encoding="utf-8"))


@pytest.mark.parametrize("case", _golden_cases(), ids=lambda c: c["in"] or "(bos)")
def test_golden_parity(case):
    assert fold(case["in"]) == case["out"]


def test_capital_i_does_not_produce_combining_dot():
    # If the order were reversed ("lower first, then fold"), Python would produce
    # a combined dotted "i̇" (U+0069 U+0307) for "İ" and diverge from Go.
    out = fold("İ")
    assert out == "i"
    assert len(out) == 1


def test_dotless_i_and_dotted_i_fold_to_same_place():
    # Separate letters in Turkish; deliberately merged for dictionary lookup,
    # because on social media the two are used interchangeably.
    assert fold("ısparta") == fold("isparta") == "isparta"


def test_idempotent():
    for case in _golden_cases():
        once = fold(case["in"])
        assert fold(once) == once
