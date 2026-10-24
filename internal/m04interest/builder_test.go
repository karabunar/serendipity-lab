package m04interest

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"
)

type fakeSource struct {
	sigs []Signal
	err  error
	// since is the threshold given by the caller — for the rolling update test.
	since time.Time
}

func (f *fakeSource) Positives(_ context.Context, _ string, since time.Time) ([]Signal, error) {
	f.since = since
	return f.sigs, f.err
}

func (f *fakeSource) Negatives(_ context.Context, _ string, since time.Time) ([]Signal, error) {
	f.since = since
	return f.sigs, f.err
}

type memStore map[string]Profile

func (m memStore) Get(_ context.Context, uid string) (Profile, error) { return m[uid], nil }
func (m memStore) Save(_ context.Context, uid string, p Profile) error {
	m[uid] = p
	return nil
}

func bLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func TestBuilder_FirstRunProducesProfile(t *testing.T) {
	now := time.Now()
	pos := &fakeSource{sigs: []Signal{{Entities: []string{"Fenerbahçe"}, Value: 2, At: now}}}
	st := memStore{}

	p, err := NewBuilder(pos, &fakeSource{}, st, DefaultBuildConfig(), bLogger()).BuildUser(context.Background(), "u1", now)
	if err != nil {
		t.Fatal(err)
	}
	if p.Stable["fenerbahce"] <= 0 || p.Recent["fenerbahce"] <= 0 {
		t.Fatalf("p = %+v", p)
	}
	if len(p.Query) != 1 || p.Query[0] != "fenerbahce" {
		t.Fatalf("Query = %v", p.Query)
	}
	if p.Version != SchemaVersion || p.UpdatedAt.IsZero() {
		t.Fatalf("p = %+v", p)
	}
	if _, ok := st["u1"]; !ok {
		t.Fatal("profile not saved")
	}
}

func TestBuilder_PassesPreviousUpdatedAtToSources(t *testing.T) {
	// Contract of the rolling update: sources must return ONLY new
	// interactions, otherwise the weight would inflate on every run.
	now := time.Now()
	prev := NewProfile()
	prev.UpdatedAt = now.Add(-6 * time.Hour)
	st := memStore{"u1": prev}
	pos := &fakeSource{}

	if _, err := NewBuilder(pos, &fakeSource{}, st, DefaultBuildConfig(), bLogger()).BuildUser(context.Background(), "u1", now); err != nil {
		t.Fatal(err)
	}
	if !pos.since.Equal(prev.UpdatedAt) {
		t.Fatalf("since = %v, want %v", pos.since, prev.UpdatedAt)
	}
}

func TestBuilder_OldWeightDecays(t *testing.T) {
	now := time.Now()
	cfg := DefaultBuildConfig()
	cfg.StableHalfLife = 24 * time.Hour
	cfg.RecentHalfLife = 24 * time.Hour

	prev := NewProfile()
	prev.UpdatedAt = now.Add(-24 * time.Hour)
	prev.Stable = Weights{"a": 8}
	st := memStore{"u1": prev}

	// Decay is applied at WRITE time; triggering a write requires a new signal on ANOTHER
	// entity. Not writing at all on a run without signals is deliberate
	// (see TestBuilder_NoNewSignalDoesNOTWrite).
	pos := &fakeSource{sigs: []Signal{{Entities: []string{"b"}, Value: 1, At: now}}}

	p, err := NewBuilder(pos, &fakeSource{}, st, cfg, bLogger()).BuildUser(context.Background(), "u1", now)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Stable["a"]; got < 3.9 || got > 4.1 {
		t.Fatalf("Stable[A] = %v, want ~4 (one half-life)", got)
	}
}

func TestBuilder_NoNewSignalDoesNOTWrite(t *testing.T) {
	// Decay is not lost, it is DEFERRED: since UpdatedAt stays old, on the next
	// real signal elapsed covers the whole gap and gives the same result.
	// Rewriting on an empty run would mean (a) a needless SET for ~31k users
	// and (b) the dormant profile's TTL extending forever
	// — Redis memory is limited.
	now := time.Now()
	prev := NewProfile()
	prev.UpdatedAt = now.Add(-100 * time.Hour)
	prev.Stable = Weights{"a": 8}
	st := memStore{"u1": prev}

	p, err := NewBuilder(&fakeSource{}, &fakeSource{}, st, DefaultBuildConfig(), bLogger()).BuildUser(context.Background(), "u1", now)
	if err != nil {
		t.Fatal(err)
	}
	// The returned profile must be IDENTICAL to the old one — not decayed.
	if p.Stable["a"] != 8 {
		t.Fatalf("Stable[A] = %v, want 8 (untouched)", p.Stable["a"])
	}
	if !st["u1"].UpdatedAt.Equal(prev.UpdatedAt) {
		t.Fatalf("UpdatedAt in store changed: %v", st["u1"].UpdatedAt)
	}
}

