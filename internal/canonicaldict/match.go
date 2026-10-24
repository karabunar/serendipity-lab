package canonicaldict

import (
	"unicode"

	"serendipity/internal/entitylink"
)

// MinSuffixMatchRunes is the shortest length at which a match is allowed to END IN THE MIDDLE OF A WORD.
// shortest length.
//
// Why needed: the rule "START at a word boundary" alone is not enough. In the text "Alibaba"
// position 0 is a word start; if the dictionary has "ali" it would match
// and stick a wrong entity onto every text. But putting the boundary condition at the END too
// is unacceptable in Turkish: "Fenerbahceli", "Galatasaray'in", "Istanbul'da"
// are legitimate passes.
//
// Compromise: if the match continues with a suffix it must be long enough. A 3-letter
// stem found inside a longer word is almost always a coincidence;
// a 10-letter stem almost never.
const MinSuffixMatchRunes = 5

// MaxAliasRunes is the inner loop cap of the scan. A single absurdly long
// alias lengthens the attempt made at EVERY position; the cap fixes this cost.
// Aliases longer than this do not match.
const MaxAliasRunes = 64

// minAliasRunes is the shortest alias length of the scan.
//
// ⚠️ This value is NO LONGER THE SAME as MIN_ALIAS_LEN in the builder and must not be.
// build.py was split into two thresholds: MIN_ALIAS_LEN=3 for aliases DERIVED
// from Wikidata (2-letter surfaces produced many matches that an
// independent NER did not support —
// the whole peak was ISO country codes), MIN_OVERRIDE_ALIAS_LEN=2 for curated overrides.json entries.
// Since the dictionary can still carry curated 2-letter keys the SCAN FLOOR MUST STAY AT 2 — if raised to 3
// hand-added abbreviations like "fb" sit in Redis but this side cannot find them,
// and neither an error
// nor a metric is produced.
const minAliasRunes = 2

// dictSnapshot is the dictionary snapshot swapped atomically. The map and the
// scan parameter change TOGETHER — if kept separate an inconsistent moment
// could occur during refresh.
type dictSnapshot struct {
	aliases  map[string]string
	maxRunes int // the longest alias in the dictionary (capped by MaxAliasRunes)
}

func newSnapshot(aliases map[string]string) *dictSnapshot {
	maxRunes := 0
	for a := range aliases {
		if n := len([]rune(a)); n > maxRunes {
			maxRunes = n
		}
	}
	if maxRunes > MaxAliasRunes {
		maxRunes = MaxAliasRunes
	}
	return &dictSnapshot{aliases: aliases, maxRunes: maxRunes}
}

// Match returns the canonical entity names occurring in the text IN PASS ORDER.
//
// No deduplication is done — the caller may want to know how many times the same entity
// occurred.
//
// Rule:
//   - a match STARTS at a word boundary (start of text or after a rune
//     that is not a letter/digit)
//   - it may end anywhere (Turkish is agglutinative), but if it ends in the middle of a word
//     the MinSuffixMatchRunes condition applies
//   - at each position the LONGEST match wins
//   - matches DO NOT OVERLAP: the matched region is skipped
//
// Cost: number of word starts × maxRunes map lookups. In a typical
// post (~500 runes, ~80 words) ~2,400 lookups ≈ 0.1 ms. No extra memory —
// the dictionary map already in memory is used.
func (c *Cache) Match(text string) []string {
	matches := c.MatchLocated(text)
	out := make([]string, 0, len(matches))
	for _, match := range matches {
		out = append(out, match.Canonical)
	}
	return out
}

