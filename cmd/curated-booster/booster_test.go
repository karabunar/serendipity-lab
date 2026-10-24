package main

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"

	"serendipity/internal/curated/grpcclient"
	"serendipity/internal/postcache"
	"serendipity/internal/postmeta"
)

func TestRunBoost_WritesHighQualityOldPost(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})

	oldEnough := time.Now().Add(-72 * time.Hour) // > CURATED_MIN_AGE (48h)
	cache := postcache.NewLatestPostsCache(rdb, 24*time.Hour, testLogger())
	if err := cache.RefreshAuthor(context.Background(), "author1", []postcache.PostEntry{
		{PostID: "p1", CreatedAt: oldEnough},
	}); err != nil {
		t.Fatalf("RefreshAuthor: %v", err)
	}

	pdStore := postmeta.New(rdb, postmeta.Config{}, testLogger())
	// p1: from the same author as OTHER high-engagement posts (we add at least 1 more post so that
	// author_trust is high, but only p1 will enter the pool —
	// even if p1's OWN engagement is 0, it gets its quality from author_trust).
	if err := writeTestPost(rdb, "p1", "author1", 0, 0, "Bu yeterince uzun ve kaliteli bir post metnidir, iki yüz karaktere yakın olması gerekir ki content_signal formülü tam puan versin, o yüzden burada epey satır dolduruyoruz devam ediyoruz.", oldEnough); err != nil {
		t.Fatalf("writeTestPost p1: %v", err)
	}

	cfg := BoostConfig{
		MinAge:           48 * time.Hour,
		QualityThreshold: 0, // no filter — S2 default
		TextLenNorm:      200,
		MediaBonus:       0.3,
	}
	if err := RunBoost(context.Background(), rdb, cache, pdStore, nil, cfg, testLogger()); err != nil {
		t.Fatalf("RunBoost: %v", err)
	}

	members, err := rdb.ZRevRange(context.Background(), boostedZSetKey, 0, -1).Result()
	if err != nil {
		t.Fatalf("ZRevRange: %v", err)
	}
	if len(members) != 1 || members[0] != "p1" {
		t.Errorf("expected [p1] in %s, got %v", boostedZSetKey, members)
	}
}

func TestRunBoost_SkipsPostYoungerThanMinAge(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})

	tooYoung := time.Now().Add(-1 * time.Hour) // < CURATED_MIN_AGE (48h)
	cache := postcache.NewLatestPostsCache(rdb, 24*time.Hour, testLogger())
	if err := cache.RefreshAuthor(context.Background(), "author1", []postcache.PostEntry{
		{PostID: "p1", CreatedAt: tooYoung},
	}); err != nil {
		t.Fatalf("RefreshAuthor: %v", err)
	}
	pdStore := postmeta.New(rdb, postmeta.Config{}, testLogger())
	if err := writeTestPost(rdb, "p1", "author1", 0, 0, "kısa", tooYoung); err != nil {
		t.Fatalf("writeTestPost: %v", err)
	}

	cfg := BoostConfig{MinAge: 48 * time.Hour, QualityThreshold: 0, TextLenNorm: 200, MediaBonus: 0.3}
	if err := RunBoost(context.Background(), rdb, cache, pdStore, nil, cfg, testLogger()); err != nil {
		t.Fatalf("RunBoost: %v", err)
	}

	exists := mr.Exists(boostedZSetKey)
	if exists {
		t.Error("expected serendipity:curated:boosted to NOT exist — post too young")
	}
}

func TestRunBoost_ResetsZSetOnEachRun(t *testing.T) {
	// DEL+ZADD pattern: a post_id written in the previous run but no longer
	// a candidate MUST DROP from the ZSET in this run (spec §3.3 note — the design decision
	// that prevents stale references from accumulating).
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})

	mr.ZAdd(boostedZSetKey, 9.0, "stale-from-previous-run")

	cache := postcache.NewLatestPostsCache(rdb, 24*time.Hour, testLogger())
	pdStore := postmeta.New(rdb, postmeta.Config{}, testLogger())
	cfg := BoostConfig{MinAge: 48 * time.Hour, QualityThreshold: 0, TextLenNorm: 200, MediaBonus: 0.3}

	if err := RunBoost(context.Background(), rdb, cache, pdStore, nil, cfg, testLogger()); err != nil {
		t.Fatalf("RunBoost: %v", err)
	}

	if mr.Exists(boostedZSetKey) {
		t.Error("expected serendipity:curated:boosted to be cleared — no authors scanned, ZSET should not persist stale entries")
	}
}

// writeTestPost writes the serendipity:post:data:{id} HASH directly for the test
// (postmeta.Store's own write API is specific to the real writers;
// the test uses HSET directly here, as the postmeta hydrate tests do).
func writeTestPost(rdb goredis.Cmdable, postID, authorID string, likes, reblogs int64, text string, createdAt time.Time) error {
	ctx := context.Background()
	key := "serendipity:post:data:" + postID
	return rdb.HSet(ctx, key, map[string]interface{}{
		"author_id":  authorID,
		"text":       text,
		"likes":      likes,
		"reblogs":    reblogs,
		"created_at": createdAt.Format(time.RFC3339),
	}).Err()
}

type fakeCurator struct {
	results map[string]grpcclient.EnrichedPost
	err     error
}

