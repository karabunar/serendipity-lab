package entityslot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"serendipity/internal/core"
	"serendipity/internal/m04interest"
	"serendipity/internal/postmeta"
)

type sahteProfil struct {
	p   m04interest.Profile
	err error
}

func (s sahteProfil) Get(_ context.Context, _ string) (m04interest.Profile, error) {
	return s.p, s.err
}

type sahteIndeks struct {
	out     map[string][]string
	err     error
	sonArgs map[string]int
	// IDF inputs; nil → DocFreq returns zero and weights stay raw.
	df    map[string]int64
	total int64
	dfErr error
}

func (s *sahteIndeks) DocFreq(_ context.Context, _ []string) (map[string]int64, int64, error) {
	return s.df, s.total, s.dfErr
}

func (s *sahteIndeks) RecentBatch(_ context.Context, limits map[string]int) (map[string][]string, error) {
	s.sonArgs = limits
	return s.out, s.err
}

type sahteMeta struct {
	out map[string]postmeta.EntityMeta
	err error
}

func (s sahteMeta) GetEntityMeta(_ context.Context, _ []string) (map[string]postmeta.EntityMeta, error) {
	return s.out, s.err
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// testZaman is a fixed time for imitating a generated profile.
// Profile.IsZero() looks ONLY at UpdatedAt.
func testZaman() time.Time { return time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC) }

func profil(query []string, stable, negative map[string]float64) m04interest.Profile {
	p := m04interest.NewProfile()
	p.UpdatedAt = testZaman()
	p.Query = query
	for k, v := range stable {
		p.Stable[k] = v
	}
	for k, v := range negative {
		p.Negative[k] = v
	}
	return p
}

func TestFetch_NoProfile(t *testing.T) {
	f := New(sahteProfil{p: m04interest.Profile{}}, &sahteIndeks{}, sahteMeta{}, DefaultConfig(), testLogger())
	got, err := f.Fetch(context.Background(), "u1", 10)
	if err != nil {
		t.Fatalf("err = %v, missing profile is NOT an error", err)
	}
	if got != nil {
		t.Fatalf("got = %v", got)
	}
}

func TestFetch_ProfileErrorFailOpen(t *testing.T) {
	f := New(sahteProfil{err: errors.New("redis down")}, &sahteIndeks{}, sahteMeta{}, DefaultConfig(), testLogger())
	got, err := f.Fetch(context.Background(), "u1", 10)
	// The discovery channel does NOT bring down the feed: if the profile cannot be read there is no channel.
	if err != nil {
		t.Fatalf("err = %v, fail-open expected", err)
	}
	if got != nil {
		t.Fatalf("got = %v", got)
	}
}

func TestFetch_EmptyQuery(t *testing.T) {
	f := New(sahteProfil{p: profil(nil, nil, nil)}, &sahteIndeks{}, sahteMeta{}, DefaultConfig(), testLogger())
	got, err := f.Fetch(context.Background(), "u1", 10)
	if err != nil || got != nil {
		t.Fatalf("got = %v, err = %v", got, err)
	}
}

func TestFetch_BasicFlow(t *testing.T) {
	idx := &sahteIndeks{out: map[string][]string{"Fenerbahçe": {"p1", "p2"}}}
	meta := sahteMeta{out: map[string]postmeta.EntityMeta{
		"p1": {AuthorID: "a1", Entities: []string{"Fenerbahçe"}},
		"p2": {AuthorID: "a2", Entities: []string{"Fenerbahçe"}},
	}}
	f := New(sahteProfil{p: profil([]string{"Fenerbahçe"}, map[string]float64{"Fenerbahçe": 1.0}, nil)},
		idx, meta, DefaultConfig(), testLogger())

	got, err := f.Fetch(context.Background(), "u1", 10)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("candidate count = %d", len(got))
	}
	for _, c := range got {
		if c.SlotSource != core.SlotEntity {
			t.Fatalf("SlotSource = %v", c.SlotSource)
		}
		if c.Score != 1.0 {
			t.Fatalf("Score = %v, want entity weight 1.0", c.Score)
		}
		if c.AuthorID == "" {
			t.Fatal("AuthorID empty")
		}
	}
	// Overfetch must be reflected in the index: 10 quota x 3.0 = 30.
	if idx.sonArgs["Fenerbahçe"] != 30 {
		t.Fatalf("overfetch not applied: %v", idx.sonArgs)
	}
}

