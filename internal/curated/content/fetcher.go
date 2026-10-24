// Package curatedcontent provides, for Curated Post-Boost Injection, the Fetcher that
// reads candidates from the serendipity:curated:boosted ZSET (pre-filled by
// cmd/curated-booster) and the Recorder that does best-effort
// reporting after injection. It is completely independent of organic retrieval and
// of whitelistcontent — the pool is pre-computed, at request time
// it only reads Redis (unlike whitelist it does NOT do a request-time Qdrant search).
//
// Niche matching (Strategy 3 — Vector-Native Matching): the candidates' category is NOT
// produced by an LLM and then embedded SEPARATELY (~90% extra LLM cost). Instead,
// the user's niche embeddings (serendipity:user:niche:{uid}) are compared by cosine with the post's ALREADY existing
// original embedding (read through IPostVectorSource from the post vector store).
package content

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"time"

	"github.com/redis/go-redis/v9"

	"serendipity/internal/core"
	"serendipity/internal/curated/model"
)

// boostedZSetKey is the ZSET that cmd/curated-booster fills
// (member=post_id, score=author_trust×content_signal). This name must be the
// SAME as the writer's.
const boostedZSetKey = "serendipity:curated:boosted"

// IHydrator is the interface that fills AuthorID/Text/Media from post_ids
// (satisfied by postmeta.Store.Hydrate).
type IHydrator interface {
	Hydrate(ctx context.Context, postIDs []string) (map[string]model.PostData, error)
}

// IPostVectorSource is the interface that reads posts' original embeddings in
// bulk; the Qdrant-backed vectordb adapter in this repo satisfies it.
type IPostVectorSource interface {
	GetVectorsByPostIDs(ctx context.Context, postIDs []string) (map[string][]float64, error)
}

// nicheEntry is a single niche category inside serendipity:user:niche:{uid}.
type nicheEntry struct {
	Category  string    `json:"category"`
	Embedding []float64 `json:"embedding"`
}

// userNicheKey builds the serendipity:user:niche:{uid} key.
func userNicheKey(userID string) string {
	return "serendipity:user:niche:" + userID
}

// Config controls the Fetcher's behavior.
type Config struct {
	// FetchLimit is the top-N candidate count to pull from the ZSET (default 20).
	// DELIBERATELY LARGER than the number of injection positions (default 1) —
	// so that it serves as a spare candidate for the CuratedInjector after the seen-filter
	// (same rationale as whitelistcontent.Config.TopN).
	FetchLimit int
	// SimilarityThreshold, CURATED_NICHE_SIMILARITY_THRESHOLD — the cosine threshold between the user's
	// niche embedding and the candidate's original post embedding.
	SimilarityThreshold float64
	// ScanLimit is how many ZSET members are read when the user HAS niches, before the niche
	// filter runs; the result is then cut to FetchLimit. Reading only FetchLimit members would
	// make the filter pick among the top-N by pool score alone, which is empty or random when the
	// scores tie. 0 means DefaultScanFactor × FetchLimit.
	ScanLimit int
	// RelativeMargin makes the cosine cut adaptive: a candidate must be within this distance of
	// the BEST niche similarity found among the scanned posts (and above SimilarityThreshold).
	// Embedding similarities cluster in a narrow high band, so one fixed threshold is either
	// too loose for one user and too strict for another. 0 means DefaultRelativeMargin; a
	// negative value turns the relative cut off.
	RelativeMargin float64
}

const (
	// DefaultScanFactor is the ScanLimit multiple of FetchLimit used when ScanLimit is 0.
	DefaultScanFactor = 10
	// DefaultRelativeMargin is the RelativeMargin used when it is 0.
	DefaultRelativeMargin = 0.04
)

// DefaultConfig holds the default values.
func DefaultConfig() Config {
	return Config{FetchLimit: 20}
}

