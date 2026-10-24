package postentity

import (
	"reflect"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"serendipity/internal/entitylink"
)

// sahteLinker is the test twin of canonicaldict.Cache.
type sahteLinker struct{ alias map[string]string }

func (s sahteLinker) Canonical(term string) (string, bool) {
	v, ok := s.alias[normalizeKey(term)]
	return v, ok
}

// containsWord looks for a substring that starts at a word boundary in the folded text.
// A simplified test twin of canonicaldict.Match — it HAS NO PLACE
// IN PRODUCTION CODE, the real scan lives in canonicaldict.
func containsWord(text, sub string) bool {
	idx := strings.Index(text, sub)
	return idx == 0 || (idx > 0 && !isWordByte(text[idx-1]))
}

func isWordByte(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

func (s sahteLinker) Match(text string) []string {
	var out []string
	for a, c := range s.alias {
		if containsWord(normalizeKey(text), a) {
			out = append(out, c)
		}
	}
	return out
}

func linker() sahteLinker {
	return sahteLinker{alias: map[string]string{
		"fenerbahce": "Fenerbahçe",
		"erdogan":    "Recep Tayyip Erdoğan",
		"gazze":      "Gazze",
	}}
}

type locatedTestLinker struct {
	sahteLinker
	matches []entitylink.LocatedMatch
}

func (l locatedTestLinker) MatchLocated(string) []entitylink.LocatedMatch {
	return l.matches
}

func TestMergeHybridAtText_SuppressionOnlyIfInsideSpan(t *testing.T) {
	text := "Recep Tayyip Erdoğan konuştu. Bilal Erdoğan açıklama yaptı."
	byteStart := strings.Index(text, "Bilal")
	start := len([]rune(text[:byteStart]))
	end := start + len([]rune("Bilal Erdoğan"))
	link := locatedTestLinker{
		sahteLinker: linker(),
		matches: []entitylink.LocatedMatch{{
			Canonical: "Recep Tayyip Erdoğan", Start: 13, End: 20,
		}, {
			Canonical: "Recep Tayyip Erdoğan", Start: start + 6, End: end,
		}},
	}

	got := MergeHybridAtText(text,
		[]string{"Recep Tayyip Erdoğan"},
		[]NERSpan{{Text: "Bilal Erdoğan", Start: start, End: end}}, link)
	if len(got) != 2 || got[0] != "Recep Tayyip Erdoğan" || got[1] != "bilal erdogan" {
		t.Fatalf("legitimate dictionary match outside the span was lost: %v", got)
	}
}

func TestMergeHybridAtText_SuppressionDropsSingleMatchInsideSpan(t *testing.T) {
	text := "Bilal Erdoğan açıklama yaptı."
	end := len([]rune("Bilal Erdoğan"))
	link := locatedTestLinker{
		sahteLinker: linker(),
		matches: []entitylink.LocatedMatch{{
			Canonical: "Recep Tayyip Erdoğan", Start: 6, End: end,
		}},
	}

	got := MergeHybridAtText(text,
		[]string{"Recep Tayyip Erdoğan"},
		[]NERSpan{{Text: "Bilal Erdoğan", Start: 0, End: end}}, link)
	if len(got) != 1 || got[0] != "bilal erdogan" {
		t.Fatalf("wrong canonical inside the span was not dropped: %v", got)
	}
}

// Rule 1: the NER span matches an alias EXACTLY → the dictionary's canonical.
func TestMergeHybrid_Rule1_ExactMatch(t *testing.T) {
	got := MergeHybrid(nil, []NERSpan{{Text: "Fenerbahçe", Label: "ORG"}}, linker())
	want := []string{"Fenerbahçe"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

// Rule 2: attached to an alias INSIDE the span → the NER's surface wins.
// This is the rule that cuts identity confusion: "Bilal Erdogan" must NOT become Erdogan.
//
// ⚠️ THE INPUT MUST BE REALISTIC. This test was first written with dictNames=nil
// and in that form it TESTED NOTHING: since the dictionary leg was empty the rule 4
// loop never ran, and the same output was produced whether suppression existed or not.
// In reality the dictionary scans THE SAME TEXT independently (finds the alias "erdogan" inside
// "Bilal Erdoğan konuştu") and returns its own canonical — the identity
// confusion that must be cut is exactly this.
func TestMergeHybrid_Rule2_CutsCollapseInside(t *testing.T) {
	got := MergeHybrid(
		[]string{"Recep Tayyip Erdoğan"}, // the inner alias the dictionary found in the SAME text
		[]NERSpan{{Text: "Bilal Erdoğan", Label: "PERSON"}},
		linker())
	want := []string{"bilal erdogan"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("identity mix-up was not cut off: got %v want %v", got, want)
	}
}

// Suppression is SELECTIVE: only the canonical triggered by rule 2 drops, the UNRELATED canonicals the dictionary
// found in the same post stay.
//
// This test catches a regression that implements suppression as "throw everything away" —
// the test above alone CANNOT catch it (there was only one canonical
// to suppress there).
func TestMergeHybrid_Rule2_SuppressionIsSelective(t *testing.T) {
	got := MergeHybrid(
		[]string{"Recep Tayyip Erdoğan", "Gazze"},
		[]NERSpan{{Text: "Bilal Erdoğan", Label: "PERSON"}},
		linker())
	want := []string{"Gazze", "bilal erdogan"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

// Rule 2 suppression does NOT AFFECT rule 1 (EXACT match): if the whole span is an
// alias the dictionary's canonical is already the right key.
func TestMergeHybrid_Rule1_UnaffectedBySuppression(t *testing.T) {
	got := MergeHybrid(
		[]string{"Fenerbahçe"},
		[]NERSpan{{Text: "Fenerbahçe", Label: "ORG"}},
		linker())
	want := []string{"Fenerbahçe"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

// I1: the output is in PRIORITY order — linked canonicals first (rule 1+4),
// then raw NER surfaces (rule 2+3). Since the caller trims MaxEntities from the TAIL, this order
// keeps rule 4's hashtag rescue alive.
func TestMergeHybrid_LinkedCanonicalsComeFirst(t *testing.T) {
	got := MergeHybrid(
		[]string{"Gazze"},                        // rule 4 → canonical
		[]NERSpan{{Text: "TÜGVA", Label: "ORG"}}, // rule 3 → raw surface
		linker())
	want := []string{"Gazze", "tugva"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("priority order broken: got %v want %v", got, want)
	}
}

// A7 (spec §10): if a raw NER surface and a dictionary canonical fold onto the SAME key
// the winner is the CANONICAL. The dictionary builder normally makes the canonical its own
// alias, but when it does not (here: the linker does not recognize "TÜGVA", the dictionary
// still returned "TÜGVA") the span loop ran first, so the raw surface stole the shared
// `seen` key and pushed the canonical into the low-priority bucket:
// ["Gazze","tugva"]. The raw surface took the canonical name's place and, since trimming cuts
// from the tail, I1's ordering rationale collapsed in exactly this case.
func TestMergeHybrid_CanonicalNotCapturedByRawSurface(t *testing.T) {
	got := MergeHybrid(
		[]string{"TÜGVA", "Gazze"},
		[]NERSpan{{Text: "TÜGVA", Label: "ORG"}},
		linker())
	want := []string{"TÜGVA", "Gazze"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("canonical lost to the raw surface: got %v want %v", got, want)
	}
}

// Rule 3: not in the dictionary at all → the NER's surface (raw in Plan A, no linker).
func TestMergeHybrid_Rule3_DictionaryLessSurface(t *testing.T) {
	got := MergeHybrid(nil, []NERSpan{{Text: "TÜGVA", Label: "ORG"}}, linker())
	want := []string{"tugva"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

// Rule 4: the dictionary found it, NER never saw it → canonical. Hashtag rescue.
func TestMergeHybrid_Rule4_HashtagRescue(t *testing.T) {
	got := MergeHybrid([]string{"Gazze"}, nil, linker())
	want := []string{"Gazze"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("hashtag rescue did not work: got %v want %v", got, want)
	}
}

// Rule 4 must not add a second time what NER ALREADY found.
func TestMergeHybrid_Rule4_DoesNotAddDuplicate(t *testing.T) {
	got := MergeHybrid([]string{"Fenerbahçe"},
		[]NERSpan{{Text: "Fenerbahçe", Label: "ORG"}}, linker())
	if len(got) != 1 {
		t.Fatalf("duplicate added: %v", got)
	}
}

// If the linker is nil (flag not off, dictionary empty): NER surfaces pass raw.
func TestMergeHybrid_NoLinkerDoesNotBreak(t *testing.T) {
	got := MergeHybrid(nil, []NERSpan{{Text: "Fenerbahçe", Label: "ORG"}}, nil)
	want := []string{"fenerbahce"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

// ─── L1: normalizeKey characterization ────────────────────────────────────
//
// These two tests DO NOT CHANGE BEHAVIOR, they PIN the existing behavior. Rationale:
// the whole measurement was taken with exactly this normalization; if trimming
// changes, none of the measured figures remain valid.

// Everything after the apostrophe is discarded — Turkish suffix separation.
func TestNormalizeKey_DropsApostropheSuffix(t *testing.T) {
	if got := normalizeKey("Fenerbahçe'nin"); got != "fenerbahce" {
		t.Fatalf("normalizeKey = %q, want %q", got, "fenerbahce")
	}
	// The curly apostrophe (U+2019) is in the same class too.
	if got := normalizeKey("Fenerbahçe’nin"); got != "fenerbahce" {
		t.Fatalf("apostrophe: normalizeKey = %q, want %q", got, "fenerbahce")
	}
}

// KNOWN LIMIT: trimming discards not only the suffix but EVERYTHING AFTER the apostrophe.
// In a multi-word surface ("X'in Y") the second word is lost too.
//
// Accepted today: surfaces are short proper nouns anyway and the measurement
// was taken with this behavior. The test prevents this limit from changing SILENTLY — if it is widened
// this breaks and reminds that the measurement must be redone.
func TestNormalizeKey_DropsAllTextAfterApostrophe(t *testing.T) {
	if got := normalizeKey("X'in Y"); got != "x" {
		t.Fatalf("known limit changed: normalizeKey = %q, want %q", got, "x")
	}
}

// ─── A10: MergeRule denominator ───────────────────────────────────────────
//
// The metric's stated purpose is the "rule2 RATIO"; it could not be computed without the denominator (span count =
// rule1 + rule2_span_wins + rule3). Rule 4
// (hashtag rescue) is also counted separately: the share the dictionary adds to NER.
func kuralSayaci(rule string) float64 {
	return testutil.ToFloat64(MergeRule.WithLabelValues(rule))
}

func TestMergeRule_AllSpansCountedUnderOneRule(t *testing.T) {
	r1, r2, r3 := kuralSayaci("rule1_linked"), kuralSayaci("rule2_span_wins"), kuralSayaci("rule3_unknown_surface")
	MergeHybrid(nil, []NERSpan{
		{Text: "Fenerbahçe"},    // rule 1
		{Text: "Bilal Erdoğan"}, // rule 2 (inner alias "erdogan")
		{Text: "TÜGVA"},         // rule 3
	}, linker())
	if d := kuralSayaci("rule1_linked") - r1; d != 1 {
		t.Errorf("rule1_linked increase = %v, want 1", d)
	}
	if d := kuralSayaci("rule2_span_wins") - r2; d != 1 {
		t.Errorf("rule2_span_wins increase = %v, want 1", d)
	}
	if d := kuralSayaci("rule3_unknown_surface") - r3; d != 1 {
		t.Errorf("rule3_unknown_surface increase = %v, want 1", d)
	}
}

// Rule 4 is counted ONLY when the dictionary adds a canonical NER did not see;
// one NER already found (duplicate) and one rule 2 suppressed are not counted.
func TestMergeRule_Rule4CountsOnlyDictionarySingleAdd(t *testing.T) {
	r4 := kuralSayaci("rule4_dict_only")
	MergeHybrid(
		[]string{"Gazze", "Fenerbahçe", "Recep Tayyip Erdoğan"},
		[]NERSpan{{Text: "Fenerbahçe"}, {Text: "Bilal Erdoğan"}},
		linker())
	if d := kuralSayaci("rule4_dict_only") - r4; d != 1 {
		t.Errorf("rule4_dict_only increase = %v, want 1 (Gazze only)", d)
	}
}

// The series set is exactly five rules: pre-seeding creates these five labels (a PromQL alarm
// built on a nonexistent series CANNOT fire and the denominator stays
// missing); more than that shows a new label was added without being seeded.
func TestMergeRule_SeriesArePreSeeded(t *testing.T) {
	if n := testutil.CollectAndCount(MergeRule); n != 5 {
		t.Fatalf("MergeRule %d series carried, want 5", n)
	}
}