func TestFetch_NegativeVeto(t *testing.T) {
	idx := &sahteIndeks{out: map[string][]string{"Fenerbahçe": {"p1", "p2"}}}
	meta := sahteMeta{out: map[string]postmeta.EntityMeta{
		"p1": {AuthorID: "a1", Entities: []string{"Fenerbahçe"}},
		// p2 also mentions the rival club → veto.
		"p2": {AuthorID: "a2", Entities: []string{"Fenerbahçe", "Galatasaray"}},
	}}
	p := profil([]string{"Fenerbahçe"}, map[string]float64{"Fenerbahçe": 1.0},
		map[string]float64{"Galatasaray": 0.8})

	f := New(sahteProfil{p: p}, idx, meta, vetoAcik(), testLogger())
	got, err := f.Fetch(context.Background(), "u1", 10)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(got) != 1 || got[0].PostID != "p1" {
		t.Fatalf("veto did not work: %+v", got)
	}
}

func TestFetch_WhenVetoOff(t *testing.T) {
	idx := &sahteIndeks{out: map[string][]string{"Fenerbahçe": {"p1", "p2"}}}
	meta := sahteMeta{out: map[string]postmeta.EntityMeta{
		"p1": {AuthorID: "a1", Entities: []string{"Fenerbahçe"}},
		"p2": {AuthorID: "a2", Entities: []string{"Fenerbahçe", "Galatasaray"}},
	}}
	p := profil([]string{"Fenerbahçe"}, map[string]float64{"Fenerbahçe": 1.0},
		map[string]float64{"Galatasaray": 0.8})

	cfg := DefaultConfig()
	cfg.VetoEnabled = false
	f := New(sahteProfil{p: p}, idx, meta, cfg, testLogger())

	got, _ := f.Fetch(context.Background(), "u1", 10)
	if len(got) != 2 {
		t.Fatalf("dropped while veto off: %+v", got)
	}
}

func TestFetch_AuthorQuota(t *testing.T) {
	ids := []string{"p1", "p2", "p3", "p4", "p5", "p6"}
	idx := &sahteIndeks{out: map[string][]string{"Fenerbahçe": ids}}
	m := make(map[string]postmeta.EntityMeta, len(ids))
	for _, id := range ids {
		m[id] = postmeta.EntityMeta{AuthorID: "spam", Entities: []string{"Fenerbahçe"}}
	}
	f := New(sahteProfil{p: profil([]string{"Fenerbahçe"}, map[string]float64{"Fenerbahçe": 1.0}, nil)},
		idx, sahteMeta{out: m}, DefaultConfig(), testLogger())

	got, err := f.Fetch(context.Background(), "u1", 100)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("author quota (5) not applied: %d candidates", len(got))
	}
}

func TestFetch_SelfExclusion(t *testing.T) {
	idx := &sahteIndeks{out: map[string][]string{"Fenerbahçe": {"p1", "p2"}}}
	meta := sahteMeta{out: map[string]postmeta.EntityMeta{
		"p1": {AuthorID: "u1", Entities: []string{"Fenerbahçe"}}, // the user's own post
		"p2": {AuthorID: "a2", Entities: []string{"Fenerbahçe"}},
	}}
	f := New(sahteProfil{p: profil([]string{"Fenerbahçe"}, map[string]float64{"Fenerbahçe": 1.0}, nil)},
		idx, meta, DefaultConfig(), testLogger())

	got, _ := f.Fetch(context.Background(), "u1", 10)
	if len(got) != 1 || got[0].PostID != "p2" {
		t.Fatalf("self-exclusion did not work: %+v", got)
	}
}

