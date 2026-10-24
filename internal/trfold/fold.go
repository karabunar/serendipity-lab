// Package trfold carries the SINGLE folding contract that reduces Turkish text
// to a dictionary key.
//
// Why a separate leaf package: this contract has two Go consumers —
// canonicaldict (dictionary reader) and postentity (support check). postentity
// does not want to import canonicaldict, because that package also drags in go-redis.
// Copying would mean writing the contract a third time; this package is the shared
// source whose only dependency is "strings".
//
// Counterpart on the writing side (Python): python/canonical_dict/fold.py
package trfold

import "strings"

// trFold reduces Turkish accented letters to their ASCII counterparts.
//
// ⚠️ CONTRACT: it must carry EXACTLY the same pairs as _TR_FOLD in
// python/canonical_dict/fold.py. If they diverge, dictionary lookups
// silently miss — the key exists in Redis, this side cannot find it, and no error
// or metric is produced. Lock: python/canonical_dict/testdata/fold_golden.json
var trFold = strings.NewReplacer(
	"ç", "c", "Ç", "c",
	"ğ", "g", "Ğ", "g",
	"ı", "i", "I", "i",
	"İ", "i", "i", "i",
	"ö", "o", "Ö", "o",
	"ş", "s", "Ş", "s",
	"ü", "u", "Ü", "u",
)

// Fold produces/looks up a dictionary key. The order is part of the contract: FIRST
// accent folding, THEN lowercase. In the reverse order "İ" yields a combining-dot
// "i̇" (U+0069 U+0307) and diverges from the Python side.
//
// ⚠️ The one known deviation — Greek final sigma: Go's strings.ToLower maps "Σ"
// to "σ" regardless of position, while Python's str.lower() yields "ς" at the end of
// a word ("ΑΣ" → Go "ασ", Python "ας"). The fixture deliberately omits this:
// adding it would break the Python test, and it is practically irrelevant for entity names (Turkish/English
// labels). If Greek aliases start entering the dictionary,
// this deviation must be resolved first.
func Fold(s string) string {
	return strings.ToLower(trFold.Replace(s))
}
