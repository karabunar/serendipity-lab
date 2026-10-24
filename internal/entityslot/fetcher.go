// Package entity implements the fourth retrieval slot, which fetches candidate posts
// from the entities in the user's interest profile (Slot4 — Entity Discovery).
//
// Difference from the ANN-based discovery slot: the match is SYMBOLIC, it uses no vector. Even when
// the ANN query vector is noise (e.g. a mock embedder), this channel is
// unaffected and depends only on Redis.
package entityslot

import (
	"context"
	"log/slog"
	"math"
	"sort"
	"time"

	"serendipity/internal/core"
	"serendipity/internal/m04interest"
	"serendipity/internal/postmeta"
	"serendipity/internal/trfold"
)

// IInterestProfileSource reads the user's entity interest profile.
// Concrete impl: *m04interest.RedisStore.
type IInterestProfileSource interface {
	Get(ctx context.Context, userID string) (m04interest.Profile, error)
}

// IIndexReader reads from the inverted index with a window.
// Concrete impl: *entityindex.Reader.
type IIndexReader interface {
	RecentBatch(ctx context.Context, limits map[string]int) (map[string][]string, error)
	// DocFreq, the IDF inputs: per entity, the number of posts in the window (df) and
	// the total number of indexed posts in the window (N).
	DocFreq(ctx context.Context, entities []string) (map[string]int64, int64, error)
}

// IPostMetaSource returns the author and entity info of candidate posts.
// Concrete impl: *postmeta.Store.
type IPostMetaSource interface {
	GetEntityMeta(ctx context.Context, postIDs []string) (map[string]postmeta.EntityMeta, error)
}

// Config is the slot behavior.
type Config struct {
	// MaxPostsPerAuthor, spam layer 2: an author can return this many posts in a single
	// request. Same pattern as the per-author caps of the other slots.
	MaxPostsPerAuthor int
	// Overfetch is how many times the quota is fetched from the index. It compensates for what veto and the author
	// quota will eliminate; the share of an entity with an empty pool
	// is also covered from this surplus WITHOUT OPENING A SEPARATE ROUND.
	Overfetch float64
	// VetoEnabled turns elimination by the Negative map on/off.
	//
	// ⚠️ DEFAULT OFF. The negative map is fed from
	// skip, and skip measures EXPOSURE, not "dislike": in a measurement over
	// real users the negative map was almost a copy of the positive one (one event
	// topic +66 / -44), the current rule cut the slot from 200 → 18 candidates, and whichever
	// threshold was tried, the vetoed ones were always hubs. The rule stays in the code;
	// before turning it on, the negative signal must be normalized by exposure.
	//
	VetoEnabled bool
	// MinRelativeWeight drops query entities whose (IDF-scaled) weight is below this fraction of the
	// user's strongest entity. Without it, every entity in the profile's query list gets at least one
	// slot, so a one-off interest rides along with the user's real ones and puts off-topic posts in
	// the result. 0 means DefaultMinRelativeWeight; a negative value turns the floor off.
	MinRelativeWeight float64
	// MaxAuthorShare softly limits how much of the result one author may fill: at most
	// ceil(MaxAuthorShare × limit) posts are taken from an author in the first pass; posts over that
	// are used only to fill what is still missing (still bounded by MaxPostsPerAuthor), so the result
	// is diverse without coming back short. 0 means DefaultMaxAuthorShare; a negative value turns it off.
	MaxAuthorShare float64
}

const (
	// DefaultMinRelativeWeight is the MinRelativeWeight used when it is 0.
	DefaultMinRelativeWeight = 0.15
	// DefaultMaxAuthorShare is the MaxAuthorShare used when it is 0.
	DefaultMaxAuthorShare = 0.3
)

// DefaultConfig holds the default values.
func DefaultConfig() Config {
	return Config{MaxPostsPerAuthor: 5, Overfetch: 3.0, VetoEnabled: false}
}

// Fetcher implements Slot4 (ICandidateFetcher).
type Fetcher struct {
	profiles IInterestProfileSource
	index    IIndexReader
	meta     IPostMetaSource
	cfg      Config
	logger   *slog.Logger
}

