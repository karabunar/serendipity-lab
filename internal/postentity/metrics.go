package postentity

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// ExtractTotal is the per-post extraction result.
//
//	tagged         — at least one dictionary/NER match was found in the text
//	empty          — no match (still written, so the post is not processed again)
//	error          — JSON could not be produced; the post stays untagged and is retried
//
// ⚠️ ONLY PER-POST results. The round in which NER was unreachable (one
// event per round) is NOT here, it is in the post consumer's EntityScanSkipped{reason=
// "ner_unavailable"} counter — same family as empty_dict.
// was in the same vector up to that much, and every PromQL that took a ratio came out wrong by up to a factor of POST_ENTITY_MAX_PER_RUN
// (A8).
//
// ⚠️ The `error` bucket is UNREACHABLE TODAY: the only error source is json.Marshal
// and Marshal cannot fail on a struct with two []string fields. The defensive branch
// is deliberately kept (so that if a field that cannot be marshaled is one day added to Tags it is not
// silently swallowed), but if a nonzero `error` value is ever seen
// it means this schema changed — NOT an LLM/network error.
//
// A persistently high "empty" ratio is a problem of DICTIONARY coverage
// (not the prompt): missing aliases are added to overrides.json.
var ExtractTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "serendipity_post_entity_extract_total",
	Help: "Post entity extraction outcomes by result.",
}, []string{"result"})

// EntitiesPerPost is the distribution of entity counts per tagged post. Phase 2
// (user profile) is designed according to this distribution.
var EntitiesPerPost = promauto.NewHistogram(prometheus.HistogramOpts{
	Name:    "serendipity_post_entity_count",
	Help:    "Number of dictionary-matched entities kept per post.",
	Buckets: []float64{0, 1, 2, 3, 5, 8, 12},
})

func init() {
	for _, r := range []string{"tagged", "empty", "error"} {
		ExtractTotal.WithLabelValues(r)
	}
}

// MergeRule is how many times each rule was applied in the hybrid merge.
//
//	rule1_linked          — the NER span matched an alias EXACTLY → canonical
//	rule2_span_wins       — the NER span would have collapsed onto an alias inside it, the span won
//	rule2_dict_suppressed — the dictionary canonical DROPPED because of rule 2
//	rule3_unknown_surface — the surface has no dictionary counterpart at all
//	rule4_dict_only       — the dictionary found it, NER never saw it (hashtag rescue)
//
// DENOMINATOR: every non-empty NER span is counted EXACTLY once into
// one of rule1/rule2_span_wins/rule3 (when a linker exists), so
//
//	rule2 ratio = rule2_span_wins / (rule1_linked + rule2_span_wins + rule3_unknown_surface)
//
// rule4 is the number of extra keys the dictionary added to NER (it does not enter the span denominator).
// ⚠️ The counters are package-global promauto; MergeHybrid is therefore not pure
// and tests read the VALUE DIFFERENCE (before/after) — breaks if parallel tests are written.
//
// Why it is measured: the rule2 ratio is a direct signal about the dictionary's identity-confusion
// load; if it drops, dictionary cleanup is working.
//
// rule2_dict_suppressed is counted SEPARATELY because on the old/offsetless linker path
// suppression may be approximate. The new NER proto has start/end and the production
// path preserves a legitimate match outside the span. This counter is the COST of suppression:
// if it grows permanently large relative to rule2_span_wins, dictionary cleanup should be
// re-examined.
var MergeRule = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "serendipity_post_entity_merge_rule_total",
	Help: "Hybrid merge rule applications by rule.",
}, []string{"rule"})

func init() {
	for _, r := range []string{"rule1_linked", "rule2_span_wins", "rule2_dict_suppressed", "rule3_unknown_surface", "rule4_dict_only"} {
		MergeRule.WithLabelValues(r)
	}
}