func TestFetch_TrimmedToQuota(t *testing.T) {
	ids := make([]string, 0, 20)
	m := make(map[string]postmeta.EntityMeta, 20)
	for i := range 20 {
		id := string(rune('a' + i))
		ids = append(ids, id)
		m[id] = postmeta.EntityMeta{AuthorID: id, Entities: []string{"Fenerbahçe"}}
	}
	idx := &sahteIndeks{out: map[string][]string{"Fenerbahçe": ids}}
	f := New(sahteProfil{p: profil([]string{"Fenerbahçe"}, map[string]float64{"Fenerbahçe": 1.0}, nil)},
		idx, sahteMeta{out: m}, DefaultConfig(), testLogger())

	got, _ := f.Fetch(context.Background(), "u1", 5)
	if len(got) != 5 {
		t.Fatalf("not trimmed to quota: %d", len(got))
	}
}

func TestFetch_IndexErrorPropagates(t *testing.T) {
	idx := &sahteIndeks{err: errors.New("redis down")}
	f := New(sahteProfil{p: profil([]string{"Fenerbahçe"}, map[string]float64{"Fenerbahçe": 1.0}, nil)},
		idx, sahteMeta{}, DefaultConfig(), testLogger())

	// This is a slot error; fanOut already tolerates it (the other three slots run).
	if _, err := f.Fetch(context.Background(), "u1", 10); err == nil {
		t.Fatal("index error was swallowed")
	}
}

func TestName(t *testing.T) {
	f := New(sahteProfil{}, &sahteIndeks{}, sahteMeta{}, DefaultConfig(), testLogger())
	if f.Name() != "entity" {
		t.Fatalf("Name = %q", f.Name())
	}
}

// A6: the builder writes profile keys with trfold.Fold ("galatasaray"),
// while the candidate's `entities` field carries the dictionary canonical raw ("Galatasaray").
// If the veto looks at the raw string, the rival club's post gets through.
func TestFetch_NegativeVetoWithFoldedKey(t *testing.T) {
	idx := &sahteIndeks{out: map[string][]string{"fenerbahce": {"p1", "p2"}}}
	meta := sahteMeta{out: map[string]postmeta.EntityMeta{
		"p1": {AuthorID: "a1", Entities: []string{"Fenerbahçe"}},
		"p2": {AuthorID: "a2", Entities: []string{"Fenerbahçe", "Galatasaray"}},
	}}
	p := profil([]string{"fenerbahce"}, map[string]float64{"fenerbahce": 1.0},
		map[string]float64{"galatasaray": 0.8})

	f := New(sahteProfil{p: p}, idx, meta, vetoAcik(), testLogger())
	got, err := f.Fetch(context.Background(), "u1", 10)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(got) != 1 || got[0].PostID != "p1" {
		t.Fatalf("folded Negative key did not trigger the veto: %+v", got)
	}
}

