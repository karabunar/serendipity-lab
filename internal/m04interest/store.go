package m04interest

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

const keyPrefix = "serendipity:user:interest:"

// DefaultTTL is the lifetime of the profile in Redis. As long as the user stays active
// it is refreshed on every run; the profile of a user who goes completely silent
// naturally expires and takes no space (memory is limited).
const DefaultTTL = 30 * 24 * time.Hour

// ProfileKey returns the user's profile key.
func ProfileKey(userID string) string { return keyPrefix + userID }

// RedisStore reads/writes serendipity:user:interest:{uid}.
type RedisStore struct {
	rdb    goredis.Cmdable
	ttl    time.Duration
	logger *slog.Logger
}

// NewRedisStore uses DefaultTTL for ttl<=0.
func NewRedisStore(rdb goredis.Cmdable, ttl time.Duration, logger *slog.Logger) *RedisStore {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &RedisStore{rdb: rdb, ttl: ttl, logger: logger}
}

// Get returns the profile. The following THREE cases also return an EMPTY profile and are NOT AN ERROR —
// all of them mean "build from scratch":
//   - key missing (first run)
//   - JSON corrupt (manual intervention / half write)
//   - schema version old (field meanings may have changed)
//
// Making these errors would mean a single corrupt record blocking the user
// PERMANENTLY; the profile is a derivative that can be rebuilt anyway.
func (s *RedisStore) Get(ctx context.Context, userID string) (Profile, error) {
	raw, err := s.rdb.Get(ctx, ProfileKey(userID)).Result()
	if err != nil {
		if errors.Is(err, goredis.Nil) {
			return Profile{}, nil
		}
		return Profile{}, err
	}

	var p Profile
	if uErr := json.Unmarshal([]byte(raw), &p); uErr != nil {
		s.logger.Warn("interest profile corrupt — will be rebuilt from scratch",
			slog.String("user_id", userID),
			slog.String("error", uErr.Error()))
		return Profile{}, nil
	}
	if p.Version != SchemaVersion {
		s.logger.Info("interest profile has old schema — will be rebuilt from scratch",
			slog.String("user_id", userID),
			slog.Int("found", p.Version),
			slog.Int("want", SchemaVersion))
		return Profile{}, nil
	}
	return p, nil
}

// Save writes the profile with a TTL.
func (s *RedisStore) Save(ctx context.Context, userID string, p Profile) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return s.rdb.Set(ctx, ProfileKey(userID), raw, s.ttl).Err()
}
