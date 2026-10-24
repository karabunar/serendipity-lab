package canonicaldict

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// matchCache returns a Cache filled with the given dictionary.
func matchCache(t *testing.T, aliases map[string]string) *Cache {
	t.Helper()
	c := NewCache(&fakeLoader{data: aliases}, time.Hour, testLogger())
	c.Start(context.Background())
	t.Cleanup(c.Stop)
	return c
}

func TestMatch_SimpleMatch(t *testing.T) {
	c := matchCache(t, map[string]string{"fenerbahce": "Fenerbahçe"})
	if got := c.Match("bugün Fenerbahçe kazandı"); !reflect.DeepEqual(got, []string{"Fenerbahçe"}) {
		t.Fatalf("Match = %v", got)
	}
}

func TestMatch_TurkishFoldingApplied(t *testing.T) {
	// The text is unaccented, the dictionary key is folded: both are "fenerbahce".
	c := matchCache(t, map[string]string{"fenerbahce": "Fenerbahçe"})
	for _, text := range []string{"FENERBAHÇE", "fenerbahce", "Fenerbahçe"} {
		if got := c.Match(text); len(got) != 1 {
			t.Fatalf("Match(%q) = %v", text, got)
		}
	}
}

func TestMatch_SuffixedWordIsCaught(t *testing.T) {
	// Turkish agglutination: the match may end in the MIDDLE of a word.
	c := matchCache(t, map[string]string{
		"fenerbahce":  "Fenerbahçe",
		"galatasaray": "Galatasaray",
		"istanbul":    "İstanbul",
	})
	cases := map[string]string{
		"Fenerbahçeli taraftar":  "Fenerbahçe",
		"Galatasaray'ın golü":    "Galatasaray",
		"İstanbul'da yağmur":     "İstanbul",
		"fenerbahcelilerin sesi": "Fenerbahçe",
	}
	for text, want := range cases {
		got := c.Match(text)
		if len(got) != 1 || got[0] != want {
			t.Fatalf("Match(%q) = %v, want [%s]", text, got, want)
		}
	}
}

func TestMatch_SuffixAfterApostropheDoesNotMatch(t *testing.T) {
	// Since the apostrophe counts as a BOUNDARY the position after it also became a fresh
	// word start; as a result Turkish suffixes were scanned as aliases
	// in their own right. The dictionary has 2-rune aliases (MIN_ALIAS_LEN=2)
	// and such a name would fire on EVERY possessive construction in the corpus.
	c := matchCache(t, map[string]string{
		"besiktas": "Beşiktaş",
		"ta":       "TA-Org",
		"ye":       "YE-Org",
		"nin":      "NIN-Org",
		"ali":      "Ali Koç",
	})
	cases := map[string][]string{
		"Beşiktaş'ta güzel maç": {"Beşiktaş"},
		"Türkiye'ye gitti":      {},
		"Ali'nin evi":           {"Ali Koç"},
	}
	for text, want := range cases {
		got := c.Match(text)
		if len(got) != len(want) {
			t.Fatalf("Match(%q) = %v, want %v", text, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("Match(%q) = %v, want %v", text, got, want)
			}
		}
	}
}

func TestMatch_ApostropheDoesNOTBreakMainMatch(t *testing.T) {
	// The rule only rejects suffix continuation; a match starting BEFORE the apostrophe
	// (the reason the rule exists) works as is. The curly quote and acute
	// accent variants behave the same.
	c := matchCache(t, map[string]string{"galatasaray": "Galatasaray"})
	for _, text := range []string{
		"Galatasaray'ın golü",
		"Galatasaray’ın golü",
		"Galatasaray´ın golü",
	} {
		got := c.Match(text)
		if len(got) != 1 || got[0] != "Galatasaray" {
			t.Fatalf("Match(%q) = %v, want [Galatasaray]", text, got)
		}
	}
}