// IDF: a hub entity like "Türkiye" that appears in every post must not push a distinctive interest (Arda Güler)
// from the top of the slot, even if it is heavier in the profile.
// In real data the first 8 candidates of the 6 users with the richest
// profiles were IDENTICAL because of hubs.
func TestFetch_IDFPutsHubEntityBehindDiscriminatingInterest(t *testing.T) {
	idx := &sahteIndeks{
		out: map[string][]string{
			"turkiye":    {"t1", "t2", "t3", "t4", "t5"},
			"arda guler": {"a1", "a2", "a3", "a4", "a5"},
		},
		df:    map[string]int64{"turkiye": 1000, "arda guler": 10},
		total: 2000,
	}
	meta := sahteMeta{out: map[string]postmeta.EntityMeta{}}
	for i, id := range []string{"t1", "t2", "t3", "t4", "t5", "a1", "a2", "a3", "a4", "a5"} {
		meta.out[id] = postmeta.EntityMeta{AuthorID: fmt.Sprintf("yazar%d", i)}
	}
	p := profil([]string{"turkiye", "arda guler"},
		map[string]float64{"turkiye": 10, "arda guler": 4}, nil)

	f := New(sahteProfil{p: p}, idx, meta, DefaultConfig(), testLogger())
	got, err := f.Fetch(context.Background(), "u1", 4)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(got) == 0 || !strings.HasPrefix(got[0].PostID, "a") {
		t.Fatalf("first candidate must come from the distinctive interest, got %+v", ids(got))
	}
	arda := 0
	for _, c := range got {
		if strings.HasPrefix(c.PostID, "a") {
			arda++
		}
	}
	if arda < len(got)-arda {
		t.Fatalf("hub entity dominated the quota: tail=%d / %d (%v)", arda, len(got), ids(got))
	}
}

// A DocFreq error does NOT bring the slot down: IDF is an improvement, we continue with raw weights
// (today's behavior).
func TestFetch_DocFreqErrorContinuesWithRawWeights(t *testing.T) {
	idx := &sahteIndeks{
		out:   map[string][]string{"turkiye": {"t1"}},
		dfErr: errors.New("redis down"),
	}
	meta := sahteMeta{out: map[string]postmeta.EntityMeta{"t1": {AuthorID: "a"}}}
	p := profil([]string{"turkiye"}, map[string]float64{"turkiye": 1}, nil)
	got, err := New(sahteProfil{p: p}, idx, meta, DefaultConfig(), testLogger()).
		Fetch(context.Background(), "u1", 5)
	if err != nil || len(got) != 1 {
		t.Fatalf("DocFreq error dropped the slot: got=%v err=%v", got, err)
	}
}

func ids(cs []core.Candidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.PostID
	}
	return out
}

// The veto is DEFAULT OFF. The negative signal (skip)
// measures exposure: for a set of real users the negative map was
// almost a copy of the positive one and the current rule cut the slot from 200 → 18 candidates; whichever
// threshold was tried, the vetoed ones were always hubs (Türkiye, İsrail,
// Gazze), there was never a "rival club" sample.
// Feed generation derives the default of ENTITY_NEGATIVE_VETO_ENABLED from here.
func TestDefaultConfig_VetoOff(t *testing.T) {
	if DefaultConfig().VetoEnabled {
		t.Fatal("veto is ON by default — with measured data it would crash the slot")
	}
}

// vetoAcik is for tests that exercise the veto rule itself: the rule stays in
// the code, only the default is off.
func vetoAcik() Config {
	c := DefaultConfig()
	c.VetoEnabled = true
	return c
}

func authorsOf(got []core.Candidate) map[string]int {
	out := map[string]int{}
	for _, c := range got {
		out[c.AuthorID]++
	}
	return out
}

// An entity far below the user's strongest one must not get a slot of its own: the weak
// interest used to ride along (every query entity got at least one slot).
func TestFetch_WeakEntityBelowRelativeFloorIsNotQueried(t *testing.T) {
	idx := &sahteIndeks{out: map[string][]string{"kubernetes": {"p1", "p2"}, "antalya": {"p3"}}}
	meta := sahteMeta{out: map[string]postmeta.EntityMeta{
		"p1": {AuthorID: "a1"}, "p2": {AuthorID: "a2"}, "p3": {AuthorID: "a3"},
	}}
	prof := profil([]string{"kubernetes", "antalya"}, map[string]float64{"kubernetes": 20, "antalya": 1}, nil)

	got, err := New(sahteProfil{p: prof}, idx, meta, DefaultConfig(), testLogger()).Fetch(context.Background(), "u1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, asked := idx.sonArgs["antalya"]; asked {
		t.Fatalf("weak entity must not be queried: %v", idx.sonArgs)
	}
	for _, c := range got {
		if c.PostID == "p3" {
			t.Fatal("off-topic post from the weak entity leaked in")
		}
	}

	// A negative floor restores the old behavior.
	idx2 := &sahteIndeks{out: idx.out}
	cfg := DefaultConfig()
	cfg.MinRelativeWeight = -1
	_, _ = New(sahteProfil{p: prof}, idx2, meta, cfg, testLogger()).Fetch(context.Background(), "u1", 10)
	if _, asked := idx2.sonArgs["antalya"]; !asked {
		t.Fatalf("floor off: weak entity must be queried: %v", idx2.sonArgs)
	}
}