func TestBuilder_DeferredDecayIsEquivalent(t *testing.T) {
	// Proof of the contract: a signal that arrives after two empty runs were skipped
	// must give the SAME result as if there had been no empty runs.
	now := time.Now()
	cfg := DefaultBuildConfig()
	cfg.StableHalfLife = 24 * time.Hour
	cfg.RecentHalfLife = 24 * time.Hour

	prev := NewProfile()
	prev.UpdatedAt = now.Add(-24 * time.Hour)
	prev.Stable = Weights{"a": 8}

	b := func(st memStore) *Builder {
		return NewBuilder(&fakeSource{}, &fakeSource{}, st, cfg, bLogger())
	}

	// (1) Two empty runs, then a signal.
	stA := memStore{"u1": prev}
	for _, at := range []time.Time{now.Add(-16 * time.Hour), now.Add(-8 * time.Hour)} {
		if _, err := b(stA).BuildUser(context.Background(), "u1", at); err != nil {
			t.Fatal(err)
		}
	}
	pos := &fakeSource{sigs: []Signal{{Entities: []string{"b"}, Value: 1, At: now}}}
	withSkips, err := NewBuilder(pos, &fakeSource{}, stA, cfg, bLogger()).BuildUser(context.Background(), "u1", now)
	if err != nil {
		t.Fatal(err)
	}

	// (2) A direct signal with no empty runs.
	stB := memStore{"u1": prev}
	pos2 := &fakeSource{sigs: []Signal{{Entities: []string{"b"}, Value: 1, At: now}}}
	direct, err := NewBuilder(pos2, &fakeSource{}, stB, cfg, bLogger()).BuildUser(context.Background(), "u1", now)
	if err != nil {
		t.Fatal(err)
	}

	if diff := withSkips.Stable["a"] - direct.Stable["a"]; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("deferred decay is not equivalent: %v vs %v", withSkips.Stable["a"], direct.Stable["a"])
	}
}

func TestBuilder_TwoTimeConstantsDecayDifferently(t *testing.T) {
	// Two totals from the same data: stable must decay slowly, recent quickly.
	now := time.Now()
	cfg := DefaultBuildConfig()
	cfg.StableHalfLife = 2160 * time.Hour // 90 days
	cfg.RecentHalfLife = 168 * time.Hour  // 7 days

	prev := NewProfile()
	prev.UpdatedAt = now.Add(-168 * time.Hour)
	prev.Stable = Weights{"a": 8}
	prev.Recent = Weights{"a": 8}
	st := memStore{"u1": prev}

	// A new signal on ANOTHER entity to trigger the write.
	pos := &fakeSource{sigs: []Signal{{Entities: []string{"b"}, Value: 1, At: now}}}

	p, err := NewBuilder(pos, &fakeSource{}, st, cfg, bLogger()).BuildUser(context.Background(), "u1", now)
	if err != nil {
		t.Fatal(err)
	}
	if p.Stable["a"] <= p.Recent["a"] {
		t.Fatalf("persistent (%v) decayed faster than recent (%v)", p.Stable["a"], p.Recent["a"])
	}
}

func TestBuilder_NegativesKeptSeparateNotSubtractedFromStable(t *testing.T) {
	now := time.Now()
	pos := &fakeSource{sigs: []Signal{{Entities: []string{"galatasaray"}, Value: 1, At: now}}}
	neg := &fakeSource{sigs: []Signal{{Entities: []string{"galatasaray"}, Value: 5, At: now}}}

	p, err := NewBuilder(pos, neg, memStore{}, DefaultBuildConfig(), bLogger()).BuildUser(context.Background(), "u1", now)
	if err != nil {
		t.Fatal(err)
	}
	if p.Negative["galatasaray"] <= 0 {
		t.Fatalf("Negative = %v", p.Negative)
	}
	// The veto is NOT derived so that it can be calibrated independently.
	if p.Stable["galatasaray"] <= 0 {
		t.Fatalf("removed from Stable: %v", p.Stable)
	}
}

