package main

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"

	"serendipity/internal/curated/content"
	"serendipity/internal/postcache"
	"serendipity/internal/postmeta"
)

// What TestRunBoost_To_FetcherFetch_ProducerConsumerContract wants to lock in:
// cmd/curated-booster (RunBoost, root module) and
// internal/curated/content (Fetcher, a separate module — linked only via a local `replace`)
// share the "serendipity:curated:boosted" ZSET key as TWO INDEPENDENT string
// literals. booster_test.go reads the ZSET RunBoost wrote with a raw
// ZRevRange; fetcher_test.go tests the Fetcher against a ZSET WRITTEN BY HAND
// into miniredis — neither of them runs the real RunBoost →
// real Fetcher chain end to end. If the key literal ever changes
// on only one side (typo, refactor) this test breaks immediately;
// the other two (being isolated) would stay green.
func TestRunBoost_To_FetcherFetch_ProducerConsumerContract(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})

	oldEnough := time.Now().Add(-72 * time.Hour) // > CURATED_MIN_AGE (48h)

	// --- Producer-side setup: author1 → p1 (high quality), author2 → p2 (low quality) ---
	cache := postcache.NewLatestPostsCache(rdb, 24*time.Hour, testLogger())
	if err := cache.RefreshAuthor(context.Background(), "author1", []postcache.PostEntry{
		{PostID: "p1", CreatedAt: oldEnough},
	}); err != nil {
		t.Fatalf("RefreshAuthor author1: %v", err)
	}
	if err := cache.RefreshAuthor(context.Background(), "author2", []postcache.PostEntry{
		{PostID: "p2", CreatedAt: oldEnough},
	}); err != nil {
		t.Fatalf("RefreshAuthor author2: %v", err)
	}

	pdStore := postmeta.New(rdb, postmeta.Config{}, testLogger())
	// p1: high-engagement author (high author_trust) + long quality text
	// (high content_signal) → let score = trust×signal be large.
	longQualityText := "Bu yeterince uzun ve kaliteli bir post metnidir, iki yüz karaktere yakın olması gerekir ki content_signal formülü tam puan versin, o yüzden burada epey satır dolduruyoruz devam ediyoruz."
	if err := writeTestPost(rdb, "p1", "author1", 10, 5, longQualityText, oldEnough); err != nil {
		t.Fatalf("writeTestPost p1: %v", err)
	}
	// p2: low-engagement author + short text → let score be small (but
	// it still enters the pool because threshold=0 means no filter).
	if err := writeTestPost(rdb, "p2", "author2", 1, 0, "kısa", oldEnough); err != nil {
		t.Fatalf("writeTestPost p2: %v", err)
	}

	// --- Producer: RunBoost runs against the real miniredis ---
	boostCfg := BoostConfig{
		MinAge:           48 * time.Hour,
		QualityThreshold: 0, // no filter — S2 default
		TextLenNorm:      200,
		MediaBonus:       0.3,
	}
	if err := RunBoost(context.Background(), rdb, cache, pdStore, nil, boostCfg, testLogger()); err != nil {
		t.Fatalf("RunBoost: %v", err)
	}

	// --- Consumer: the REAL content.Fetcher against the same miniredis ---
	// postVecSrc nil is safe: "any-user" has no niche data, loadUserNiches
	// returns early and postVecSrc is never touched (Strategy 3 fail-soft path).
	fetcher := content.New(rdb, pdStore, nil, content.Config{FetchLimit: 20}, testLogger())
	got, err := fetcher.Fetch(context.Background(), "any-user")
	if err != nil {
		t.Fatalf("Fetcher.Fetch: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("expected 2 candidates from Fetcher (RunBoost wrote p1+p2), got %d: %+v", len(got), got)
	}
	// booster.go writes ZAdd(score=trust×signal, higher = better quality);
	// fetcher.go reads ZRevRangeWithScores (high to low) — p1 (long text,
	// high content_signal) must come before p2 (short text).
	if got[0].Candidate.PostID != "p1" {
		t.Errorf("expected p1 (higher quality) first, got %s", got[0].Candidate.PostID)
	}
	if got[1].Candidate.PostID != "p2" {
		t.Errorf("expected p2 (lower quality) second, got %s", got[1].Candidate.PostID)
	}
	if got[0].FinalScore <= got[1].FinalScore {
		t.Errorf("expected p1.score > p2.score, got p1=%v p2=%v", got[0].FinalScore, got[1].FinalScore)
	}
	if got[0].Candidate.AuthorID != "author1" {
		t.Errorf("expected p1's AuthorID hydrated as author1, got %s", got[0].Candidate.AuthorID)
	}
	if got[1].Candidate.AuthorID != "author2" {
		t.Errorf("expected p2's AuthorID hydrated as author2, got %s", got[1].Candidate.AuthorID)
	}
}
