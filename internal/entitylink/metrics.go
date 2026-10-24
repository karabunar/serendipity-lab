package entitylink

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// BuildRuns counts the outcomes of corpus linker map builds.
var BuildRuns = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "serendipity_entity_link_build_total",
	Help: "Corpus entity linker build outcomes.",
}, []string{"result"})

// MappingCount holds the alias count written in the last successful map swap.
var MappingCount = promauto.NewGauge(prometheus.GaugeOpts{
	Name: "serendipity_entity_link_mapping_count",
	Help: "Number of corpus-derived entity linker mappings.",
})

func init() {
	for _, result := range []string{"success", "error", "empty"} {
		BuildRuns.WithLabelValues(result)
	}
}
