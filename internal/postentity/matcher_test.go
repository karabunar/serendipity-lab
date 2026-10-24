package postentity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func exLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// fakeMatcher returns the values of the keys that occur in the text.
type fakeMatcher map[string]string

func (f fakeMatcher) Match(text string) []string {
	var out []string
	low := strings.ToLower(text)
	for k, v := range f {
		if strings.Contains(low, k) {
			out = append(out, v)
		}
	}
	return out
}

func TestExtractBatch_MatchProducesJSON(t *testing.T) {
	e := NewExtractor(fakeMatcher{"fenerbahçe": "Fenerbahçe"}, DefaultConfig(), exLogger())
	got, _ := e.ExtractBatch(context.Background(), map[string]string{"p1": "bugün Fenerbahçe kazandı"})

	var tags Tags
	if err := json.Unmarshal([]byte(got["p1"]), &tags); err != nil {
		t.Fatal(err)
	}
	if len(tags.Entities) != 1 || tags.Entities[0] != "Fenerbahçe" {
		t.Fatalf("tags = %+v", tags)
	}
	// topics are no longer produced but the field is KEPT — the JSON schema must not change.
	if tags.Topics == nil {
		t.Fatal("topics must not be null, must be an empty array")
	}
}

func TestExtractBatch_NoMatchEmptyResultIS_WRITTEN(t *testing.T) {
	// If an empty result is not written the post is reprocessed every round.
	e := NewExtractor(fakeMatcher{}, DefaultConfig(), exLogger())
	got, _ := e.ExtractBatch(context.Background(), map[string]string{"p1": "hicbir ozel ad yok"})
	if got["p1"] != `{"entities":[],"topics":[]}` {
		t.Fatalf("empty result = %q", got["p1"])
	}
}

func TestExtractBatch_Deduplicates(t *testing.T) {
	// Match returns raw passes; deduplication is done HERE.
	m := fakeMatcher{"fb": "Fenerbahçe"}
	e := NewExtractor(m, DefaultConfig(), exLogger())
	got, _ := e.ExtractBatch(context.Background(), map[string]string{"p1": "fb ve yine fb"})

	var tags Tags
	if err := json.Unmarshal([]byte(got["p1"]), &tags); err != nil {
		t.Fatal(err)
	}
	if len(tags.Entities) != 1 {
		t.Fatalf("deduplication did not work: %v", tags.Entities)
	}
}

func TestExtractBatch_MaxEntitiesApplied(t *testing.T) {
	e := NewExtractor(fakeMatcher{"a": "A", "b": "B", "c": "C"},
		Config{MaxEntities: 2, Workers: 1}, exLogger())
	got, _ := e.ExtractBatch(context.Background(), map[string]string{"p1": "a b c"})

	var tags Tags
	if err := json.Unmarshal([]byte(got["p1"]), &tags); err != nil {
		t.Fatal(err)
	}
	if len(tags.Entities) != 2 {
		t.Fatalf("MaxEntities not applied: %v", tags.Entities)
	}
}

func TestExtractBatch_TrimmedTextIsScanned(t *testing.T) {
	// A name beyond MaxPromptRunes must not be scanned — trimming was kept so that scan cost
	// stays fixed.
	tail := "zonguldakspor"
	text := strings.Repeat("a", MaxPromptRunes+50) + " " + tail
	e := NewExtractor(fakeMatcher{tail: "Zonguldakspor"}, DefaultConfig(), exLogger())
	got, _ := e.ExtractBatch(context.Background(), map[string]string{"p1": text})

	var tags Tags
	if err := json.Unmarshal([]byte(got["p1"]), &tags); err != nil {
		t.Fatal(err)
	}
	if len(tags.Entities) != 0 {
		t.Fatalf("scanned beyond the truncation limit: %v", tags.Entities)
	}
}

func TestExtractBatch_EmptyInputDoesNotCallMatcher(t *testing.T) {
	called := false
	e := NewExtractor(matcherFunc(func(string) []string { called = true; return nil }),
		DefaultConfig(), exLogger())
	if got, _ := e.ExtractBatch(context.Background(), nil); len(got) != 0 {
		t.Fatalf("got = %v", got)
	}
	if called {
		t.Fatal("matcher called on empty input")
	}
}

