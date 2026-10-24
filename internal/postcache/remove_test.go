package postcache

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newTestCache returns a miniredis-backed LatestPostsCache.
func newTestCache(t *testing.T) (*LatestPostsCache, *redis.Client) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewLatestPostsCache(rdb, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil))), rdb
}

// RemovePost removes a post from the author's latest-posts index
// (the index leg of the post.deleted tombstone).
//
// The existing `ZRemRangeByRank` does NOT cover this: it is a rank-based trim, i.e.
// it only enforces the "newest N" limit. If a deleted post is inside that N
// it stays there, and the favorite-author and F2 retrieval keep pulling it
// as a candidate.
func TestRemovePost_RemovesMember(t *testing.T) {
	c, rdb := newTestCache(t)
	ctx := context.Background()

	for _, p := range []PostEntry{
		{PostID: "p1", CreatedAt: time.Unix(100, 0)},
		{PostID: "p2", CreatedAt: time.Unix(200, 0)},
	} {
		if err := c.AddPost(ctx, "a1", p); err != nil {
			t.Fatal(err)
		}
	}

	if err := c.RemovePost(ctx, "a1", "p1"); err != nil {
		t.Fatalf("RemovePost() error = %v", err)
	}

	got, err := c.GetRecentPostsByAuthor(ctx, "a1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "p2" {
		t.Fatalf("only p2 must remain, got %v", got)
	}
	_ = rdb
}

// IDEMPOTENT: the tombstone is delivered at-least-once.
func TestRemovePost_MissingMember_NoError(t *testing.T) {
	c, _ := newTestCache(t)
	ctx := context.Background()
	if err := c.AddPost(ctx, "a1", PostEntry{PostID: "p1", CreatedAt: time.Unix(100, 0)}); err != nil {
		t.Fatal(err)
	}
	if err := c.RemovePost(ctx, "a1", "yok"); err != nil {
		t.Fatalf("deleting a nonexistent member must not be an error: %v", err)
	}
	if err := c.RemovePost(ctx, "bilinmeyen-yazar", "p1"); err != nil {
		t.Fatalf("nonexistent key must not be an error: %v", err)
	}
}

// Empty arguments are a no-op — the `serendipity:author:posts:` (prefix-only) key
// must not be touched.
func TestRemovePost_EmptyArgs_NoOp(t *testing.T) {
	c, _ := newTestCache(t)
	ctx := context.Background()
	if err := c.RemovePost(ctx, "", "p1"); err != nil {
		t.Fatalf("empty author must not be an error: %v", err)
	}
	if err := c.RemovePost(ctx, "a1", ""); err != nil {
		t.Fatalf("empty post must not be an error: %v", err)
	}
}