func TestBuilder_NegativeSourceErrorDoesNotDropPositives(t *testing.T) {
	// If the analytics mirror cannot be read, negatives stay empty and we continue
	// with positives.
	now := time.Now()
	pos := &fakeSource{sigs: []Signal{{Entities: []string{"a"}, Value: 1, At: now}}}
	neg := &fakeSource{err: errors.New("mirror down")}

	p, err := NewBuilder(pos, neg, memStore{}, DefaultBuildConfig(), bLogger()).BuildUser(context.Background(), "u1", now)
	if err != nil {
		t.Fatal(err)
	}
	if p.Stable["a"] <= 0 {
		t.Fatalf("positives were dropped: %+v", p)
	}
	if len(p.Negative) != 0 {
		t.Fatalf("Negative = %v", p.Negative)
	}
}

func TestBuilder_PositiveSourceErrorPropagates(t *testing.T) {
	// The profile must NOT BE BUILT without positives: decaying the old profile and
	// rewriting it would make data loss look like "interest declined".
	pos := &fakeSource{err: errors.New("ring down")}
	st := memStore{}
	if _, err := NewBuilder(pos, &fakeSource{}, st, DefaultBuildConfig(), bLogger()).BuildUser(context.Background(), "u1", time.Now()); err == nil {
		t.Fatal("expected an error")
	}
	if len(st) != 0 {
		t.Fatal("profile must NOT be written on error")
	}
}

func TestBuilder_MaxEntitiesPrunes(t *testing.T) {
	now := time.Now()
	cfg := DefaultBuildConfig()
	cfg.MaxEntities = 2

	sigs := []Signal{
		{Entities: []string{"a"}, Value: 1, At: now},
		{Entities: []string{"b"}, Value: 5, At: now},
		{Entities: []string{"c"}, Value: 3, At: now},
	}
	p, err := NewBuilder(&fakeSource{sigs: sigs}, &fakeSource{}, memStore{}, cfg, bLogger()).BuildUser(context.Background(), "u1", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Stable) != 2 {
		t.Fatalf("Stable = %v", p.Stable)
	}
	if _, ok := p.Stable["a"]; ok {
		t.Fatalf("lightest A was kept: %v", p.Stable)
	}
}

func TestBuilder_ConfidenceAccumulates(t *testing.T) {
	now := time.Now()
	prev := NewProfile()
	prev.UpdatedAt = now.Add(-time.Hour)
	prev.Confidence = Confidence{Interactions: 100}
	st := memStore{"u1": prev}

	sigs := []Signal{
		{Entities: []string{"a"}, Value: 1, At: now},
		{Entities: []string{"b"}, Value: 1, At: now},
	}
	p, err := NewBuilder(&fakeSource{sigs: sigs}, &fakeSource{}, st, DefaultBuildConfig(), bLogger()).BuildUser(context.Background(), "u1", now)
	if err != nil {
		t.Fatal(err)
	}
	if p.Confidence.Interactions != 102 {
		t.Fatalf("Interactions = %d, want 102", p.Confidence.Interactions)
	}
}

type fakeGraph struct {
	sigs   []Signal
	err    error
	called bool
	since  time.Time
}

func (f *fakeGraph) Prior(_ context.Context, _ string, since time.Time) ([]Signal, error) {
	f.called = true
	f.since = since
	return f.sigs, f.err
}

func TestBuilder_GraphPriorGivesWeakContribution(t *testing.T) {
	now := time.Now()
	pos := &fakeSource{sigs: []Signal{{Entities: []string{"a"}, Value: 1, At: now}}}
	g := &fakeGraph{sigs: []Signal{{Entities: []string{"b"}, Value: 1, At: now}}}

	b := NewBuilder(pos, &fakeSource{}, memStore{}, DefaultBuildConfig(), bLogger()).WithGraph(g, 0.2)
	p, err := b.BuildUser(context.Background(), "u1", now)
	if err != nil {
		t.Fatal(err)
	}
	// The user's own behavior must always come before the neighbor's.
	if p.Stable["b"] >= p.Stable["a"] {
		t.Fatalf("graph contribution (%v) overrode own behavior (%v)", p.Stable["b"], p.Stable["a"])
	}
	if p.Stable["b"] <= 0 {
		t.Fatalf("graph contribution not applied at all: %v", p.Stable)
	}
}

