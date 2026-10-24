package entityslot

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// SlotSkipped is the cases where the slot returned without producing any candidates.
//
//	no_profile    — the user has no interest profile (the majority; not an error)
//	empty_query   — the profile exists but the Query list is empty
//	profile_error — Redis read error (fail-open: no channel, the feed does not drop)
var SlotSkipped = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "serendipity_entity_slot_skipped_total",
	Help: "Entity discovery slot early-returns by reason.",
}, []string{"reason"})

// SlotCandidates tracks how the candidate count falls across the elimination layers.
// The difference between two consecutive stages is how much that layer eliminated.
//
//	fetched          — from the inverted index (including overfetch)
//	after_veto       — after the Negative veto
//	after_author_cap — after the author quota + self-exclusion
//	returned         — after trimming to the slot quota
var SlotCandidates = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "serendipity_entity_slot_candidates_total",
	Help: "Entity discovery slot candidate counts per filtering stage.",
}, []string{"stage"})

// EmptyEntities is the number of queried entities whose pool came out empty. Input of
// measurement gate G2: if the ratio exceeds 50% the window or the source is insufficient.
var EmptyEntities = promauto.NewCounter(prometheus.CounterOpts{
	Name: "serendipity_entity_slot_empty_entities_total",
	Help: "Queried entities that returned no posts within the age window.",
})

func init() {
	for _, r := range []string{"no_profile", "empty_query", "profile_error"} {
		SlotSkipped.WithLabelValues(r)
	}
	for _, s := range []string{"fetched", "after_veto", "after_author_cap", "returned"} {
		SlotCandidates.WithLabelValues(s)
	}
}

// IDFFallback is the number of requests where the entity slot skipped IDF scaling and
// continued with the raw weight. reason=docfreq_error (Redis error) ·
// empty_index (total indexed posts in the window is 0). A sustained increase means the slot's
// personalization is being lost to hub entities
// (Türkiye, Erdoğan).
var IDFFallback = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "serendipity_entity_slot_idf_fallback_total",
	Help: "Entity slot requests that skipped IDF scaling and used raw profile weights, by reason.",
}, []string{"reason"})

func init() {
	for _, r := range []string{"docfreq_error", "empty_index"} {
		IDFFallback.WithLabelValues(r)
	}
}
