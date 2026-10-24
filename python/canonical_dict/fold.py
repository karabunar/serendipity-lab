"""Turkish folding — must be IDENTICAL to the Go side.

Dictionary keys are produced here, lookup happens in Go. If the two sides
normalize differently, lookups silently miss: the key is in Redis but Go
never finds it. No error message, no metric — it just looks like "the
dictionary never matches".

So `fold()` is a *contract*, not a helper function. The Go counterpart is
`Fold` in `internal/trfold/fold.go`; the two are locked together via
`testdata/fold_golden.json` (repo counterpart: `normalize_url` + sha256
golden-parity test).

Order matters: accent folding first, then lowercase. In the reverse order,
Python's `str.lower()` turns "İ" into a combined dotted "i̇" (U+0069 U+0307)
and diverges from Go's `strings.ToLower`.
"""

from __future__ import annotations

# The SAME pairs, in the SAME order, as strings.NewReplacer(...) on the Go side.
# If you change this, update trFold in Go and fold_golden.json together.
_TR_FOLD = {
    "ç": "c", "Ç": "c",
    "ğ": "g", "Ğ": "g",
    "ı": "i", "I": "i",
    "İ": "i", "i": "i",
    "ö": "o", "Ö": "o",
    "ş": "s", "Ş": "s",
    "ü": "u", "Ü": "u",
}

_TABLE = str.maketrans(_TR_FOLD)


def fold(s: str) -> str:
    """Normalizes for comparison/key generation.

    Go counterpart: strings.ToLower(trFold.Replace(s))
    """
    return s.translate(_TABLE).lower()
