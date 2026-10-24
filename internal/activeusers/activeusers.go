// Package activeusers is a reader that pages active user IDs from a Redis ZSET
// (member = user ID, score = last-seen time).
package activeusers

import (
	"context"

	goredis "github.com/redis/go-redis/v9"
)

// DefaultKey is the default key of the active-user ZSET.
const DefaultKey = "serendipity:users:active"

// ZSet is a ZSET-backed pager.
type ZSet struct {
	rdb goredis.Cmdable
	key string
}

// New uses DefaultKey if key is empty.
func New(rdb goredis.Cmdable, key string) *ZSet {
	if key == "" {
		key = DefaultKey
	}
	return &ZSet{rdb: rdb, key: key}
}

// PageByIndex returns the range [offset, offset+count); once offset exceeds the set size
// an empty slice comes back, so the paging loop ends cleanly.
func (z *ZSet) PageByIndex(ctx context.Context, offset, count int) ([]string, error) {
	if count <= 0 || offset < 0 {
		return nil, nil
	}
	return z.rdb.ZRange(ctx, z.key, int64(offset), int64(offset+count-1)).Result()
}

// All reads all users page by page and returns them in a single slice.
func (z *ZSet) All(ctx context.Context, pageSize int) ([]string, error) {
	if pageSize <= 0 {
		pageSize = 500
	}
	var out []string
	for offset := 0; ; offset += pageSize {
		page, err := z.PageByIndex(ctx, offset, pageSize)
		if err != nil {
			return out, err
		}
		if len(page) == 0 {
			return out, nil
		}
		out = append(out, page...)
	}
}