func TestExtractBatch_CanceledContextStopsHalfway(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e := NewExtractor(fakeMatcher{"a": "A"}, Config{MaxEntities: 8, Workers: 1}, exLogger())
	if got, _ := e.ExtractBatch(ctx, map[string]string{"p1": "a", "p2": "a"}); len(got) != 0 {
		t.Fatalf("result after cancel = %v", got)
	}
}

func TestExtractBatch_NilMatcherProducesEmptyResult(t *testing.T) {
	// If the dictionary was never built, extraction must NOT CRASH; every post is tagged empty.
	e := NewExtractor(nil, DefaultConfig(), exLogger())
	got, _ := e.ExtractBatch(context.Background(), map[string]string{"p1": "Fenerbahçe"})
	if got["p1"] != `{"entities":[],"topics":[]}` {
		t.Fatalf("got = %q", got["p1"])
	}
}

// matcherFunc turns a single function into a Matcher.
type matcherFunc func(string) []string

func (f matcherFunc) Match(text string) []string { return f(text) }

// fakeNER is the test fake of INERSource.
type fakeNER struct {
	resp map[string][]NERSpan
	err  error
}

func (s fakeNER) ExtractBatch(_ context.Context, _ map[string]string) (map[string][]NERSpan, error) {
	return s.resp, s.err
}

// Behavior must stay BIT-IDENTICAL when there is no NER source (rollback guarantee).
//
// NOTE: the text MATCHES fakeMatcher's key ON PURPOSE ("fenerbahçe")
// — otherwise the dictionary path is never triggered and the test would have tested nothing
// (the "text" value in the brief draft was changed for this reason).
func TestExtractBatch_BehaviorSameWithoutNER(t *testing.T) {
	ex := NewExtractor(fakeMatcher{"fenerbahçe": "Fenerbahçe"}, DefaultConfig(), exLogger())
	got, _ := ex.ExtractBatch(context.Background(), map[string]string{"p1": "bugün fenerbahçe kazandı"})
	if !strings.Contains(got["p1"], "Fenerbahçe") {
		t.Fatalf("dictionary-only path broken: %v", got)
	}
}

// With NER on, the surfaces are written too.
//
// NOTE: for the same reason the text contains "gazze" — to verify that the dictionary path also
// really runs (not only the NER surface).
func TestExtractBatch_NERSurfacesAdded(t *testing.T) {
	ex := NewExtractor(fakeMatcher{"gazze": "Gazze"}, DefaultConfig(), exLogger(),
		WithNER(fakeNER{resp: map[string][]NERSpan{"p1": {{Text: "TÜGVA", Label: "ORG"}}}}, nil))
	got, _ := ex.ExtractBatch(context.Background(), map[string]string{"p1": "gazze'de durum"})
	if !strings.Contains(got["p1"], "tugva") || !strings.Contains(got["p1"], "Gazze") {
		t.Fatalf("hybrid output incomplete: %v", got)
	}
}

// ⚠️ If the NER call returns an ERROR NOTHING is written — the counterpart of the empty_dict gate.
// A half-tagged post is never scanned again (the scanner only scans a post that does NOT have an
// entities field), so an empty label cannot be taken back.
func TestExtractBatch_NothingWrittenOnNERError(t *testing.T) {
	ex := NewExtractor(fakeMatcher{"gazze": "Gazze"}, DefaultConfig(), exLogger(),
		WithNER(fakeNER{err: errors.New("ner down")}, nil))
	got, err := ex.ExtractBatch(context.Background(), map[string]string{"p1": "metin"})
	if len(got) != 0 {
		t.Fatalf("write happened although NER failed: %v", got)
	}
	// A8: the per-round event returns to the CALLER as an error — it is written not to per-post
	// ExtractTotal but to the scanner's EntityScanSkipped family.
	if !errors.Is(err, ErrNERUnavailable) {
		t.Fatalf("err = %v, ErrNERUnavailable expected", err)
	}
}

