package entityslot

import "testing"

func toplam(q map[string]int) int {
	t := 0
	for _, v := range q {
		t += v
	}
	return t
}

func TestDistributeQuota_WeightProportional(t *testing.T) {
	w := map[string]float64{"fb": 0.6, "gs": 0.2, "btc": 0.2}
	got := distributeQuota(w, []string{"fb", "gs", "btc"}, 100)

	if toplam(got) != 100 {
		t.Fatalf("total = %d, want 100", toplam(got))
	}
	if got["fb"] <= got["gs"] {
		t.Fatalf("weight not reflected: %v", got)
	}
	// around 0.6/1.0 x 100 = 60.
	if got["fb"] < 55 || got["fb"] > 65 {
		t.Fatalf("fb share = %d, want ~60", got["fb"])
	}
}

func TestDistributeQuota_FloorOne(t *testing.T) {
	// Weak but real interests must not go completely silent.
	w := map[string]float64{"fb": 0.99, "niche": 0.01}
	got := distributeQuota(w, []string{"fb", "niche"}, 10)

	if got["niche"] < 1 {
		t.Fatalf("floor 1 not applied: %v", got)
	}
	if toplam(got) != 10 {
		t.Fatalf("total = %d, want 10", toplam(got))
	}
}

func TestDistributeQuota_PruningFromOne(t *testing.T) {
	// Pruning: if len(order)=5 > total=3 pick the heaviest 3, drop the lighter ones completely.
	// This test confirms the pruning behavior; it provides the floor for the remaining behaviors.
	w := map[string]float64{"a": 0.5, "b": 0.3, "c": 0.1, "d": 0.05, "e": 0.05}
	got := distributeQuota(w, []string{"a", "b", "c", "d", "e"}, 3)

	if toplam(got) != 3 {
		t.Fatalf("total = %d, want 3", toplam(got))
	}
	// After pruning the heaviest three [a, b, c] must be queried.
	if got["a"] < 1 || got["b"] < 1 || got["c"] < 1 {
		t.Fatalf("bad quota after pruning: %v", got)
	}
	// The lighter ones must be dropped completely.
	if got["d"] > 0 || got["e"] > 0 {
		t.Fatalf("light ones still starve: %v", got)
	}
}

func TestDistributeQuota_FloorOneNobodyZeroed(t *testing.T) {
	// Case without pruning: len(order)=4 <= total=10.
	// Floor 1 contract: NO queried entity may be 0.
	w := map[string]float64{"a": 0.97, "b": 0.01, "c": 0.01, "d": 0.01}
	got := distributeQuota(w, []string{"a", "b", "c", "d"}, 10)

	if toplam(got) != 10 {
		t.Fatalf("total = %d, want 10", toplam(got))
	}
	// None may be 0.
	for _, e := range []string{"a", "b", "c", "d"} {
		if got[e] < 1 {
			t.Fatalf("%s = %d, want >= 1: %v", e, got[e], got)
		}
	}
	// Weight must be reflected: a far more than the others.
	if got["a"] <= 5 {
		t.Fatalf("a = %d, should be larger by weight", got["a"])
	}
}

func TestDistributeQuota_AllWeightsZero(t *testing.T) {
	// Profile generated but the Stable map is empty/zero: equal split.
	got := distributeQuota(map[string]float64{}, []string{"a", "b"}, 10)
	if toplam(got) != 10 {
		t.Fatalf("toplam = %d", toplam(got))
	}
	if got["a"] != got["b"] {
		t.Fatalf("not an equal split: %v", got)
	}
}

func TestDistributeQuota_EmptyInput(t *testing.T) {
	if got := distributeQuota(nil, nil, 10); len(got) != 0 {
		t.Fatalf("got = %v", got)
	}
	if got := distributeQuota(map[string]float64{"a": 1}, []string{"a"}, 0); len(got) != 0 {
		t.Fatalf("got for total=0 = %v", got)
	}
}
