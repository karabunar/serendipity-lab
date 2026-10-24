package postentity

import (
	"strings"
	"testing"
)

func TestPromptText_TrimsText(t *testing.T) {
	// Trimming fixes the scan cost; the rune count is checked, not bytes
	// (multi-byte Turkish characters would mislead).
	long := strings.Repeat("ç", MaxPromptRunes+500)
	if n := len([]rune(PromptText(long))); n != MaxPromptRunes {
		t.Fatalf("truncation did not work: %d rune", n)
	}
}

func TestPromptText_HalfTokenDropped(t *testing.T) {
	// Raw trimming used to produce a FAKE word boundary at the end of the text:
	// since the suffix-length condition is not applied at the end of the text, the trimmed head fragment
	// ("Galat") of "Galatasaray" would look like a valid match.
	// Not a missing entity, a WRONG entity.
	in := strings.Repeat("a", MaxPromptRunes-6) + " Galatasaray"
	got := PromptText(in)
	if strings.Contains(got, "Galat") {
		t.Fatalf("half token left: ...%q", got[max(0, len(got)-20):])
	}
	if !strings.HasSuffix(got, "a") {
		t.Fatalf("previous whole word is gone too: ...%q", got[max(0, len(got)-20):])
	}
}

func TestPromptText_LongTextWithoutSpacesNotEmptied(t *testing.T) {
	// Edge case: if there is no space at all within the budget (a single giant token)
	// LastIndexByte returns -1; the trimmed text must be left AS IS,
	// otherwise the text would be deleted entirely.
	in := strings.Repeat("ç", MaxPromptRunes+500)
	got := PromptText(in)
	if n := len([]rune(got)); n != MaxPromptRunes {
		t.Fatalf("text without spaces fell to %d runes, want %d", n, MaxPromptRunes)
	}
}

func TestPromptText_ShortTextUnaffected(t *testing.T) {
	// Text below the trimming threshold is NOT TOUCHED — the last word must not be cut.
	in := "Fenerbahçe kazandı Galatasaray"
	if got := PromptText(in); got != in {
		t.Fatalf("PromptText = %q, want %q", got, in)
	}
}

func TestPromptText_Idempotent(t *testing.T) {
	// Passing an already clean/trimmed text through again must give the same result.
	in := "Fenerbahçe kazandı"
	once := PromptText(in)
	if twice := PromptText(once); twice != once {
		t.Fatalf("not idempotent: %q → %q", once, twice)
	}
}

func TestNormalizeText_CollapsesWhitespace(t *testing.T) {
	got := NormalizeText("bugün\n  Sarı   Lacivert\tkazandı")
	if got != "bugün Sarı Lacivert kazandı" {
		t.Fatalf("got = %q", got)
	}
}

func TestNormalizeText_RawURLDropped(t *testing.T) {
	// In index_only mode post:data.text carries RAW URLs; the domain name is an
	// ordinary word to the scanner and every post that shares a link would inflate the publisher
	// into an entity.
	got := NormalizeText("haber: https://www.milliyet.com.tr/spor/x")
	if got != "haber:" {
		t.Fatalf("got = %q", got)
	}
	if got := NormalizeText("bak http://t.co/abc simdi"); got != "bak simdi" {
		t.Fatalf("http not caught: %q", got)
	}
}

func TestNormalizeText_URLDoesNOTMergeNeighborTokens(t *testing.T) {
	// If the URL were DELETED the tokens on both sides would join and neither would
	// match. Replacing with a space preserves the word boundary.
	got := NormalizeText("Fenerbahçe https://x.com/a Galatasaray")
	if got != "Fenerbahçe Galatasaray" {
		t.Fatalf("got = %q", got)
	}
}

func TestNormalizeText_DropsLinkMask(t *testing.T) {
	// [LINK] tokens are URL masking residue; they carry no entities and must not
	// break the word-boundary computation.
	got := NormalizeText("[LINK]Fenerbahçe[LINK] haberi")
	if got != "Fenerbahçe haberi" {
		t.Fatalf("got = %q", got)
	}
}