func TestBuilder_GraphDoesNOT_TriggerWrite(t *testing.T) {
	// If a user with NO interactions of their own but whose neighbors post were rewritten
	// on every run, the 30-day TTL would extend forever and the dormant
	// profile would never expire — that was the reason the empty-run gate exists.
	now := time.Now()
	g := &fakeGraph{sigs: []Signal{{Entities: []string{"b"}, Value: 1, At: now}}}
	prev := NewProfile()
	prev.UpdatedAt = now.Add(-100 * time.Hour)
	st := memStore{"u1": prev}

	b := NewBuilder(&fakeSource{}, &fakeSource{}, st, DefaultBuildConfig(), bLogger()).WithGraph(g, 0.2)
	if _, err := b.BuildUser(context.Background(), "u1", now); err != nil {
		t.Fatal(err)
	}
	if !st["u1"].UpdatedAt.Equal(prev.UpdatedAt) {
		t.Fatal("wrote with graph signal only — TTL would extend forever")
	}
	// On a run caught by the gate, Redis must not be hit at all for the graph.
	if g.called {
		t.Fatal("graph source queried while gate closed")
	}
}

func TestBuilder_ColdStartGraphWritesALONE(t *testing.T) {
	// Design §4.2.5: the real value of the graph is with LOW-INTERACTION users.
	// When the gate was unconditional, a user with zero interactions never reached the graph
	// block; the feature's rationale was silently cancelled.
	now := time.Now()
	g := &fakeGraph{sigs: []Signal{{Entities: []string{"b"}, Value: 1, At: now}}}
	st := memStore{}

	b := NewBuilder(&fakeSource{}, &fakeSource{}, st, DefaultBuildConfig(), bLogger()).WithGraph(g, 0.2)
	p, err := b.BuildUser(context.Background(), "u1", now)
	if err != nil {
		t.Fatal(err)
	}
	if !g.called {
		t.Fatal("graph source never queried on cold start")
	}
	if p.Stable["b"] <= 0 {
		t.Fatalf("graph-only profile not produced: %+v", p)
	}
	if _, ok := st["u1"]; !ok {
		t.Fatal("graph-only write not done on cold start")
	}

	// ONE write per TTL period: since prev is no longer zero, later
	// empty runs must be skipped again (otherwise the dormant profile's TTL extends forever).
	written := st["u1"]
	g.called = false
	if _, err := b.BuildUser(context.Background(), "u1", now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if !st["u1"].UpdatedAt.Equal(written.UpdatedAt) {
		t.Fatal("second empty run rewrote — TTL would extend forever")
	}
	if g.called {
		t.Fatal("gate must not query the graph when prev is set")
	}
}

func TestBuilder_ColdStartDoesNOTWriteIfGraphAlsoEmpty(t *testing.T) {
	// If all three sources are empty there is nothing to write; an empty profile would mean
	// a key + TTL cost for 31k dormant users.
	now := time.Now()
	st := memStore{}
	b := NewBuilder(&fakeSource{}, &fakeSource{}, st, DefaultBuildConfig(), bLogger()).
		WithGraph(&fakeGraph{}, 0.2)

	if _, err := b.BuildUser(context.Background(), "u1", now); err != nil {
		t.Fatal(err)
	}
	if len(st) != 0 {
		t.Fatalf("meaningless empty profile written: %+v", st)
	}
}

func TestBuilder_ColdStartDoesNOTWriteWhenGraphOff(t *testing.T) {
	now := time.Now()
	st := memStore{}
	if _, err := NewBuilder(&fakeSource{}, &fakeSource{}, st, DefaultBuildConfig(), bLogger()).
		BuildUser(context.Background(), "u1", now); err != nil {
		t.Fatal(err)
	}
	if len(st) != 0 {
		t.Fatalf("empty profile written while graph off: %+v", st)
	}
}

func TestBuilder_PassesPreviousUpdatedAtToGraph(t *testing.T) {
	// Without `since`, the same neighbor post is re-added on every run and the graph
	// contribution accumulates and overrides the user's own behavior.
	now := time.Now()
	prev := NewProfile()
	prev.UpdatedAt = now.Add(-6 * time.Hour)
	st := memStore{"u1": prev}
	pos := &fakeSource{sigs: []Signal{{Entities: []string{"a"}, Value: 1, At: now}}}
	g := &fakeGraph{}

	b := NewBuilder(pos, &fakeSource{}, st, DefaultBuildConfig(), bLogger()).WithGraph(g, 0.2)
	if _, err := b.BuildUser(context.Background(), "u1", now); err != nil {
		t.Fatal(err)
	}
	if !g.since.Equal(prev.UpdatedAt) {
		t.Fatalf("graf since = %v, want %v", g.since, prev.UpdatedAt)
	}
}

func TestBuilder_WithGraphNilAndZeroWeightDisables(t *testing.T) {
	// "Flag off" and "weight zero" must give the SAME effect; the builder
	// must never call a nil source.
	now := time.Now()
	pos := &fakeSource{sigs: []Signal{{Entities: []string{"a"}, Value: 1, At: now}}}
	g := &fakeGraph{sigs: []Signal{{Entities: []string{"b"}, Value: 1, At: now}}}

	for _, tc := range []struct {
		name   string
		src    GraphSource
		weight float64
	}{
		{"nil kaynak", nil, 0.2},
		{"sifir agirlik", g, 0},
		{"negatif agirlik", g, -1},
	} {
		g.called = false
		b := NewBuilder(pos, &fakeSource{}, memStore{}, DefaultBuildConfig(), bLogger()).WithGraph(tc.src, tc.weight)
		p, err := b.BuildUser(context.Background(), "u1", now)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if g.called {
			t.Fatalf("%s: disabled graph source called", tc.name)
		}
		if _, ok := p.Stable["b"]; ok {
			t.Fatalf("%s: graph contribution leaked: %v", tc.name, p.Stable)
		}
	}
}

func TestBuilder_GraphNotCalledAndNoEffectWhenOff(t *testing.T) {
	now := time.Now()
	g := &fakeGraph{sigs: []Signal{{Entities: []string{"b"}, Value: 1, At: now}}}
	pos := &fakeSource{sigs: []Signal{{Entities: []string{"a"}, Value: 1, At: now}}}

	p, err := NewBuilder(pos, &fakeSource{}, memStore{}, DefaultBuildConfig(), bLogger()).BuildUser(context.Background(), "u1", now)
	if err != nil {
		t.Fatal(err)
	}
	if g.called {
		t.Fatal("graph source used without WithGraph")
	}
	if _, ok := p.Stable["b"]; ok {
		t.Fatalf("Stable = %v", p.Stable)
	}
}

func TestBuilder_GraphErrorDoesNotDropProfile(t *testing.T) {
	now := time.Now()
	pos := &fakeSource{sigs: []Signal{{Entities: []string{"a"}, Value: 1, At: now}}}
	g := &fakeGraph{err: errors.New("neo down")}

	b := NewBuilder(pos, &fakeSource{}, memStore{}, DefaultBuildConfig(), bLogger()).WithGraph(g, 0.2)
	p, err := b.BuildUser(context.Background(), "u1", now)
	if err != nil {
		t.Fatal(err)
	}
	if p.Stable["a"] <= 0 {
		t.Fatalf("graph error dropped the positives: %+v", p)
	}
}

// filteringSource imitates the `since` gate of the real sources: it only returns
// signals with At > since. Behavioral proof for the grace test.
type filteringSource struct{ all []Signal }

func (f *filteringSource) Positives(_ context.Context, _ string, since time.Time) ([]Signal, error) {
	out := []Signal{}
	for _, s := range f.all {
		if since.IsZero() || s.At.After(since) {
			out = append(out, s)
		}
	}
	return out, nil
}

func TestBuilder_SinceGraceDefaultIsCURRENTBehavior(t *testing.T) {
	// Default 0 = exactly-once. The threshold given to the source must not change.
	now := time.Now()
	prev := NewProfile()
	prev.UpdatedAt = now.Add(-6 * time.Hour)
	pos := &fakeSource{sigs: []Signal{{Entities: []string{"a"}, Value: 1, At: now}}}
	neg := &fakeSource{}
	g := &fakeGraph{}

	b := NewBuilder(pos, neg, memStore{"u1": prev}, DefaultBuildConfig(), bLogger()).WithGraph(g, 0.2)
	if _, err := b.BuildUser(context.Background(), "u1", now); err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]time.Time{"pozitif": pos.since, "olumsuz": neg.since, "graf": g.since} {
		if !got.Equal(prev.UpdatedAt) {
			t.Fatalf("%s since = %v, want %v", name, got, prev.UpdatedAt)
		}
	}
}

