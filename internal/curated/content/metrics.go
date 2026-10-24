package content

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// FetchOutcome tracks at which point each Fetcher.Fetch call stopped.
// The post-based twin of whitelistcontent.FetchOutcome.
//
// Labels: result ∈ {"ok", "empty_pool", "hydrate_error"}.
var FetchOutcome = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "serendipity_curated_fetch_total",
	Help: "Curated Post-Boost Injection fetch outcomes by result.",
}, []string{"result"})

// FetchLatency measures Fetch's end-to-end (Redis ZREVRANGE + post hydrate)
// duration.
var FetchLatency = promauto.NewHistogram(prometheus.HistogramOpts{
	Name:    "serendipity_curated_fetch_latency_seconds",
	Help:    "Curated Post-Boost Injection Fetch() end-to-end latency.",
	Buckets: []float64{.002, .005, .01, .02, .05, .08, .1},
})

func init() {
	for _, result := range []string{"ok", "empty_pool", "hydrate_error"} {
		FetchOutcome.WithLabelValues(result)
	}
}