func TestMatch_ApostropheAsQuoteDoesNotBlock(t *testing.T) {
	// The rule looks at the word + apostrophe + suffix sequence. In a quote, BEFORE the apostrophe
	// is a space or the start of text; the word start must be PRESERVED there.
	c := matchCache(t, map[string]string{"besiktas": "Beşiktaş"})
	for _, text := range []string{"maçta 'Beşiktaş' kazandı", "'Beşiktaş' kazandı"} {
		got := c.Match(text)
		if len(got) != 1 || got[0] != "Beşiktaş" {
			t.Fatalf("Match(%q) = %v, want [Beşiktaş]", text, got)
		}
	}
}

func TestMatch_ShortNameDoesNotMatchInsideWord(t *testing.T) {
	// If "ali" (3 runes) matches inside "alibaba" a wrong entity is produced
	// in every text. If a suffix follows the MinSuffixMatchRunes condition kicks in.
	c := matchCache(t, map[string]string{"ali": "Ali Koç", "fb": "Fenerbahçe"})
	for _, text := range []string{"alibaba sirketi", "fbi raporu"} {
		if got := c.Match(text); len(got) != 0 {
			t.Fatalf("Match(%q) = %v, should be empty", text, got)
		}
	}
}

func TestMatch_ShortNameMatchesAtWordBoundary(t *testing.T) {
	// The same short name is ACCEPTED if it ends at a boundary.
	c := matchCache(t, map[string]string{"fb": "Fenerbahçe"})
	for _, text := range []string{"FB kazandı", "hadi FB!", "bugün fb"} {
		got := c.Match(text)
		if len(got) != 1 || got[0] != "Fenerbahçe" {
			t.Fatalf("Match(%q) = %v", text, got)
		}
	}
}

func TestMatch_DoesNOTStartMidWord(t *testing.T) {
	// Even if the word "spor" occurs inside "Trabzonspor" the match must
	// START at a word start.
	c := matchCache(t, map[string]string{"spor": "Spor"})
	if got := c.Match("Trabzonspor maci"); len(got) != 0 {
		t.Fatalf("Match = %v, should be empty", got)
	}
}

func TestMatch_LongestMatchWins(t *testing.T) {
	c := matchCache(t, map[string]string{
		"fener":      "Fener (semt)",
		"fenerbahce": "Fenerbahçe",
	})
	got := c.Match("Fenerbahçe kazandı")
	if len(got) != 1 || got[0] != "Fenerbahçe" {
		t.Fatalf("Match = %v, longest match must win", got)
	}
}

func TestMatch_MatchesDoNotOverlap(t *testing.T) {
	// after "fenerbahce" matches, the "fener" inside it must NOT match AGAIN.
	c := matchCache(t, map[string]string{
		"fener":      "Fener",
		"fenerbahce": "Fenerbahçe",
		"bahce":      "Bahçe",
	})
	got := c.Match("fenerbahce")
	if len(got) != 1 || got[0] != "Fenerbahçe" {
		t.Fatalf("Match = %v", got)
	}
}

func TestMatch_MultiWordAlias(t *testing.T) {
	c := matchCache(t, map[string]string{
		"sari lacivert": "Fenerbahçe",
		"lacivert":      "Lacivert",
	})
	got := c.Match("bugün sarı lacivert bayrak")
	if len(got) != 1 || got[0] != "Fenerbahçe" {
		t.Fatalf("Match = %v, multi-word name must be the longest match", got)
	}
}

func TestMatch_MultipleEntities(t *testing.T) {
	c := matchCache(t, map[string]string{
		"fenerbahce":  "Fenerbahçe",
		"galatasaray": "Galatasaray",
	})
	got := c.Match("Fenerbahçe Galatasaray derbisi")
	if !reflect.DeepEqual(got, []string{"Fenerbahçe", "Galatasaray"}) {
		t.Fatalf("Match = %v", got)
	}
}

func TestMatch_SameEntityTwiceReturnsTwice(t *testing.T) {
	// Deduplication is the CALLER's job; Match returns raw passes so the caller can
	// do counting/weighting.
	c := matchCache(t, map[string]string{"fenerbahce": "Fenerbahçe"})
	got := c.Match("Fenerbahçe ve yine Fenerbahçe")
	if len(got) != 2 {
		t.Fatalf("Match = %v, two passes expected", got)
	}
}