func TestBuilder_SinceGraceShiftsGateBackward(t *testing.T) {
	// Late-arriving interaction: its EVENT time is before the last write but it landed
	// in the buffer after. Without a grace window it is lost permanently.
	now := time.Now()
	prev := NewProfile()
	prev.UpdatedAt = now.Add(-6 * time.Hour)
	gec := Signal{Entities: []string{"a"}, Value: 1, At: now.Add(-8 * time.Hour)}

	cfg := DefaultBuildConfig()
	st := memStore{"u1": prev}
	if _, err := NewBuilder(&filteringSource{all: []Signal{gec}}, &fakeSource{}, st, cfg, bLogger()).
		BuildUser(context.Background(), "u1", now); err != nil {
		t.Fatal(err)
	}
	if _, ok := st["u1"].Stable["a"]; ok {
		t.Fatal("late interaction caught with grace=0 — not today's behavior")
	}

	cfg.SinceGrace = 4 * time.Hour
	st2 := memStore{"u1": prev}
	if _, err := NewBuilder(&filteringSource{all: []Signal{gec}}, &fakeSource{}, st2, cfg, bLogger()).
		BuildUser(context.Background(), "u1", now); err != nil {
		t.Fatal(err)
	}
	if st2["u1"].Stable["a"] <= 0 {
		t.Fatalf("late interaction inside grace window not caught: %+v", st2["u1"].Stable)
	}
}

