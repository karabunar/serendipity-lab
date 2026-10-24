package canonicaldict

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// AliasCount is the alias count in the in-pod snapshot. Staying at zero means
// "the dictionary was never built": in that case the extraction round is skipped
// WITHOUT ANY PASS and nothing is written. The signal that the dictionary is empty
// is, alongside this gauge, the serendipity_post_entity_scan_skipped_total
// {reason="empty_dict"} metric — it reports that scan rounds were skipped.
var AliasCount = promauto.NewGauge(prometheus.GaugeOpts{
	Name: "serendipity_canonical_dict_alias_count",
	Help: "Aliases in the in-process canonical dictionary snapshot.",
})

// AgeSeconds is the age of the dictionary — the difference between :meta.generated_at and now.
//
// The keys have NO TTL (deliberate, see python/canonical_dict/redis_sink.py):
// therefore a stale dictionary gives no sign of itself. The
// ONLY measurable indicator of freshness is this gauge.
//
// It is NOT SET when :meta cannot be read or cannot be parsed — setting a wrong 0
// is the lie "the dictionary is fresh" and hides exactly the state we want to monitor.
var AgeSeconds = promauto.NewGauge(prometheus.GaugeOpts{
	Name: "serendipity_canonical_dict_age_seconds",
	Help: "Age of the canonical dictionary derived from :meta.generated_at.",
})
