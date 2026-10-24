package m04interest

import (
	"math"
	"testing"
	"time"
)

func approx(t *testing.T, got, want, tol float64, msg string) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Fatalf("%s: got %v, want %v (±%v)", msg, got, want, tol)
	}
}

func TestLambda_HalvesAtHalfLife(t *testing.T) {
	// Half-life definition: after h time the weight must drop to exactly half.
	h := 90 * 24 * time.Hour
	l := Lambda(h)
	got := math.Exp(-l * h.Hours())
	approx(t, got, 0.5, 1e-9, "yari-omur")
}

func TestLambda_ZeroAndNegativeMeanNoDecay(t *testing.T) {
	// Behavior with no decay (lambda=0) is a safe default: a wrong config
	// must NOT ZERO OUT the weights, it should only turn decay off.
	for _, h := range []time.Duration{0, -time.Hour} {
		if l := Lambda(h); l != 0 {
			t.Fatalf("Lambda(%v) = %v, want 0", h, l)
		}
	}
}

func TestDecayWeights_DecaysExponentially(t *testing.T) {
	w := Weights{"Fenerbahçe": 8.0}
	l := Lambda(24 * time.Hour)
	out := DecayWeights(w, l, 24*time.Hour)
	approx(t, out["Fenerbahçe"], 4.0, 1e-9, "one half-life sonra")
}

func TestDecayWeights_DoesNotMutateInput(t *testing.T) {
	// The rolling update uses the same map several times; mutating it
	// in place would be silent data corruption.
	w := Weights{"A": 10}
	DecayWeights(w, Lambda(time.Hour), time.Hour)
	if w["A"] != 10 {
		t.Fatalf("input changed: %v", w["A"])
	}
}

func TestDecayWeights_ZeroElapsedNoop(t *testing.T) {
	w := Weights{"A": 3}
	out := DecayWeights(w, Lambda(time.Hour), 0)
	approx(t, out["A"], 3, 1e-9, "elapsed=0")
}

func TestDecayWeights_NegativeElapsedDoesNotGrow(t *testing.T) {
	// Clock skew / corrupt updated_at could GROW the weight EXPONENTIALLY.
	w := Weights{"A": 3}
	out := DecayWeights(w, Lambda(time.Hour), -100*time.Hour)
	approx(t, out["A"], 3, 1e-9, "negatif elapsed")
}

func TestAddSignals_WeightsByValueAndAge(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	l := Lambda(24 * time.Hour)
	sig := []Signal{
		{Entities: []string{"Fenerbahçe"}, Value: 2.0, At: now},                      // fresh reblog
		{Entities: []string{"Fenerbahçe"}, Value: 1.0, At: now.Add(-24 * time.Hour)}, // like one half-life ago
	}
	out := AddSignals(Weights{}, sig, l, now)
	approx(t, out["fenerbahce"], 2.0+0.5, 1e-9, "2.0 + 1.0*0.5")
}

func TestAddSignals_OneSignalFeedsAllEntities(t *testing.T) {
	now := time.Now()
	out := AddSignals(Weights{}, []Signal{
		{Entities: []string{"Fenerbahçe", "transfer"}, Value: 1.0, At: now},
	}, Lambda(time.Hour), now)
	if len(out) != 2 || out["fenerbahce"] != out["transfer"] {
		t.Fatalf("out = %v", out)
	}
}

func TestAddSignals_FutureDatedSignalDoesNotInflate(t *testing.T) {
	// If At > now, exp(+x) would blow up the weight; the age is clamped to 0.
	now := time.Now()
	out := AddSignals(Weights{}, []Signal{
		{Entities: []string{"A"}, Value: 1.0, At: now.Add(48 * time.Hour)},
	}, Lambda(time.Hour), now)
	approx(t, out["a"], 1.0, 1e-9, "gelecek tarih")
}

func TestAddSignals_SkipsEntitylessAndZeroValueSignals(t *testing.T) {
	now := time.Now()
	out := AddSignals(Weights{}, []Signal{
		{Entities: nil, Value: 1.0, At: now},
		{Entities: []string{"A"}, Value: 0, At: now},
		{Entities: []string{""}, Value: 1.0, At: now},
	}, Lambda(time.Hour), now)
	if len(out) != 0 {
		t.Fatalf("out = %v", out)
	}
}

