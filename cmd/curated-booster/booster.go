// booster.go contains the core business logic of the cmd/curated-booster CronJob:
// serendipity:author:posts:* scan → author_trust computation →
// content_signal → serendipity:curated:boosted ZSET write.
package main

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"serendipity/internal/curated/grpcclient"
	"serendipity/internal/curated/model"
	"serendipity/internal/postcache"
)

const (
	authorPostsKeyPrefix = "serendipity:author:posts:"
	boostedZSetKey       = "serendipity:curated:boosted"
	scanBatchSize        = 200
	// maxPostsPerAuthorScan is the number of latest posts to scan per author —
	// aligned with postcache.LatestPostsCache's own capacity (20).
	maxPostsPerAuthorScan = 20
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// BoostConfig controls RunBoost's behavior.
type BoostConfig struct {
	MinAge              time.Duration // CURATED_MIN_AGE
	QualityThreshold    float64       // CURATED_QUALITY_THRESHOLD — heuristic PRE-FILTER threshold (before going to the LLM)
	TextLenNorm         float64       // CURATED_TEXT_LEN_NORM
	MediaBonus          float64       // CURATED_MEDIA_BONUS
	LLMQualityThreshold float64       // CURATED_LLM_QUALITY_THRESHOLD — threshold for the LLM quality_score to enter the ZSET (a SEPARATE threshold, spec § 5)
}

// hydrator is the minimal interface RunBoost needs (satisfied by postmeta.Store;
// the same concrete type is used in tests).
type hydrator interface {
	Hydrate(ctx context.Context, postIDs []string) (map[string]model.PostData, error)
}

// curatorClient is the minimal interface RunBoost needs
// (satisfied by grpcclient.CuratorClient; fakeCurator is used in tests).
type curatorClient interface {
	EnrichPosts(ctx context.Context, candidates []grpcclient.EnrichCandidate) ([]grpcclient.EnrichedPost, error)
}

// postAuthorCache is the minimal interface RunBoost needs
// (satisfied by postcache.LatestPostsCache).
type postAuthorCache interface {
	GetRecentPostsByAuthorWithScores(ctx context.Context, authorID string, limit int) ([]postcache.PostEntry, error)
}

// RunBoost scans all authors, computes the curated boost candidates and
// rewrites the serendipity:curated:boosted ZSET FROM SCRATCH (DEL+ZADD, single
// pipeline — the postcache.RefreshAuthor pattern). This is a deviation from the
// ZREMRANGEBYSCORE pruning proposed in spec §3.3: DEL+ZADD structurally makes it
// impossible for stale post_ids past the 7-day serendipity:post:data TTL to
// accumulate permanently in the ZSET — each run fills the ZSET only with candidates
// that are valid RIGHT NOW (passing the age + threshold test).
func RunBoost(ctx context.Context, rdb goredis.Cmdable, cache postAuthorCache, store hydrator, curator curatorClient, cfg BoostConfig, logger *slog.Logger) error {
	authorIDs, err := scanAuthorIDs(ctx, rdb)
	if err != nil {
		return err
	}
	BoostAuthorsScanned.Set(float64(len(authorIDs)))

	now := time.Now()
	minCreatedAt := now.Add(-cfg.MinAge)

	// candidateContent carries post_id → (text, hasMedia) for the curator call
	// — it reuses the data the heuristic loop ALREADY read, WITHOUT hydrating again.
	type candidateInfo struct {
		text     string
		hasMedia bool
	}
	candidateContent := make(map[string]candidateInfo)

	var members []goredis.Z
	for _, authorID := range authorIDs {
		entries, err := cache.GetRecentPostsByAuthorWithScores(ctx, authorID, maxPostsPerAuthorScan)
		if err != nil || len(entries) == 0 {
			continue
		}
		postIDs := make([]string, len(entries))
		for i, e := range entries {
			postIDs[i] = e.PostID
		}
		data, err := store.Hydrate(ctx, postIDs)
		if err != nil {
			logger.Warn("curated-booster hydrate failed for author",
				slog.String("author_id", authorID), slog.String("error", err.Error()))
			continue
		}
		trust := authorTrust(data)
		// Note: there is DELIBERATELY no early "trust <= 0 → skip the author entirely"
		// shortcut here — trust=0 (an author who has received no engagement) always
		// yields score=0, and whether that gets dropped is decided by the per-post
		// QualityThreshold gate below. The S2 default
		// (CURATED_QUALITY_THRESHOLD=0, "no filter") expects trust=0 posts to pass
		// too — an early skip would break that.
		for _, postID := range postIDs {
			pd, ok := data[postID]
			if !ok {
				continue
			}
			if pd.CreatedAt.IsZero() || pd.CreatedAt.After(minCreatedAt) {
				continue // not old enough (or created_at is unknown)
			}
			hasMedia := len(pd.ImageURLs) > 0 || len(pd.VideoURLs) > 0
			signal := contentSignal(pd.Text, hasMedia, cfg.TextLenNorm, cfg.MediaBonus)
			score := trust * signal
			// Strict "<" (score==threshold is INCLUDED): with threshold=0,
			// score=0 (trust=0 or signal=0) passes too — this is the contract of the
			// "no filter" S2 default. In threshold>0 calibration it has
			// no practical effect (score cannot be negative, trust/signal are always
			// >= 0).
			if score < cfg.QualityThreshold {
				continue
			}
			candidateContent[postID] = candidateInfo{text: pd.Text, hasMedia: hasMedia}
			members = append(members, goredis.Z{Score: score, Member: postID})
		}
	}

	if curator != nil && len(members) > 0 {
		enrichReqs := make([]grpcclient.EnrichCandidate, 0, len(members))
		for _, m := range members {
			postID, _ := m.Member.(string)
			info := candidateContent[postID]
			enrichReqs = append(enrichReqs, grpcclient.EnrichCandidate{
				PostID: postID, Text: info.text, HasMedia: info.hasMedia,
			})
		}
		enriched, err := curator.EnrichPosts(ctx, enrichReqs)
		if err != nil {
			logger.Warn("curator enrich failed — continuing with heuristic score (fallback)",
				slog.String("error", err.Error()))
			// members are ALREADY filled with the heuristic score — nothing changes, this is the fallback.
		} else {
			enrichedByID := make(map[string]grpcclient.EnrichedPost, len(enriched))
			for _, e := range enriched {
				enrichedByID[e.PostID] = e
			}
			newMembers := make([]goredis.Z, 0, len(members))
			for _, m := range members {
				postID, _ := m.Member.(string)
				e, ok := enrichedByID[postID]
				if !ok || e.QualityScore < cfg.LLMQualityThreshold {
					// The LLM never returned this post (dropped on its side) or
					// it failed the threshold — drop this post from the ZSET ENTIRELY (the heuristic
					// fallback applies ONLY when the curator's WHOLE CALL fails,
					// not when an individual post is rejected).
					continue
				}
				newMembers = append(newMembers, goredis.Z{Score: e.QualityScore, Member: postID})
			}
			members = newMembers
		}
	}

	pipe := rdb.Pipeline()
	pipe.Del(ctx, boostedZSetKey)
	if len(members) > 0 {
		pipe.ZAdd(ctx, boostedZSetKey, members...)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return err
	}
	BoostCandidatesWritten.Set(float64(len(members)))
	logger.Info("curated boost run complete",
		slog.Int("authors_scanned", len(authorIDs)),
		slog.Int("candidates_written", len(members)),
	)
	return nil
}

// scanAuthorIDs scans the serendipity:author:posts:* keys with SCAN (until the cursor
// is exhausted) and returns the author_id list.
func scanAuthorIDs(ctx context.Context, rdb goredis.Cmdable) ([]string, error) {
	var authorIDs []string
	var cursor uint64
	for {
		keys, next, err := rdb.Scan(ctx, cursor, authorPostsKeyPrefix+"*", scanBatchSize).Result()
		if err != nil {
			return nil, err
		}
		for _, k := range keys {
			authorIDs = append(authorIDs, strings.TrimPrefix(k, authorPostsKeyPrefix))
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return authorIDs, nil
}

// authorTrust is the average (likes+reblogs) value of an author's scanned
// posts — spec §1.6: Beta (Wilson) is not reused because it rewards posts with
// high engagement, whereas the Curator's target (low-engagement but high-quality)
// is the exact opposite. author_trust is an AUTHOR-LEVEL proxy — a single
// low-engagement post can still be a boost candidate if the author's other posts are
// trustworthy.
func authorTrust(data map[string]model.PostData) float64 {
	if len(data) == 0 {
		return 0
	}
	var sum float64
	for _, pd := range data {
		sum += float64(pd.Likes + pd.Reblogs)
	}
	return sum / float64(len(data))
}

// contentSignal is a cheap, LLM-free quality proxy for the post's text/media
// (between 0 and 1+MediaBonus). When the agent (Phase 1 Curator/Quality
// Analyst) arrives, the LLM score REPLACES this function — the ZSET interface
// and everything else stays the same (spec §2, §8 handoff contract).
func contentSignal(text string, hasMedia bool, textLenNorm, mediaBonus float64) float64 {
	if textLenNorm <= 0 {
		textLenNorm = 200
	}
	ratio := float64(len(text)) / textLenNorm
	if ratio > 1 {
		ratio = 1
	}
	if ratio < 0 {
		ratio = 0
	}
	if hasMedia {
		ratio *= 1 + mediaBonus
	}
	return ratio
}
