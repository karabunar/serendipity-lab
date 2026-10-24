// Package postmeta is a small store that reads and writes the entity fields on the post HASH.
// It covers only the part used by the entity pipeline (no media
// hydration or link signal).
package postmeta

import (
	"fmt"
	"log/slog"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// KeyPrefix is the key prefix of the post data HASHes.
const KeyPrefix = "serendipity:post:data:"

// ScanPattern is the SCAN pattern that scans all post data HASHes.
func ScanPattern() string { return KeyPrefix + "*" }

const (
	keyPrefix = KeyPrefix

	fieldAuthorID  = "author_id"
	fieldText      = "text"
	fieldEntities  = "entities"
	fieldCategory  = "category"
	fieldCreatedAt = "created_at"

	// DefaultTTL is how long post data is kept in Redis.
	DefaultTTL = 7 * 24 * time.Hour
)

// Config carries the Store parameters.
type Config struct {
	TTL time.Duration // 0 → DefaultTTL
}

// Store reads/writes on KeyPrefix+{id} HASHes. The go-redis client
// is supplied from outside (miniredis in tests).
type Store struct {
	rdb    goredis.Cmdable
	ttl    time.Duration
	logger *slog.Logger
}

// New returns a new Store with the given Redis client and config.
func New(rdb goredis.Cmdable, cfg Config, logger *slog.Logger) *Store {
	ttl := cfg.TTL
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return &Store{rdb: rdb, ttl: ttl, logger: logger}
}

func (s *Store) key(postID string) string { return keyPrefix + postID }

// Key returns the full Redis key for a post_id.
func (s *Store) Key(postID string) string { return s.key(postID) }

// TTL returns the effective TTL value.
func (s *Store) TTL() time.Duration { return s.ttl }

func (s *Store) log() *slog.Logger {
	if s.logger != nil {
		return s.logger
	}
	return slog.Default()
}

func wrapErr(op string, postID string, err error) error {
	if postID == "" {
		return fmt.Errorf("postmeta %s: %w", op, err)
	}
	return fmt.Errorf("postmeta %s %q: %w", op, postID, err)
}
