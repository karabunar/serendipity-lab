package main

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// BoostAuthorsScanned tracks the number of authors scanned in a RunBoost run (those with a
// serendipity:author:posts key).
var BoostAuthorsScanned = promauto.NewGauge(prometheus.GaugeOpts{
	Name: "serendipity_curated_boost_authors_scanned",
	Help: "Number of authors scanned in the last curated-booster run.",
})

// BoostCandidatesWritten tracks the number of candidates (posts) written to the
// serendipity:curated:boosted ZSET at the end of a RunBoost run.
// Constantly 0 → the threshold is too high or author_trust produces no signal at all
// (the primary indicator for spec §7 S2/S3 calibration).
var BoostCandidatesWritten = promauto.NewGauge(prometheus.GaugeOpts{
	Name: "serendipity_curated_boost_candidates_written",
	Help: "Number of candidates written to serendipity:curated:boosted in the last run.",
})