func TestPrune_KeepsHeaviest(t *testing.T) {
	w := Weights{"A": 1, "B": 5, "C": 3, "D": 2}
	out := Prune(w, 2)
	if len(out) != 2 {
		t.Fatalf("out = %v", out)
	}
	if _, ok := out["B"]; !ok {
		t.Fatalf("heaviest B was dropped: %v", out)
	}
	if _, ok := out["C"]; !ok {
		t.Fatalf("second heaviest C was dropped: %v", out)
	}
}

func TestPrune_StableOnEqualWeight(t *testing.T) {
	// Nondeterministic truncation would shake the profile from run to run. On ties
	// the entity name is the alphabetical tie-break.
	w := Weights{"B": 1, "A": 1, "C": 1}
	first := Prune(w, 2)
	for range 20 {
		if got := Prune(w, 2); len(got) != len(first) || got["A"] != first["A"] || got["B"] != first["B"] {
			t.Fatalf("unstable truncation: %v vs %v", got, first)
		}
	}
	if _, ok := first["A"]; !ok {
		t.Fatalf("alphabetical tie-break not applied: %v", first)
	}
}

func TestPrune_UnlimitedAndSmallMapUntouched(t *testing.T) {
	w := Weights{"A": 1, "B": 2}
	for _, max := range []int{0, -1, 5} {
		if got := Prune(w, max); len(got) != 2 {
			t.Fatalf("max=%d → %v", max, got)
		}
	}
}

func TestTopK_InWeightOrderAndStable(t *testing.T) {
	w := Weights{"A": 1, "B": 5, "C": 3}
	got := TopK(w, 2, 0)
	want := []string{"B", "C"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("TopK = %v, want %v", got, want)
	}
}

func TestTopK_AppliesMinWeight(t *testing.T) {
	w := Weights{"A": 0.1, "B": 5}
	got := TopK(w, 10, 1.0)
	if len(got) != 1 || got[0] != "B" {
		t.Fatalf("TopK = %v", got)
	}
}

func TestTopK_EmptyMapReturnsEmptySliceNotNil(t *testing.T) {
	// So that JSON has [] instead of "query": null; the consumer
	// should not have to do a nil check.
	got := TopK(Weights{}, 5, 0)
	if got == nil || len(got) != 0 {
		t.Fatalf("TopK = %v", got)
	}
}

func TestComputeConfidence_InteractionsAccumulateDensityTop1Ratio(t *testing.T) {
	prev := Confidence{Interactions: 100}
	c := ComputeConfidence(prev, 43, Weights{"A": 6, "B": 4})
	if c.Interactions != 143 {
		t.Fatalf("Interactions = %d, want 143", c.Interactions)
	}
	approx(t, c.Concentration, 0.6, 1e-9, "top1/toplam")
}

func TestComputeConfidence_EmptyProfileZeroDensity(t *testing.T) {
	c := ComputeConfidence(Confidence{}, 0, Weights{})
	if c.Interactions != 0 || c.Concentration != 0 {
		t.Fatalf("c = %+v", c)
	}
}

// A6: the post's `entities` field carries dictionary canonicals
// ("TÜGVA") and folded NER surfaces ("tugva") MIXED. If the profile
// were keyed by the raw string, the same entity would split into two buckets and Query and the veto
// would miss one of them. The key is trfold.Fold — the same folding
// the reverse index (entityindex.Key) uses.
func TestAddSignals_SumsSameEntityInOneBucket(t *testing.T) {
	now := time.Now()
	out := AddSignals(Weights{}, []Signal{
		{Entities: []string{"TÜGVA"}, Value: 1.0, At: now},
		{Entities: []string{"tugva"}, Value: 1.0, At: now},
	}, 0, now)
	if len(out) != 1 {
		t.Fatalf("same entity split into %d buckets: %v", len(out), out)
	}
	approx(t, out["tugva"], 2.0, 1e-9, "iki sinyal tek kovada")
}

// The raw keys of a profile written BEFORE folding must also merge into the same bucket
// on the first rolling update — otherwise the old "Fenerbahçe" bucket would live on, decaying,
// next to the new "fenerbahce" bucket.
func TestAddSignals_FoldsOldRawKeys(t *testing.T) {
	now := time.Now()
	out := AddSignals(Weights{"Fenerbahçe": 1.0}, []Signal{
		{Entities: []string{"fenerbahce"}, Value: 1.0, At: now},
	}, 0, now)
	if len(out) != 1 {
		t.Fatalf("old raw key stayed separate: %v", out)
	}
	approx(t, out["fenerbahce"], 2.0, 1e-9, "eski + yeni tek kovada")
}