// New fills zero/negative config fields with defaults.
func New(profiles IInterestProfileSource, index IIndexReader, meta IPostMetaSource, cfg Config, logger *slog.Logger) *Fetcher {
	d := DefaultConfig()
	if cfg.MaxPostsPerAuthor <= 0 {
		cfg.MaxPostsPerAuthor = d.MaxPostsPerAuthor
	}
	if cfg.Overfetch < 1 {
		cfg.Overfetch = d.Overfetch
	}
	if cfg.MinRelativeWeight == 0 {
		cfg.MinRelativeWeight = DefaultMinRelativeWeight
	}
	if cfg.MaxAuthorShare == 0 {
		cfg.MaxAuthorShare = DefaultMaxAuthorShare
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Fetcher{profiles: profiles, index: index, meta: meta, cfg: cfg, logger: logger}
}

// Name is the log/metric label.
func (f *Fetcher) Name() string { return "entity" }

// aday is the intermediate record carried through elimination.
type aday struct {
	postID string
	weight float64
}

// Fetch returns at most limit candidates from the user's interest profile.
//
// A missing profile and a profile read error are NOT ERRORS (nil, nil): the discovery
// channel does not bring down feed generation. An index/meta error, however, is a slot error —
// CandidateManager.fanOut already tolerates a slot error and the other three slots
// keep running.
func (f *Fetcher) Fetch(ctx context.Context, userID string, limit int) ([]core.Candidate, error) {
	if limit <= 0 {
		return nil, nil
	}

	p, err := f.profiles.Get(ctx, userID)
	if err != nil {
		SlotSkipped.WithLabelValues("profile_error").Inc()
		f.logger.Warn("entity slot: could not read interest profile — channel absent for this request",
			slog.String("user_id", userID),
			slog.String("error", err.Error()))
		return nil, nil
	}
	if p.IsZero() {
		SlotSkipped.WithLabelValues("no_profile").Inc()
		return nil, nil
	}
	if len(p.Query) == 0 {
		SlotSkipped.WithLabelValues("empty_query").Inc()
		return nil, nil
	}

	weights := f.idfWeights(ctx, userID, p)
	query := f.significant(p.Query, weights)
	quota := distributeQuota(weights, query, limit)
	if len(quota) == 0 {
		SlotSkipped.WithLabelValues("empty_query").Inc()
		return nil, nil
	}

	limits := make(map[string]int, len(quota))
	for e, n := range quota {
		limits[e] = int(math.Ceil(float64(n) * f.cfg.Overfetch))
	}

	byEntity, err := f.index.RecentBatch(ctx, limits)
	if err != nil {
		return nil, err
	}

	// Candidate pool: the one with the HIGHEST entity weight wins (the same post may
	// come from multiple entities).
	pool := make(map[string]float64)
	order := make([]string, 0, limit)
	for _, e := range query {
		ids := byEntity[e]
		if len(ids) == 0 {
			EmptyEntities.Inc()
			continue
		}
		w := weights[e]
		for _, id := range ids {
			if cur, ok := pool[id]; ok {
				if w > cur {
					pool[id] = w
				}
				continue
			}
			pool[id] = w
			order = append(order, id)
		}
	}
	if len(order) == 0 {
		return nil, nil
	}
	SlotCandidates.WithLabelValues("fetched").Add(float64(len(order)))

	metas, err := f.meta.GetEntityMeta(ctx, order)
	if err != nil {
		return nil, err
	}

	// Layer 1: Negative veto.
	neg := katlanmisNegatif(p)
	vetoed := make([]aday, 0, len(order))
	for _, id := range order {
		m, ok := metas[id]
		if !ok {
			continue // not in post:data — hydration would fail too
		}
		if f.cfg.VetoEnabled && negatifTasiyor(neg, m.Entities) {
			continue
		}
		vetoed = append(vetoed, aday{postID: id, weight: pool[id]})
	}
	SlotCandidates.WithLabelValues("after_veto").Add(float64(len(vetoed)))

	// Layer 2: author quota + self-exclusion. Candidates are visited in weight order so the
	// soft per-author share (first pass) takes each author's best posts; the overflow is kept
	// aside and only used to fill what the diverse pass could not.
	sort.SliceStable(vetoed, func(i, j int) bool { return vetoed[i].weight > vetoed[j].weight })
	softCap := f.cfg.MaxPostsPerAuthor
	if f.cfg.MaxAuthorShare > 0 {
		if c := int(math.Ceil(f.cfg.MaxAuthorShare * float64(limit))); c >= 1 && c < softCap {
			softCap = c
		}
	}
	perAuthor := make(map[string]int, len(vetoed))
	kept := make([]aday, 0, len(vetoed))
	var overflow []aday
	for _, a := range vetoed {
		author := metas[a.postID].AuthorID
		if author == "" || author == userID {
			continue
		}
		if perAuthor[author] >= f.cfg.MaxPostsPerAuthor {
			continue
		}
		perAuthor[author]++
		if perAuthor[author] > softCap {
			overflow = append(overflow, a)
			continue
		}
		kept = append(kept, a)
	}
	SlotCandidates.WithLabelValues("after_author_cap").Add(float64(len(kept) + len(overflow)))

	// Trim to the quota: the one with the HIGHER weight survives.
	//
	// Ordering is done EXPLICITLY, Query's order is NOT TRUSTED: Query currently derives its order from
	// Stable by weight but that is an internal detail of Phase 2
	// and if it changes this would silently cut the wrong post. sort.SliceStable
	// preserves index order (i.e. freshness) on equal weight — freshness does not enter the score a second
	// time, it is only a tiebreaker.
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].weight > kept[j].weight })
	if len(kept) > limit {
		kept = kept[:limit]
	}
	for _, a := range overflow {
		if len(kept) >= limit {
			break
		}
		kept = append(kept, a)
	}

	now := time.Now()
	out := make([]core.Candidate, 0, len(kept))
	for _, a := range kept {
		out = append(out, core.Candidate{
			PostID:      a.postID,
			AuthorID:    metas[a.postID].AuthorID,
			Score:       a.weight,
			SlotSource:  core.SlotEntity,
			RetrievedAt: now,
		})
	}
	SlotCandidates.WithLabelValues("returned").Add(float64(len(out)))

	f.logger.Debug("entity slot done",
		slog.String("user_id", userID),
		slog.Int("queried_entities", len(quota)),
		slog.Int("fetched", len(order)),
		slog.Int("returned", len(out)),
	)
	return out, nil
}

