// Package postcache caches the latest posts of authors in a Redis Sorted Set.
//
// Diagram reference: "Latest Posts Cache (Redis): Author's last 24h post IDs"
//
// Slot 1 (FavoriteAuthorsFetcher) is read from Redis instead of PostgreSQL
// because of the 20ms SLA requirement. After the affinity pipeline runs, the post caches
// of the referenced authors are refreshed.
//
// Redis key: serendipity:author:posts:{author_id} (Sorted Set, score = Unix timestamp)
package postcache

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

const keyPrefix = "serendipity:author:posts:"

// maxPostsPerAuthor is the maximum number of posts kept in each author's sorted set.
// Consistent with PostCacheRefresher (default postsPerAuthor=20).
const maxPostsPerAuthor = 20

// LatestPostsCache manages authors' latest posts with a Redis Sorted Set.
type LatestPostsCache struct {
	rdb    *redis.Client
	ttl    time.Duration
	logger *slog.Logger
}

// NewLatestPostsCache creates a cache with a Redis client and a TTL.
func NewLatestPostsCache(rdb *redis.Client, ttl time.Duration, logger *slog.Logger) *LatestPostsCache {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &LatestPostsCache{rdb: rdb, ttl: ttl, logger: logger}
}

// PostEntry is the post information to be written to the Redis Sorted Set.
type PostEntry struct {
	PostID    string
	CreatedAt time.Time
}

// RefreshAuthor writes the author's latest posts to the Redis Sorted Set.
// If the key exists it is reset and rewritten.
func (c *LatestPostsCache) RefreshAuthor(ctx context.Context, authorID string, posts []PostEntry) error {
	if len(posts) == 0 {
		return nil
	}

	key := keyPrefix + authorID

	pipe := c.rdb.Pipeline()
	pipe.Del(ctx, key)

	members := make([]redis.Z, len(posts))
	for i, p := range posts {
		members[i] = redis.Z{
			Score:  float64(p.CreatedAt.Unix()),
			Member: p.PostID,
		}
	}
	pipe.ZAdd(ctx, key, members...)
	pipe.Expire(ctx, key, c.ttl)

	_, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("refresh author %s: %w", authorID, err)
	}

	c.logger.Debug("author post cache refreshed",
		slog.String("author_id", authorID),
		slog.Int("posts", len(posts)),
	)
	return nil
}

// GetRecentPostsByAuthor fetches the author's latest N posts from Redis.
// Compatible with favorites.IPostCacheRepository.GetRecentPostsByAuthor.
func (c *LatestPostsCache) GetRecentPostsByAuthor(ctx context.Context, authorID string, limit int) ([]string, error) {
	key := keyPrefix + authorID

	// ZREVRANGE: the highest score (newest) comes first.
	result, err := c.rdb.ZRevRange(ctx, key, 0, int64(limit-1)).Result()
	if err != nil {
		return nil, fmt.Errorf("get recent posts for %s: %w", authorID, err)
	}
	return result, nil
}

// GetRecentPostsByAuthorWithScores returns the author's latest N posts newest-first
// together with created_at (ZSET score). The social slot builds SocialActivity.Timestamp
// from this score (GAP-B; instead of PG GetAuthorsRecentPosts).
func (c *LatestPostsCache) GetRecentPostsByAuthorWithScores(ctx context.Context, authorID string, limit int) ([]PostEntry, error) {
	key := keyPrefix + authorID
	zs, err := c.rdb.ZRevRangeWithScores(ctx, key, 0, int64(limit-1)).Result()
	if err != nil {
		return nil, fmt.Errorf("get recent posts w/ scores for %s: %w", authorID, err)
	}
	out := make([]PostEntry, 0, len(zs))
	for _, z := range zs {
		m, _ := z.Member.(string)
		if m == "" {
			continue
		}
		out = append(out, PostEntry{PostID: m, CreatedAt: time.Unix(int64(z.Score), 0)})
	}
	return out, nil
}

// AddPost incrementally adds a single post to the author's sorted set (does NOT Del).
// After adding, it limits the set to the latest maxPostsPerAuthor posts (the oldest excess is dropped).
// Called by the event-driven post.created handler (GAP-B author index).
func (c *LatestPostsCache) AddPost(ctx context.Context, authorID string, p PostEntry) error {
	key := keyPrefix + authorID

	pipe := c.rdb.Pipeline()
	pipe.ZAdd(ctx, key, redis.Z{
		Score:  float64(p.CreatedAt.Unix()),
		Member: p.PostID,
	})
	// delete the rank 0..-(maxPostsPerAuthor+1) range → the newest maxPostsPerAuthor remain.
	pipe.ZRemRangeByRank(ctx, key, 0, int64(-maxPostsPerAuthor-1))
	pipe.Expire(ctx, key, c.ttl)

	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("addpost author=%s: %w", authorID, err)
	}

	c.logger.Debug("author post cache incremental add",
		slog.String("author_id", authorID),
		slog.String("post_id", p.PostID),
	)
	return nil
}

// RemovePost removes a single post from the author's sorted set (ZREM).
// It is the index leg of the `post.deleted` tombstone — the inverse of AddPost.
//
// WHY ZRemRangeByRank in AddPost IS NOT ENOUGH: it is a rank-based TRIM,
// it only enforces the "newest maxPostsPerAuthor" limit. If the deleted post is inside that
// window it stays there, and the favorite-author and F2 retrieval keep pulling it
// as a candidate. The key's TTL cannot be waited on either: every AddPost
// refreshes EXPIRE, so for an active author the key lives indefinitely.
//
// IDEMPOTENT: for a nonexistent member or key ZREM returns 0, not an error.
// The tombstone is delivered at-least-once; if it returned an error, every redelivery would
// produce an error. The TTL is deliberately NOT refreshed — deletion is not a write, it is a rollback.
func (c *LatestPostsCache) RemovePost(ctx context.Context, authorID, postID string) error {
	if authorID == "" || postID == "" {
		return nil
	}
	if err := c.rdb.ZRem(ctx, keyPrefix+authorID, postID).Err(); err != nil {
		return fmt.Errorf("removepost author=%s post=%s: %w", authorID, postID, err)
	}
	c.logger.Debug("author post cache remove",
		slog.String("author_id", authorID),
		slog.String("post_id", postID),
	)
	return nil
}
