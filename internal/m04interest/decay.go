package m04interest

import (
	"math"
	"sort"
	"time"

	"serendipity/internal/trfold"
)

// Signal is a single interaction's contribution to the profile. Signals arrive per post
// but the profile is per entity; the caller (builder) does the translation from the post's
// `entities` field.
type Signal struct {
	Entities []string
	Value    float64 // interactions.RawInteraction.Value — the system's signal weight
	At       time.Time
}

// Lambda produces the exponential decay coefficient from the half-life (in hours).
//
// halfLife <= 0 → 0 (NO DECAY). A misconfiguration must not zero out the
// weights; at most it should turn decay off.
func Lambda(halfLife time.Duration) float64 {
	if halfLife <= 0 {
		return 0
	}
	return math.Ln2 / halfLife.Hours()
}

// DecayWeights decays all weights by `elapsed`. It DOES NOT MODIFY THE INPUT —
// the rolling update uses the same map several times; mutating it in place
// would be silent corruption.
//
// elapsed <= 0 → a copy, no change. A negative elapsed time (clock skew,
// corrupt updated_at) would grow the weight EXPONENTIALLY.
func DecayWeights(w Weights, lambda float64, elapsed time.Duration) Weights {
	out := make(Weights, len(w))
	factor := 1.0
	if lambda > 0 && elapsed > 0 {
		factor = math.Exp(-lambda * elapsed.Hours())
	}
	for k, v := range w {
		out[k] = v * factor
	}
	return out
}

// AddSignals adds new signals to the weights. Each signal is decayed by its own
// age: w += Value × e^(−λ·Δt).
//
// For a future-dated signal the age is clamped to 0 (prevents exp(+x) blow-up).
// A signal with an empty entity or a Value of 0/negative is skipped.
//
// ⚠️ THE KEY is trfold.Fold. The post's `entities`
// field carries dictionary canonicals ("TÜGVA") and folded NER surfaces
// ("tugva") mixed; a raw key would split the same entity into two buckets and
// Query and the Negative veto would miss each other. The reverse index
// (entityindex.Key) uses the same folding, so a folded name in Query
// maps to the same key in the index. The keys of the input map
// are folded too: a profile written BEFORE folding merges on the first update.
// Builder passes all three maps (Stable/Recent/Negative) through this function —
// this is the only place where folding is written.
func AddSignals(w Weights, signals []Signal, lambda float64, now time.Time) Weights {
	out := make(Weights, len(w)+len(signals))
	for k, v := range w {
		if fk := trfold.Fold(k); fk != "" {
			out[fk] += v
		}
	}
	for _, s := range signals {
		if s.Value <= 0 || len(s.Entities) == 0 {
			continue
		}
		age := now.Sub(s.At)
		if age < 0 {
			age = 0
		}
		contrib := s.Value
		if lambda > 0 && age > 0 {
			contrib *= math.Exp(-lambda * age.Hours())
		}
		for _, e := range s.Entities {
			fe := trfold.Fold(e)
			if fe == "" {
				continue
			}
			out[fe] += contrib
		}
	}
	return out
}

// roundWeights rounds weights to `decimals` decimals. It DOES NOT
// MODIFY THE INPUT.
//
// Why: in JSON a single entry {"fenerbahce":7.817318099288395} = 33 B,
// at 4 decimals 21 B — ~35% of the payload. MaxEntities is PER MAP and there are THREE maps;
// with the default of 50 a profile is ~4.8 KB, ~149 MB for 31k users. Redis
// memory is limited, so payload size matters.
//
// 4 decimals is far below any threshold this feature will use (signal
// values are on the order of 1, thresholds are subject to calibration and start from 0).
//
// An entry rounding to zero is DROPPED: an entity with weight 0 carries no
// information but takes space in JSON and could even enter the query list with
// MinQueryWeight=0.
func roundWeights(w Weights, decimals int) Weights {
	if decimals < 0 {
		return w
	}
	scale := math.Pow(10, float64(decimals))
	out := make(Weights, len(w))
	for k, v := range w {
		r := math.Round(v*scale) / scale
		if r == 0 {
			continue
		}
		out[k] = r
	}
	return out
}

// Prune reduces the map to the heaviest maxEntities entries. If maxEntities <= 0
// or the map is already small, a copy is returned.
//
// An ALPHABETICAL tie-break is applied on equal weights: nondeterministic truncation would
// shake the profile from run to run and the question "why was this entity
// dropped" could not be answered.
func Prune(w Weights, maxEntities int) Weights {
	if maxEntities <= 0 || len(w) <= maxEntities {
		out := make(Weights, len(w))
		for k, v := range w {
			out[k] = v
		}
		return out
	}
	keys := sortedByWeight(w)
	out := make(Weights, maxEntities)
	for _, k := range keys[:maxEntities] {
		out[k] = w[k]
	}
	return out
}

// TopK returns the top k entity names in weight order. Those below
// minWeight are dropped. The result NEVER RETURNS NIL — so that JSON has [] instead of
// "q": null.
func TopK(w Weights, k int, minWeight float64) []string {
	out := []string{}
	if k <= 0 {
		return out
	}
	for _, name := range sortedByWeight(w) {
		if w[name] < minWeight {
			break // sorted; the first one below the threshold ends the rest too
		}
		out = append(out, name)
		if len(out) == k {
			break
		}
	}
	return out
}

// ComputeConfidence computes the cumulative interaction count and the concentration.
func ComputeConfidence(prev Confidence, added int, stable Weights) Confidence {
	c := Confidence{Interactions: prev.Interactions + added}
	var total, top float64
	for _, v := range stable {
		total += v
		if v > top {
			top = v
		}
	}
	if total > 0 {
		c.Concentration = top / total
	}
	return c
}

// sortedByWeight sorts by weight descending, by name ascending on ties.
func sortedByWeight(w Weights) []string {
	keys := make([]string, 0, len(w))
	for k := range w {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if w[keys[i]] != w[keys[j]] {
			return w[keys[i]] > w[keys[j]]
		}
		return keys[i] < keys[j]
	})
	return keys
}