// A8 (spec §10): ExtractTotal carries ONLY per-post results. Previously the per-round
// `ner_unavailable` was in the same vector too; every PromQL that took a ratio came out wrong by up to
// a factor of POST_ENTITY_MAX_PER_RUN.
func TestExtractTotal_OnlyPerPostLabels(t *testing.T) {
	if n := testutil.CollectAndCount(ExtractTotal); n != 3 {
		t.Fatalf("ExtractTotal carried %d series, want 3 (tagged/empty/error)", n)
	}
}

// ─── C2: chunking the NER call ──────────────────────────────────────

// the NER that records, stores the slice size of each call and the texts it saw.
type kaydedenNER struct {
	mu    sync.Mutex
	sizes []int
	texts map[string]string
	refs  []map[string]string
	// failAt is WHICH call (1-based) will fail. 0 → none.
	failAt int
	calls  int
}

func (k *kaydedenNER) ExtractBatch(_ context.Context, texts map[string]string) (map[string][]NERSpan, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.calls++
	k.sizes = append(k.sizes, len(texts))
	k.refs = append(k.refs, texts)
	if k.texts == nil {
		k.texts = map[string]string{}
	}
	for id, t := range texts {
		k.texts[id] = t
	}
	if k.failAt != 0 && k.calls == k.failAt {
		return nil, errors.New("slice crashed")
	}
	out := make(map[string][]NERSpan, len(texts))
	for id := range texts {
		out[id] = []NERSpan{{Text: "S-" + id, Label: "ORG"}}
	}
	return out, nil
}

func nPost(n int) map[string]string {
	m := make(map[string]string, n)
	for i := range n {
		m[fmt.Sprintf("p%03d", i)] = "metin"
	}
	return m
}

// The NER call must be split into FIXED-SIZE slices. A single call, with the measured
// cost of 17 s/1000 posts, exceeded ENTITY_NER_TIMEOUT even at default settings
// (C2).
func TestExtractBatch_NERCallIsSliced(t *testing.T) {
	ner := &kaydedenNER{}
	ex := NewExtractor(fakeMatcher{}, Config{MaxEntities: 8, Workers: 2, NERBatch: 100},
		exLogger(), WithNER(ner, nil))

	got, _ := ex.ExtractBatch(context.Background(), nPost(250))
	if len(got) != 250 {
		t.Fatalf("written post count = %d, want 250", len(got))
	}

	sort.Sort(sort.Reverse(sort.IntSlice(ner.sizes)))
	want := []int{100, 100, 50}
	if len(ner.sizes) != len(want) {
		t.Fatalf("call count = %d (%v), want %d — slicing not done",
			len(ner.sizes), ner.sizes, len(want))
	}
	for i := range want {
		if ner.sizes[i] != want[i] {
			t.Fatalf("slice sizes = %v, want %v", ner.sizes, want)
		}
	}
	// Even if sliced, EVERY post must enter the result — no loss at the slice boundary.
	for id := range nPost(250) {
		if !strings.Contains(got[id], "s-"+id) {
			t.Fatalf("post %s lost at slice boundary: %q", id, got[id])
		}
	}
}

func TestExtractBatch_NERChunkMapsNotClearedAfterRPC(t *testing.T) {
	ner := &kaydedenNER{}
	ex := NewExtractor(fakeMatcher{}, Config{Workers: 1, NERBatch: 2},
		exLogger(), WithNER(ner, nil))
	_, _ = ex.ExtractBatch(context.Background(), nPost(3))

	for i, ref := range ner.refs {
		if len(ref) == 0 {
			t.Fatalf("RPC %d'nin input map was cleared afterwards", i)
		}
	}
}

// If NERBatch is not given DefaultNERBatch applies — if the wiring skips this field
// the single-call failure must not come back.
func TestExtractBatch_NERBatchDefaultApplied(t *testing.T) {
	ner := &kaydedenNER{}
	ex := NewExtractor(fakeMatcher{}, Config{MaxEntities: 8, Workers: 1}, exLogger(),
		WithNER(ner, nil))
	_, _ = ex.ExtractBatch(context.Background(), nPost(DefaultNERBatch+1))
	if len(ner.sizes) != 2 {
		t.Fatalf("call count = %d (%v), want 2 — default slice not applied",
			len(ner.sizes), ner.sizes)
	}
}