// significant returns the query entities worth querying: those whose weight reaches
// MinRelativeWeight of the strongest one. The order of query is kept. With the floor off, or when
// no entity has a positive weight, the list is returned unchanged.
func (f *Fetcher) significant(query []string, weights map[string]float64) []string {
	if f.cfg.MinRelativeWeight <= 0 {
		return query
	}
	top := 0.0
	for _, e := range query {
		if w := weights[e]; w > top {
			top = w
		}
	}
	if top <= 0 {
		return query
	}
	out := make([]string, 0, len(query))
	for _, e := range query {
		if weights[e] >= f.cfg.MinRelativeWeight*top {
			out = append(out, e)
		}
	}
	return out
}

// katlanmisNegatif reduces the profile's Negative keys to the trfold.Fold space.
// Built ONCE per Fetch (not per candidate).
//
// ⚠️ The comparison is in the trfold.Fold space (A6): the builder writes profile keys
// folded ("galatasaray"), while the candidate's `entities` field carries the dictionary
// canonical raw ("Galatasaray"). Keys are folded here too,
// so profiles written before folding also match correctly.
func katlanmisNegatif(p m04interest.Profile) map[string]struct{} {
	if len(p.Negative) == 0 {
		return nil
	}
	neg := make(map[string]struct{}, len(p.Negative))
	for k := range p.Negative {
		neg[trfold.Fold(k)] = struct{}{}
	}
	return neg
}

// negatifTasiyor checks whether any of the candidate's entities is in the profile's Negative map.
// This is the only mechanism that stops a rival club's post from reaching a Fenerbahçe
// fan.
func negatifTasiyor(neg map[string]struct{}, entities []string) bool {
	if len(neg) == 0 {
		return false
	}
	for _, e := range entities {
		if _, bad := neg[trfold.Fold(e)]; bad {
			return true
		}
	}
	return false
}

// idfWeights returns the EFFECTIVE weight of each entity in the query list:
//
//	weight × log(1 + N/df)
//
// Why: the profile carries the entities the user
// interacts with most, and hubs like "Türkiye" that appear in almost every
// post rise to the top of EVERYONE's profile ("Türkiye" was in the query list of 53 of 60
// users). With raw weights the quota and ordering go to
// hubs, and the first 8 candidates of the 6 users with the richest profiles came out IDENTICAL.
// IDF brings distinctive interests (Arda Güler, CHP/İzmir) forward.
//
// The profile does NOT CHANGE: "this person interacts a lot with Türkiye" is a correct piece of behavioral
// information; scaling happens only at query time, in this slot.
//
// Fail-soft: a DocFreq error or an empty index (N=0) falls back to the raw weight —
// IDF is an improvement, it must not bring the slot down. An entity with df=0 has no post
// in the window anyway; its weight does not change.
func (f *Fetcher) idfWeights(ctx context.Context, userID string, p m04interest.Profile) map[string]float64 {
	out := make(map[string]float64, len(p.Query))
	for _, e := range p.Query {
		out[e] = p.Stable[e]
	}
	df, total, err := f.index.DocFreq(ctx, p.Query)
	if err != nil {
		IDFFallback.WithLabelValues("docfreq_error").Inc()
		f.logger.Warn("entity slot: could not read IDF — continuing with raw weights",
			slog.String("user_id", userID), slog.String("error", err.Error()))
		return out
	}
	if total <= 0 {
		IDFFallback.WithLabelValues("empty_index").Inc()
		return out
	}
	for _, e := range p.Query {
		if n := df[e]; n > 0 {
			out[e] *= math.Log(1 + float64(total)/float64(n))
		}
	}
	return out
}
