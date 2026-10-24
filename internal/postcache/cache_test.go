package postcache

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestPostEntry_Fields(t *testing.T) {
	entry := PostEntry{
		PostID:    "post-123",
		CreatedAt: time.Date(2026, 4, 13, 10, 0, 0, 0, time.UTC),
	}
	if entry.PostID != "post-123" {
		t.Errorf("unexpected PostID: %s", entry.PostID)
	}
	if entry.CreatedAt.Year() != 2026 {
		t.Errorf("unexpected year: %d", entry.CreatedAt.Year())
	}
}

func TestNewLatestPostsCache_DefaultTTL(t *testing.T) {
	cache := NewLatestPostsCache(nil, 0, nil)
	if cache.ttl != 24*time.Hour {
		t.Errorf("expected 24h default TTL, got %v", cache.ttl)
	}
}

func TestNewLatestPostsCache_CustomTTL(t *testing.T) {
	cache := NewLatestPostsCache(nil, 12*time.Hour, nil)
	if cache.ttl != 12*time.Hour {
		t.Errorf("expected 12h TTL, got %v", cache.ttl)
	}
}

func TestKeyPrefix(t *testing.T) {
	expected := "serendipity:author:posts:"
	if keyPrefix != expected {
		t.Errorf("expected key prefix %s, got %s", expected, keyPrefix)
	}
}

func TestAddPost_IncrementalAndCap(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := NewLatestPostsCache(rdb, time.Hour, logger)

	ctx := context.Background()
	base := time.Unix(1700000000, 0)

	// add 3 posts incrementally (no Del)
	for i := 0; i < 3; i++ {
		p := PostEntry{
			PostID:    fmt.Sprintf("p%d", i),
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		}
		if err := c.AddPost(ctx, "author1", p); err != nil {
			t.Fatalf("AddPost() error = %v", err)
		}
	}

	key := keyPrefix + "author1"
	got, _ := rdb.ZCard(ctx, key).Result()
	if got != 3 {
		t.Fatalf("ZCard = %d, want 3 (incremental, no Del)", got)
	}

	// the newest post (p2, highest score) must be at the top
	top, _ := rdb.ZRevRange(ctx, key, 0, 0).Result()
	if len(top) != 1 || top[0] != "p2" {
		t.Fatalf("top of ZREVRANGE = %v, want [p2]", top)
	}

	// after adding 22 posts (22 total) the set must be trimmed to maxPostsPerAuthor (20)
	for i := 3; i < 22; i++ {
		p := PostEntry{
			PostID:    fmt.Sprintf("p%d", i),
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		}
		if err := c.AddPost(ctx, "author1", p); err != nil {
			t.Fatalf("AddPost() error at i=%d: %v", i, err)
		}
	}

	capped, _ := rdb.ZCard(ctx, key).Result()
	if capped != int64(maxPostsPerAuthor) {
		t.Fatalf("ZCard after 22 adds = %d, want %d (cap)", capped, maxPostsPerAuthor)
	}

	// the newest post (p21) must still be at the top
	topCapped, _ := rdb.ZRevRange(ctx, key, 0, 0).Result()
	if len(topCapped) != 1 || topCapped[0] != "p21" {
		t.Fatalf("top after cap = %v, want [p21]", topCapped)
	}
}

func TestLatestPostsCache_GetRecentPostsByAuthorWithScores(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := NewLatestPostsCache(rdb, time.Hour, logger)
	ctx := context.Background()

	_ = c.AddPost(ctx, "auth1", PostEntry{PostID: "p1", CreatedAt: time.Unix(100, 0)})
	_ = c.AddPost(ctx, "auth1", PostEntry{PostID: "p2", CreatedAt: time.Unix(200, 0)})

	got, err := c.GetRecentPostsByAuthorWithScores(ctx, "auth1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2, got %d", len(got))
	}
	if got[0].PostID != "p2" || !got[0].CreatedAt.Equal(time.Unix(200, 0)) {
		t.Fatalf("first entry wrong (want newest-first p2@200): %+v", got[0])
	}
}