func TestMatch_EmptyDictionaryReturnsEmpty(t *testing.T) {
	c := matchCache(t, map[string]string{})
	if got := c.Match("Fenerbahçe kazandı"); len(got) != 0 {
		t.Fatalf("Match = %v", got)
	}
}

func TestMatch_EmptyTextReturnsEmpty(t *testing.T) {
	c := matchCache(t, map[string]string{"fenerbahce": "Fenerbahçe"})
	if got := c.Match(""); len(got) != 0 {
		t.Fatalf("Match = %v", got)
	}
}

func TestMatch_BeforeStartReturnsEmpty(t *testing.T) {
	// Cold-start: Match must not panic before the dictionary is loaded.
	c := NewCache(&fakeLoader{data: map[string]string{"fb": "Fenerbahçe"}}, time.Hour, testLogger())
	if got := c.Match("FB kazandı"); len(got) != 0 {
		t.Fatalf("Match = %v, must be empty before Start", got)
	}
}

func TestMatch_VeryLongAliasIsCapped(t *testing.T) {
	// The MaxAliasRunes cap limits the scan's inner loop; an alias longer than the cap
	// does not match but the scan does not blow up either.
	long := ""
	for range MaxAliasRunes + 10 {
		long += "a"
	}
	c := matchCache(t, map[string]string{long: "Uzun"})
	if got := c.Match(long); len(got) != 0 {
		t.Fatalf("Match = %v, name above the cap must not match", got)
	}
}

func TestMatch_UsesCurrentDictionaryAfterRefresh(t *testing.T) {
	l := &fakeLoader{data: map[string]string{"fb": "Eski"}}
	c := NewCache(l, time.Hour, testLogger())
	c.Start(context.Background())
	defer c.Stop()

	l.mu.Lock()
	l.data = map[string]string{"fb": "Yeni"}
	l.mu.Unlock()
	c.RefreshOnce(context.Background())

	got := c.Match("FB")
	if len(got) != 1 || got[0] != "Yeni" {
		t.Fatalf("Match = %v", got)
	}
}

func TestMatch_ShortTailedNameDoesNOTSwallowNextWord(t *testing.T) {
	// Regression: the length condition looked at the WHOLE match, so
	// the "real madrid b" (13 rune) condition passed and swallowed Barcelona's "B";
	// the remaining "arcelonayi" was not a word start so
	// Barcelona did not match at all.
	c := matchCache(t, map[string]string{
		"real madrid b": "Real Madrid Castilla",
		"real madrid":   "Real Madrid CF",
		"barcelona":     "Barselona",
	})
	got := c.Match("Real Madrid Barcelona'yı yendi")
	want := []string{"Real Madrid CF", "Barselona"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Match = %v, wanted %v", got, want)
	}
}

func TestMatch_ShortTailedNameMATCHESAtWordBoundary(t *testing.T) {
	// The rule only restricts ending in the MIDDLE of a word; when the name occurs on its own
	// a short tail is not a problem.
	c := matchCache(t, map[string]string{"real madrid b": "Real Madrid Castilla"})
	if got := c.Match("Real Madrid B kazandı"); !reflect.DeepEqual(got, []string{"Real Madrid Castilla"}) {
		t.Fatalf("Match = %v", got)
	}
}

func TestMatch_LongLastWordCONTINUES_TakingSuffix(t *testing.T) {
	// If the last word of a multi-word name is long enough the Turkish suffix must still be
	// caught — the fix must not narrow this behavior.
	c := matchCache(t, map[string]string{"borsa istanbul": "Borsa İstanbul"})
	if got := c.Match("Borsa İstanbulda islem"); !reflect.DeepEqual(got, []string{"Borsa İstanbul"}) {
		t.Fatalf("Match = %v", got)
	}
}