func TestBuilder_SinceGraceHasNoEffectOnFirstRun(t *testing.T) {
	// When prev is zero everything is processed anyway; applying grace would pull `since`
	// into the past and produce a meaningless non-zero threshold.
	now := time.Now()
	cfg := DefaultBuildConfig()
	cfg.SinceGrace = 4 * time.Hour
	pos := &fakeSource{sigs: []Signal{{Entities: []string{"a"}, Value: 1, At: now}}}

	if _, err := NewBuilder(pos, &fakeSource{}, memStore{}, cfg, bLogger()).
		BuildUser(context.Background(), "u1", now); err != nil {
		t.Fatal(err)
	}
	if !pos.since.IsZero() {
		t.Fatalf("since = %v on first run, should be zero", pos.since)
	}
}

func TestBuilder_WeightsRoundedAndSAMEAsStore(t *testing.T) {
	// Redis footprint: 4 decimals trims ~35% of the JSON payload. Rounding
	// is done here and not in MarshalJSON so that the returned profile and the one in the store
	// are identical.
	now := time.Now()
	sigs := []Signal{{Entities: []string{"Fenerbahçe"}, Value: 7.817318099288395, At: now}}
	st := memStore{}

	p, err := NewBuilder(&fakeSource{sigs: sigs}, &fakeSource{sigs: sigs}, st, DefaultBuildConfig(), bLogger()).
		BuildUser(context.Background(), "u1", now)
	if err != nil {
		t.Fatal(err)
	}
	for name, w := range map[string]Weights{"Stable": p.Stable, "Recent": p.Recent, "Negative": p.Negative} {
		got := w["fenerbahce"]
		if got != 7.8173 {
			t.Fatalf("%s = %v, want 7.8173", name, got)
		}
	}
	if st["u1"].Stable["fenerbahce"] != p.Stable["fenerbahce"] {
		t.Fatalf("store (%v) and memory (%v) diverged",
			st["u1"].Stable["fenerbahce"], p.Stable["fenerbahce"])
	}
}

func TestBuilder_ClockSkewDoesNotMoveUpdatedAtBACKWARD(t *testing.T) {
	// Clock skew between pods: `now` is behind the previous write. If UpdatedAt
	// moves backward, the next run's `since` also moves back and the interactions in between
	// are added AGAIN.
	now := time.Now()
	prev := NewProfile()
	prev.UpdatedAt = now.Add(2 * time.Hour) // written in the future
	prev.Stable = Weights{"a": 8}
	st := memStore{"u1": prev}

	pos := &fakeSource{sigs: []Signal{{Entities: []string{"b"}, Value: 1, At: now}}}
	p, err := NewBuilder(pos, &fakeSource{}, st, DefaultBuildConfig(), bLogger()).
		BuildUser(context.Background(), "u1", now)
	if err != nil {
		t.Fatal(err)
	}
	if p.UpdatedAt.Before(prev.UpdatedAt) {
		t.Fatalf("UpdatedAt moved backwards: %v < %v", p.UpdatedAt, prev.UpdatedAt)
	}
}