// One prolific author must not fill half of a small result when other authors have posts.
func TestFetch_AuthorShareKeepsTheResultDiverse(t *testing.T) {
	ids := []string{"s1", "s2", "s3", "s4", "s5"}
	for i := 1; i <= 8; i++ {
		ids = append(ids, fmt.Sprintf("o%d", i))
	}
	idx := &sahteIndeks{out: map[string][]string{"kubernetes": ids}}
	m := map[string]postmeta.EntityMeta{}
	for _, id := range ids {
		author := "prolific"
		if id[0] == 'o' {
			author = "other-" + id
		}
		m[id] = postmeta.EntityMeta{AuthorID: author}
	}
	prof := profil([]string{"kubernetes"}, map[string]float64{"kubernetes": 5}, nil)

	got, err := New(sahteProfil{p: prof}, idx, sahteMeta{out: m}, DefaultConfig(), testLogger()).Fetch(context.Background(), "u1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 10 {
		t.Fatalf("want a full result of 10, got %d", len(got))
	}
	if n := authorsOf(got)["prolific"]; n != 3 { // ceil(0.3 × 10)
		t.Fatalf("prolific author should be held to 3 while others can fill the result, got %d", n)
	}
}

// When the other authors cannot fill the result, the overflow fills the gap (up to the hard cap).
func TestFetch_AuthorShareOverflowFillsWhenOthersAreFew(t *testing.T) {
	ids := []string{"s1", "s2", "s3", "s4", "s5", "o1", "o2"}
	idx := &sahteIndeks{out: map[string][]string{"kubernetes": ids}}
	m := map[string]postmeta.EntityMeta{}
	for _, id := range ids {
		a := "prolific"
		if id[0] == 'o' {
			a = "other-" + id
		}
		m[id] = postmeta.EntityMeta{AuthorID: a}
	}
	prof := profil([]string{"kubernetes"}, map[string]float64{"kubernetes": 5}, nil)
	got, _ := New(sahteProfil{p: prof}, idx, sahteMeta{out: m}, DefaultConfig(), testLogger()).Fetch(context.Background(), "u1", 10)
	if len(got) != 7 || authorsOf(got)["prolific"] != 5 {
		t.Fatalf("want 7 candidates with the prolific author at the hard cap 5, got %d / %v", len(got), authorsOf(got))
	}
}

func TestFetch_AuthorShareOffRestoresTheHardCapOnly(t *testing.T) {
	ids := []string{"s1", "s2", "s3", "s4", "s5", "o1"}
	idx := &sahteIndeks{out: map[string][]string{"kubernetes": ids}}
	m := map[string]postmeta.EntityMeta{}
	for _, id := range ids {
		a := "prolific"
		if id == "o1" {
			a = "other"
		}
		m[id] = postmeta.EntityMeta{AuthorID: a}
	}
	cfg := DefaultConfig()
	cfg.MaxAuthorShare = -1
	prof := profil([]string{"kubernetes"}, map[string]float64{"kubernetes": 5}, nil)
	got, _ := New(sahteProfil{p: prof}, idx, sahteMeta{out: m}, cfg, testLogger()).Fetch(context.Background(), "u1", 10)
	if n := authorsOf(got)["prolific"]; n != 5 {
		t.Fatalf("share off: only the hard cap applies, got %d", n)
	}
}
