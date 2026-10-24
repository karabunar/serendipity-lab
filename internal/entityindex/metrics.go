package entityindex

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// PostsIndexed is the number of posts written to the inverted index.
var PostsIndexed = promauto.NewCounter(prometheus.CounterOpts{
	Name: "serendipity_entity_index_posts_indexed_total",
	Help: "Posts written to the entity inverted index.",
})

// PostSkipped is the posts EXCLUDED from the index.
//
//	too_many_entities — spam gate: ENTITY_INDEX_MAX_ENTITIES exceeded
//	too_old           — created_at outside the window; if written it would be
//	                    pruned at the next pruning anyway
//	no_created_at     — no/invalid created_at in post:data; no score can be produced
//
// The too_many_entities ratio is the input of measurement gate G1: if it exceeds 30%
// the gate is too tight.
var PostSkipped = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "serendipity_entity_index_post_skipped_total",
	Help: "Posts excluded from the entity inverted index by reason.",
}, []string{"reason"})

// PostTruncated is the number of posts truncated to maxEntities when
// ENTITY_INDEX_OVERFLOW=truncate.
//
// Why a separate counter: in truncate mode PostSkipped{too_many_entities} now
// counts only those exceeding the upper cap (hardDrop). If the truncated ones are visible nowhere,
// the spam-density signal is SILENTLY lost — this metric keeps it
// observable.
var PostTruncated = promauto.NewCounter(prometheus.CounterOpts{
	Name: "serendipity_entity_index_post_truncated_total",
	Help: "Posts truncated to ENTITY_INDEX_MAX_ENTITIES instead of being dropped.",
})

func init() {
	for _, r := range []string{"too_many_entities", "too_old", "no_created_at"} {
		PostSkipped.WithLabelValues(r)
	}
}