// Fetcher reads the serendipity:curated:boosted ZSET by score (high to low)
// and converts it to a ScoredCandidate list. AuthorID comes from postmeta.Hydrate;
// an empty AuthorID (stale post_id — postdata deleted by TTL) makes it skip that candidate
// SILENTLY, it does not return an error.
type Fetcher struct {
	rdb        redis.Cmdable
	hydrator   IHydrator
	postVecSrc IPostVectorSource
	cfg        Config
	now        func() time.Time
	logger     *slog.Logger
}

// New creates a Fetcher. Zero-valued fields of cfg are pulled to DefaultConfig.
func New(rdb redis.Cmdable, hydrator IHydrator, postVecSrc IPostVectorSource, cfg Config, logger *slog.Logger) *Fetcher {
	d := DefaultConfig()
	if cfg.FetchLimit <= 0 {
		cfg.FetchLimit = d.FetchLimit
	}
	if cfg.ScanLimit <= 0 {
		cfg.ScanLimit = DefaultScanFactor * cfg.FetchLimit
	}
	if cfg.ScanLimit < cfg.FetchLimit {
		cfg.ScanLimit = cfg.FetchLimit
	}
	if cfg.RelativeMargin == 0 {
		cfg.RelativeMargin = DefaultRelativeMargin
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Fetcher{rdb: rdb, hydrator: hydrator, postVecSrc: postVecSrc, cfg: cfg, now: time.Now, logger: logger}
}

// Fetch reads post_ids in score order with ZREVRANGE (the top FetchLimit, or the wider ScanLimit
// when the user has niches), fills AuthorID with postmeta.Hydrate and converts them to a
// ScoredCandidate list of at most FetchLimit entries. The order from the ZSET score
// (high→low) is preserved; ties are broken by niche similarity — CuratedInjector
// does NO extra sorting.
//
// Error policy: if the ZSET is empty (nil, nil) — fail-soft, the caller treats it
// as a no-op. If Hydrate returns an error (nil, err) — curated injection is
// skipped for this request (the organic feed is never affected, the caller is expected to
// wrap it with timeout+recover).
func (f *Fetcher) Fetch(ctx context.Context, userID string) ([]model.ScoredCandidate, error) {
	start := f.now()
	defer func() { FetchLatency.Observe(time.Since(start).Seconds()) }()

	// Niches are read first: with niches the pool is scanned wider (ScanLimit) before filtering.
	niches, nicheErr := f.loadUserNiches(ctx, userID)
	readLimit := f.cfg.FetchLimit
	if nicheErr == nil && len(niches) > 0 {
		readLimit = f.cfg.ScanLimit
	}

	results, err := f.rdb.ZRevRangeWithScores(ctx, boostedZSetKey, 0, int64(readLimit-1)).Result()
	if err != nil {
		f.logger.Debug("curated zset read failed", slog.String("error", err.Error()))
		FetchOutcome.WithLabelValues("empty_pool").Inc()
		return nil, nil
	}
	if len(results) == 0 {
		FetchOutcome.WithLabelValues("empty_pool").Inc()
		return nil, nil
	}

	postIDs := make([]string, len(results))
	scoreByID := make(map[string]float64, len(results))
	for i, z := range results {
		id, _ := z.Member.(string)
		postIDs[i] = id
		scoreByID[id] = z.Score
	}

	data, err := f.hydrator.Hydrate(ctx, postIDs)
	if err != nil {
		f.logger.Debug("curated postdata hydrate failed", slog.String("error", err.Error()))
		FetchOutcome.WithLabelValues("hydrate_error").Inc()
		return nil, err
	}

	now := f.now()
	scored := make([]model.ScoredCandidate, 0, len(postIDs))
	for _, id := range postIDs {
		pd, ok := data[id]
		if !ok || pd.AuthorID == "" {
			// Stale post_id (postdata deleted by TTL) — skip silently.
			continue
		}
		cand := core.Candidate{
			PostID:      id,
			AuthorID:    pd.AuthorID,
			SlotSource:  core.SlotCurated,
			RetrievedAt: now,
		}
		score := scoreByID[id]
		scored = append(scored, model.ScoredCandidate{
			Candidate:  cand,
			FinalScore: score,
		})
	}

	FetchOutcome.WithLabelValues("ok").Inc()

	if nicheErr != nil {
		f.logger.Debug("user niche read failed — continuing without filter",
			slog.String("user_id", userID), slog.String("error", nicheErr.Error()))
		return trimTo(scored, f.cfg.FetchLimit), nil // fail-soft: if the niche cannot be read the filter is skipped
	}
	if len(niches) == 0 {
		return trimTo(scored, f.cfg.FetchLimit), nil // Job B has not run yet — no filter (spec § 3.7)
	}

	postIDsForNiche := make([]string, len(scored))
	for i, sc := range scored {
		postIDsForNiche[i] = sc.Candidate.PostID
	}
	postVecs, err := f.postVecSrc.GetVectorsByPostIDs(ctx, postIDsForNiche)
	if err != nil {
		f.logger.Debug("original post vector batch read failed — continuing without filter",
			slog.String("error", err.Error()))
		return trimTo(scored, f.cfg.FetchLimit), nil
	}

	// Best niche similarity per candidate; candidates without a stored vector are exempt.
	type simmed struct {
		sc     model.ScoredCandidate
		sim    float64
		exempt bool
	}
	all := make([]simmed, 0, len(scored))
	topSim := 0.0
	for _, sc := range scored {
		emb, ok := postVecs[sc.Candidate.PostID]
		if !ok {
			// No original embedding (not found in the vector store / deleted) →
			// EXEMPT from the niche filter (spec § 3.7 fail-soft).
			all = append(all, simmed{sc: sc, exempt: true})
			continue
		}
		best := 0.0
		for _, n := range niches {
			if sim := core.CosineSimilarity(emb, n.Embedding); sim > best {
				best = sim
			}
		}
		if best > topSim {
			topSim = best
		}
		all = append(all, simmed{sc: sc, sim: best})
	}

	cutoff := f.cfg.SimilarityThreshold
	if f.cfg.RelativeMargin > 0 && topSim-f.cfg.RelativeMargin > cutoff {
		cutoff = topSim - f.cfg.RelativeMargin
	}
	kept := all[:0:0]
	for _, c := range all {
		if c.exempt || c.sim >= cutoff {
			kept = append(kept, c)
		}
	}
	// Pool score decides the order; when scores tie, the closer match to the user's niche wins.
	sort.SliceStable(kept, func(i, j int) bool {
		if kept[i].sc.FinalScore != kept[j].sc.FinalScore {
			return kept[i].sc.FinalScore > kept[j].sc.FinalScore
		}
		return kept[i].sim > kept[j].sim
	})
	out := make([]model.ScoredCandidate, 0, len(kept))
	for _, c := range kept {
		out = append(out, c.sc)
	}
	return trimTo(out, f.cfg.FetchLimit), nil
}

// trimTo keeps at most n candidates, preserving order.
func trimTo(in []model.ScoredCandidate, n int) []model.ScoredCandidate {
	if len(in) > n {
		return in[:n]
	}
	return in
}

// loadUserNiches reads the serendipity:user:niche:{uid} JSON. If the key is missing (or
// cannot be parsed) (nil, nil) — Job B may not have run yet, not an error.
func (f *Fetcher) loadUserNiches(ctx context.Context, userID string) ([]nicheEntry, error) {
	data, err := f.rdb.Get(ctx, userNicheKey(userID)).Result()
	if err != nil {
		if err == redis.Nil {
			return nil, nil
		}
		return nil, err
	}
	var niches []nicheEntry
	if err := json.Unmarshal([]byte(data), &niches); err != nil {
		return nil, err
	}
	return niches, nil
}
