// Package canonicaldict reads the Wikidata-based canonical entity dictionary produced by
// python/build_canonical_dict.py from Redis and holds it in pod-local
// memory. The writing side is Python, the reading side is here.
package canonicaldict

import "serendipity/internal/trfold"

// Fold produces/looks up a dictionary key. The SOLE owner of the folding contract is
// internal/trfold; this only delegates to preserve the external API
// (callers of this package use Fold by name).
//
// Python parity is locked in the golden test in trfold.
func Fold(s string) string { return trfold.Fold(s) }