func (f *fakeCurator) EnrichPosts(_ context.Context, candidates []grpcclient.EnrichCandidate) ([]grpcclient.EnrichedPost, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([]grpcclient.EnrichedPost, 0, len(candidates))
	for _, c := range candidates {
		if r, ok := f.results[c.PostID]; ok {
			out = append(out, r)
		}
	}
	return out, nil
}

func TestRunBoost_CuratorSuccess_WritesLLMScore(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})

	oldEnough := time.Now().Add(-72 * time.Hour)
	cache := postcache.NewLatestPostsCache(rdb, 24*time.Hour, testLogger())
	if err := cache.RefreshAuthor(context.Background(), "author1", []postcache.PostEntry{
		{PostID: "p1", CreatedAt: oldEnough},
	}); err != nil {
		t.Fatalf("RefreshAuthor: %v", err)
	}
	pdStore := postmeta.New(rdb, postmeta.Config{}, testLogger())
	if err := writeTestPost(rdb, "p1", "author1", 5, 2, "uzun ve kaliteli bir metin burada epey satır dolduruyoruz devam ediyoruz test amaçlı", oldEnough); err != nil {
		t.Fatalf("writeTestPost: %v", err)
	}

	curator := &fakeCurator{results: map[string]grpcclient.EnrichedPost{
		"p1": {PostID: "p1", QualityScore: 0.9},
	}}

	cfg := BoostConfig{MinAge: 48 * time.Hour, QualityThreshold: 0, TextLenNorm: 200, MediaBonus: 0.3, LLMQualityThreshold: 0}
	if err := RunBoost(context.Background(), rdb, cache, pdStore, curator, cfg, testLogger()); err != nil {
		t.Fatalf("RunBoost: %v", err)
	}

	// The ZSET score must now be the LLM quality_score (0.9), not the heuristic.
	score, err := rdb.ZScore(context.Background(), boostedZSetKey, "p1").Result()
	if err != nil {
		t.Fatalf("ZScore: %v", err)
	}
	if score != 0.9 {
		t.Errorf("expected ZSET score=0.9 (LLM), got %f", score)
	}
}

func TestRunBoost_CuratorFails_FallsBackToHeuristic(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})

	oldEnough := time.Now().Add(-72 * time.Hour)
	cache := postcache.NewLatestPostsCache(rdb, 24*time.Hour, testLogger())
	if err := cache.RefreshAuthor(context.Background(), "author1", []postcache.PostEntry{
		{PostID: "p1", CreatedAt: oldEnough},
	}); err != nil {
		t.Fatalf("RefreshAuthor: %v", err)
	}
	pdStore := postmeta.New(rdb, postmeta.Config{}, testLogger())
	if err := writeTestPost(rdb, "p1", "author1", 5, 2, "uzun ve kaliteli bir metin burada epey satır dolduruyoruz devam ediyoruz test amaçlı", oldEnough); err != nil {
		t.Fatalf("writeTestPost: %v", err)
	}

	curator := &fakeCurator{err: context.DeadlineExceeded}

	cfg := BoostConfig{MinAge: 48 * time.Hour, QualityThreshold: 0, TextLenNorm: 200, MediaBonus: 0.3, LLMQualityThreshold: 0}
	if err := RunBoost(context.Background(), rdb, cache, pdStore, curator, cfg, testLogger()); err != nil {
		t.Fatalf("RunBoost should not fail when curator errors (fallback expected): %v", err)
	}

	// The post must still enter the ZSET — with the heuristic score (fallback).
	exists, err := rdb.ZScore(context.Background(), boostedZSetKey, "p1").Result()
	if err != nil {
		t.Fatalf("expected p1 in ZSET via heuristic fallback: %v", err)
	}
	if exists <= 0 {
		t.Errorf("expected positive heuristic fallback score, got %f", exists)
	}
}

func TestRunBoost_NilCurator_HeuristicOnly_Unchanged(t *testing.T) {
	// Regression: when curator=nil the behavior must stay BIT-IDENTICAL with the existing
	// Task 6 tests (the rollout scenario where the agent service was never deployed).
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})

	oldEnough := time.Now().Add(-72 * time.Hour)
	cache := postcache.NewLatestPostsCache(rdb, 24*time.Hour, testLogger())
	if err := cache.RefreshAuthor(context.Background(), "author1", []postcache.PostEntry{
		{PostID: "p1", CreatedAt: oldEnough},
	}); err != nil {
		t.Fatalf("RefreshAuthor: %v", err)
	}
	pdStore := postmeta.New(rdb, postmeta.Config{}, testLogger())
	if err := writeTestPost(rdb, "p1", "author1", 0, 0, "uzun ve kaliteli bir metin burada epey satır dolduruyoruz devam ediyoruz test amaçlı", oldEnough); err != nil {
		t.Fatalf("writeTestPost: %v", err)
	}

	cfg := BoostConfig{MinAge: 48 * time.Hour, QualityThreshold: 0, TextLenNorm: 200, MediaBonus: 0.3, LLMQualityThreshold: 0}
	if err := RunBoost(context.Background(), rdb, cache, pdStore, nil, cfg, testLogger()); err != nil {
		t.Fatalf("RunBoost: %v", err)
	}

	members, err := rdb.ZRevRange(context.Background(), boostedZSetKey, 0, -1).Result()
	if err != nil {
		t.Fatalf("ZRevRange: %v", err)
	}
	if len(members) != 1 || members[0] != "p1" {
		t.Errorf("expected [p1] in %s, got %v", boostedZSetKey, members)
	}
}
