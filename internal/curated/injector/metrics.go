package injector

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// CuratedInjectionOutcome tracks the result of every CuratedInjector.Apply call.
// Labels: result ∈ {"injected", "skipped_empty_pool", "skipped_all_seen"}.
var CuratedInjectionOutcome = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "serendipity_scoring_curated_injection_total",
	Help: "CuratedInjector outcomes by result.",
}, []string{"result"})

func init() {
	for _, result := range []string{"injected", "skipped_empty_pool", "skipped_all_seen"} {
		CuratedInjectionOutcome.WithLabelValues(result)
	}
}
