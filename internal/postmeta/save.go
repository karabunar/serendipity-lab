package postmeta

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	goredis "github.com/redis/go-redis/v9"

	"serendipity/internal/core"
)

// CategorySkipped counts the writes SetCategoryBatch skipped for a post that is NOT
// indexed.
var CategorySkipped = promauto.NewCounter(prometheus.CounterOpts{
	Name: "serendipity_postmeta_category_skipped_total",
	Help: "Number of writes SetCategoryBatch skipped because the post was not indexed.",
})

// SaveBatch writes raw posts as a HASH (text, author, creation time).
// An empty PostID is skipped; a single corrupt post does not make the batch write fail.
func (s *Store) SaveBatch(ctx context.Context, posts []core.RawPost) error {
	if len(posts) == 0 {
		return nil
	}
	pipe := s.rdb.Pipeline()
	for _, p := range posts {
		if p.PostID == "" {
			continue
		}
		k := s.key(p.PostID)
		pipe.HSet(ctx, k, encodeHashFields(p))
		pipe.Expire(ctx, k, s.ttl)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return wrapErr("SaveBatch", "", err)
	}
	s.log().Debug("postmeta save", slog.Int("count", len(posts)), slog.Duration("ttl", s.ttl))
	return nil
}

func encodeHashFields(p core.RawPost) map[string]any {
	createdAt := ""
	if !p.CreatedAt.IsZero() {
		createdAt = p.CreatedAt.Format(time.RFC3339)
	}
	fields := map[string]any{
		fieldText:      p.Text,
		fieldCreatedAt: createdAt,
	}
	// Do not write the empty author_id field at all: so the identity marker can be filled in later.
	if p.AuthorID != "" {
		fields[fieldAuthorID] = p.AuthorID
	}
	return fields
}

// setCategoryIfIndexedScript writes the category field only if the post is REALLY
// indexed and refreshes the TTL; otherwise it does nothing (return 0).
// A plain EXISTS is not enough: an identity-less "shell" hash also says EXISTS, so
// created_at or author_id is looked for as the marker.
//
// KEYS[1]=key  ARGV[1]=category field  ARGV[2]=value  ARGV[3]=ttl seconds
// ARGV[4],ARGV[5]=marker fields. Returns: 1=written, 0=skipped.
const setCategoryIfIndexedScript = `
if redis.call('HEXISTS', KEYS[1], ARGV[4]) == 0
   and redis.call('HEXISTS', KEYS[1], ARGV[5]) == 0 then
  return 0
end
redis.call('HSET', KEYS[1], ARGV[1], ARGV[2])
redis.call('EXPIRE', KEYS[1], ARGV[3])
return 1
`

// SetCategoryBatch writes the post_id → category mapping only to indexed posts.
// Empty values are silently skipped.
func (s *Store) SetCategoryBatch(ctx context.Context, postIDToCategory map[string]string) error {
	if len(postIDToCategory) == 0 {
		return nil
	}
	ttlSec := int(s.ttl.Seconds())
	pipe := s.rdb.Pipeline()
	cmds := make([]*goredis.Cmd, 0, len(postIDToCategory))
	for postID, category := range postIDToCategory {
		if postID == "" || category == "" {
			continue
		}
		cmds = append(cmds, pipe.Eval(ctx, setCategoryIfIndexedScript, []string{s.key(postID)}, fieldCategory, category, ttlSec, fieldCreatedAt, fieldAuthorID))
	}
	if len(cmds) == 0 {
		return nil
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return wrapErr("SetCategoryBatch", "", err)
	}
	for _, c := range cmds {
		if v, err := c.Int(); err == nil && v == 0 {
			CategorySkipped.Inc()
		}
	}
	return nil
}