// A SINGLE SLICE ERROR DROPS THE WHOLE ROUND. A partial write would mark the posts in the failed slice
// with `{"entities":[]}` and make them NEVER scanned again.
func TestExtractBatch_SliceErrorDropsWholeRound(t *testing.T) {
	// 250 posts → 3 slices; the SECOND one crashes. The first slice had returned SUCCESSFULLY.
	ner := &kaydedenNER{failAt: 2}
	ex := NewExtractor(fakeMatcher{}, Config{MaxEntities: 8, Workers: 2, NERBatch: 100},
		exLogger(), WithNER(ner, nil))

	got, _ := ex.ExtractBatch(context.Background(), nPost(250))
	if len(got) != 0 {
		t.Fatalf("partial write happened (%d posts) — contract is fail-closed", len(got))
	}
}

// ─── I2: both legs see the SAME text ───────────────────────────────────

// The text going to NER must also pass through PromptText: raw URLs are removed
// and trimmed to MaxPromptRunes. Otherwise spaCy would tag domain names as ORG/PRODUCT
// and EVERY post that shares a link would inflate the publisher.
func TestExtractBatch_PreparedTextGoesToNER(t *testing.T) {
	ner := &kaydedenNER{}
	ex := NewExtractor(fakeMatcher{}, DefaultConfig(), exLogger(), WithNER(ner, nil))

	text := "haber https://www.milliyet.com.tr/spor/x " + strings.Repeat("a", MaxPromptRunes+50)
	_, _ = ex.ExtractBatch(context.Background(), map[string]string{"p1": text})

	seen := ner.texts["p1"]
	if seen == "" {
		t.Fatal("NER saw no text")
	}
	if strings.Contains(seen, "milliyet") || strings.Contains(seen, "http") {
		t.Fatalf("RAW URL reached NER: %q", seen[:min(80, len(seen))])
	}
	if n := len([]rune(seen)); n > MaxPromptRunes {
		t.Fatalf("untruncated text reached NER: %d rune > %d", n, MaxPromptRunes)
	}
}

// ─── I1: trimming preserves dictionary canonicals ────────────────────────────────

// Since MaxEntities cuts from the tail, when the cap is full the survivors must be
// LINKED CANONICALS — not raw NER surfaces. 89% of surfaces stay singleton in
// measurement and Slot4 cannot produce candidates from a singleton key (design
// §7.1); rule 4's hashtag rescue would be exactly here.
func TestExtractBatch_TrimmingPreservesDictionaryCanonicals(t *testing.T) {
	ner := fakeNER{resp: map[string][]NERSpan{
		"p1": {{Text: "TÜGVA", Label: "ORG"}, {Text: "KAAN", Label: "PRODUCT"}},
	}}
	ex := NewExtractor(
		fakeMatcher{"gazze": "Gazze", "fenerbahçe": "Fenerbahçe"},
		Config{MaxEntities: 2, Workers: 1}, exLogger(),
		WithNER(ner, nil))

	got, _ := ex.ExtractBatch(context.Background(), map[string]string{"p1": "gazze ve fenerbahçe"})
	var tags Tags
	if err := json.Unmarshal([]byte(got["p1"]), &tags); err != nil {
		t.Fatal(err)
	}
	if len(tags.Entities) != 2 {
		t.Fatalf("MaxEntities not applied: %v", tags.Entities)
	}
	kalan := map[string]bool{}
	for _, e := range tags.Entities {
		kalan[e] = true
	}
	if !kalan["Gazze"] || !kalan["Fenerbahçe"] {
		t.Fatalf("dictionary canonicals dropped in truncation: %v", tags.Entities)
	}
	if kalan["tugva"] || kalan["kaan"] {
		t.Fatalf("raw surface outranked the canonical: %v", tags.Entities)
	}
}