// MatchLocated returns dictionary matches with their rune ranges. In the entity hybrid
// path it is used to tell a match inside the NER span apart from the same
// canonical elsewhere in the text.
func (c *Cache) MatchLocated(text string) []entitylink.LocatedMatch {
	snap := c.snapshot.Load().(*dictSnapshot)
	if len(snap.aliases) == 0 || text == "" {
		return nil
	}

	r := []rune(Fold(text))
	var out []entitylink.LocatedMatch

	for i := 0; i < len(r); i++ {
		if !isWordStart(r, i) {
			continue
		}
		maxLen := snap.maxRunes
		if rem := len(r) - i; maxLen > rem {
			maxLen = rem
		}
		// Longest to shortest: the first one found is the LONGEST match.
		for l := maxLen; l >= minAliasRunes; l-- {
			canonical, ok := snap.aliases[string(r[i:i+l])]
			if !ok {
				continue
			}
			// If it ends in the middle of a word the length condition applies. The criterion is not the
			// WHOLE match but its LAST WORD — the Turkish suffix attaches there.
			if i+l < len(r) && isWordRune(r[i+l]) &&
				lastTokenRunes(r[i:i+l]) < MinSuffixMatchRunes {
				continue
			}
			out = append(out, entitylink.LocatedMatch{Canonical: canonical, Start: i, End: i + l})
			i += l - 1 // together with the outer loop's i++ skips the matched region
			break
		}
	}
	return out
}

// lastTokenRunes is the rune count of the match's LAST word.
//
// Why total length is not enough: the length condition rests on the intuition "if the stem is long enough
// finding it inside is not a coincidence", but in a multi-word
// alias that intuition holds only for the last word, which CARRIES THE SUFFIX.
// Looking at total length, the "real madrid b" (13 rune) condition passed easily
// and the measured result was this:
//
//	"Real Madrid Barcelona'yi yendi" → [Real Madrid Castilla]
//
// The match swallows Barcelona's "B", the remaining "arcelonayi" is no longer a
// word start so Barcelona did NOT match AT ALL. The same trap applied to every name with a short tail such as "adalet
// sk" + "andali", "aberdeen fc" + "..." (the dictionary has
// 1,043 aliases whose last token is <=2 chars).
func lastTokenRunes(m []rune) int {
	n := 0
	for j := len(m) - 1; j >= 0 && isWordRune(m[j]); j-- {
		n++
	}
	return n
}

// isWordStart tells whether position i is a word start.
//
// APOSTROPHE EXCEPTION: isWordRune counts the apostrophe as a boundary — this is
// deliberate, so that "Galatasaray'in" can match. But the same rule also made the position AFTER
// the apostrophe a fresh word start, i.e. Turkish possessive/locative SUFFIXES were scanned
// as aliases in their own right. Since the dictionary has MIN_ALIAS_LEN=2 there are 2-rune
// aliases and the actual measured results were:
// sonuclar sunlardi:
//
//	"Besiktas'ta"  → [Besiktas, TA-Org]
//	"Turkiye'ye"   → [YE]
//	"Ali'nin"      → [NIN]
//
// Such an alias fires on EVERY possessive construction in the corpus. Rule: the position after the
// apostrophe, if a word rune PRECEDES it, is a SUFFIX
// CONTINUATION, not a new word.
//
// An apostrophe used as a quote is NOT AFFECTED ('Besiktas' kazandi):
// there, before the apostrophe is a space or the start of text. The main match is also
// unaffected — "Galatasaray'in" starts at position 0.
func isWordStart(r []rune, i int) bool {
	if !isWordRune(r[i]) {
		return false
	}
	if i == 0 {
		return true
	}
	if isWordRune(r[i-1]) {
		return false
	}
	if i >= 2 && isApostrophe(r[i-1]) && isWordRune(r[i-2]) {
		return false // word + apostrophe + SUFFIX
	}
	return true
}

// isApostrophe is the apostrophe variants used as a suffix separator in Turkish text.
// The plain quote (U+0027) comes from the keyboard, the curly quote (U+2019) from mobile
// autocorrect, the acute accent (U+00B4) from a wrong keyboard layout.
func isApostrophe(c rune) bool {
	return c == '\'' || c == '’' || c == '´'
}

// isWordRune counts letters and digits as word characters. The apostrophe
// (Galatasaray'in) and punctuation count as BOUNDARIES.
func isWordRune(c rune) bool {
	return unicode.IsLetter(c) || unicode.IsDigit(c)
}
