// Package postentity extracts entities (proper nouns) from post text and
// puts the result into the form to be written to the `entities` field of the serendipity:post:data HASH.
// It does NOT TOUCH the `category` field — MMR diversity, Gamma
// freshness and the category histogram producer read that field.
package postentity

import (
	"regexp"
	"strings"
)

// MaxPromptRunes is the upper limit of the scanned post text. Long posts raise scan
// cost and do not increase entity density.
const MaxPromptRunes = 1000

// Tags is the form written to Redis. Entities are canonical names; Topics is a free topic
// label and is kept SEPARATE from the entity — proper nouns carry the Fenerbahce/Besiktas distinction,
// a topic label falls back to "football" resolution.
//
// Topics are no longer PRODUCED by dictionary scanning (the field always stays an empty array) but
// is KEPT: the serendipity:post:data.entities JSON schema must not change, otherwise
// GetEntitiesBatch and Phase 2 readers break.
type Tags struct {
	Entities []string `json:"entities"`
	Topics   []string `json:"topics"`
}

// rawURLPattern captures raw links in the text.
//
// The [LINK] mask is NOT ENOUGH: the post consumer is running in index_only mode today and
// serendipity:post:data.text carries RAW URLs (masking was the legacy full mode's
// job). The domain name of an unmasked URL is an ordinary word to the scanner:
//
//	"haber: https://www.milliyet.com.tr/spor/x"  →  [Milliyet]
//
// So EVERY post that shares a link inflates the linked publisher
// into an entity — with no evidence that the author wrote about that institution.
// Phase 2 counts this as the user's interest.
var rawURLPattern = regexp.MustCompile(`https?://\S+`)

// NormalizeText simplifies the text for scanning: raw URLs and the [LINK]
// masking residue are discarded, whitespace/line breaks are simplified.
func NormalizeText(text string) string {
	// REPLACE WITH A SPACE, DO NOT DELETE: deleting would join the tokens on both sides of the URL
	// and break the word-boundary computation
	// ("Fenerbahçe https://x/a Galatasaray" → "FenerbahçeGalatasaray",
	// both would be lost).
	clean := rawURLPattern.ReplaceAllString(text, " ")
	clean = strings.ReplaceAll(clean, "[LINK]", " ")
	return strings.Join(strings.Fields(clean), " ")
}

// PromptText makes the text scannable: normalize + MaxPromptRunes
// trimming. The name is historical (left over from the LLM prompt) but the function is the same: it fixes the scan cost of very long
// posts.
//
// TRIMMING LEAVES NO HALF TOKEN. Raw rune trimming produced a FAKE word boundary at the end of the text:
// the scanner's suffix-length condition is "if the match ends in the MIDDLE of a word
// it must be long enough", i.e. it is never applied at the END of the text.
// Since the half token left by trimming sat at the end of the text,
// it was accepted at any length:
//
//	"...aaaaa Galat"  (actually "Galatasaray")  →  [Galat...]
//
// The result is not a missing entity but a WRONG entity — and the wrong entity is written in Phase 2 to the
// user's interest profile. The fix is to discard everything after the last
// space after trimming.
//
// Cost: if the trimming landed exactly on a word boundary the last WHOLE word is also
// lost. In a 1000-rune budget, losing a single word is negligible compared to the
// risk of a wrong entity.
func PromptText(text string) string {
	clean := NormalizeText(text)
	r := []rune(clean)
	if len(r) <= MaxPromptRunes {
		return clean
	}
	clean = string(r[:MaxPromptRunes])
	// Since NormalizeText joins with a single ASCII space, byte search is
	// safe (UTF-8 continuation bytes >= 0x80).
	//
	// EDGE CASE: if there is NO space at all within the budget (a single giant token — URL
	// residue, a long hashtag chain) LastIndexByte returns -1 and would delete the text
	// entirely. In that case the trimmed text is left AS IS:
	// the risk of a half token is better than losing the whole text.
	if sp := strings.LastIndexByte(clean, ' '); sp >= 0 {
		clean = clean[:sp]
	}
	return clean
}
