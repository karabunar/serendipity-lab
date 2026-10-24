package m04interest

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// ProfilesBuilt is the per-user build result.
//
//	built   — profile was built and written
//	skipped — no new signal, old profile preserved (no write made)
//	error   — build failed; profile UNCHANGED
var ProfilesBuilt = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "serendipity_user_interest_profiles_total",
	Help: "User interest profile build outcomes by result.",
}, []string{"result"})

// EntitiesPerProfile is the number of lasting entities per profile. Phase 3's
// queryable-list design looks at this distribution.
var EntitiesPerProfile = promauto.NewHistogram(prometheus.HistogramOpts{
	Name:    "serendipity_user_interest_entities",
	Help:    "Stable entity count per built profile.",
	Buckets: []float64{0, 1, 3, 5, 10, 20, 50},
})

// SignalsPerRun is the number of signals processed. kind=positive|negative.
//
// If negative is constantly zero, it indicates that the analytics mirror cannot be read or
// that skip records are never produced — the negative veto (Phase 3)
// would silently stay dead in that case.
var SignalsPerRun = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "serendipity_user_interest_signals_total",
	Help: "Signals folded into interest profiles by kind.",
}, []string{"kind"})

// NegativeRecordsDropped counts the analytics records the negative source skipped because it COULD NOT TIME them.
// analytics kayitlari.
//
//	bad_day — there is neither `last_ts_ms` nor a parseable `day`
//
// If it keeps increasing, it indicates that the mirror's index members are corrupt; in that case
// negative signals silently go missing.
var NegativeRecordsDropped = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "serendipity_user_interest_negative_dropped_total",
	Help: "Analytics mirror records skipped by the negative source, by reason.",
}, []string{"reason"})

func init() {
	for _, r := range []string{"built", "skipped", "error"} {
		ProfilesBuilt.WithLabelValues(r)
	}
	for _, k := range []string{"positive", "negative"} {
		SignalsPerRun.WithLabelValues(k)
	}
	NegativeRecordsDropped.WithLabelValues("bad_day")
}
