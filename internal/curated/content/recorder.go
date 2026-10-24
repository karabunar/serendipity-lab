package content

import (
	"context"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"serendipity/internal/curated/model"
)

// curatedSeenKeyPrefix is the key prefix of the per-user curated-seen set;
// any other reader of that key must use the same value.
const curatedSeenKeyPrefix = "serendipity:curated:seen:"

// Recorder writes injected curated posts to Redis: only the frequency
// cap (serendipity:curated:seen:{uid}). Difference from whitelistcontent.Recorder:
// there is NO impression/stats HASH (spec §3.4 — out of v1 scope, YAGNI). Best-effort
// — errors are logged silently, it never blocks/affects the caller.
type Recorder struct {
	rdb     redis.Cmdable
	seenTTL time.Duration
	logger  *slog.Logger
}

// NewRecorder creates a Recorder. If seenTTL<=0, the 24h default is used.
func NewRecorder(rdb redis.Cmdable, seenTTL time.Duration, logger *slog.Logger) *Recorder {
	if logger == nil {
		logger = slog.Default()
	}
	if seenTTL <= 0 {
		seenTTL = 24 * time.Hour
	}
	return &Recorder{rdb: rdb, seenTTL: seenTTL, logger: logger}
}

// RecordInjections does a SADD to the seen-set for each injected candidate
// (single Redis pipeline, best-effort). No-op if injected is empty.
func (r *Recorder) RecordInjections(ctx context.Context, userID string, injected []model.ScoredCandidate) {
	if len(injected) == 0 {
		return
	}
	seenKey := curatedSeenKeyPrefix + userID

	pipe := r.rdb.Pipeline()
	for _, sc := range injected {
		pipe.SAdd(ctx, seenKey, sc.Candidate.PostID)
	}
	pipe.Expire(ctx, seenKey, r.seenTTL)
	if _, err := pipe.Exec(ctx); err != nil {
		r.logger.Warn("curated injection recording failed (best-effort)",
			slog.String("user_id", userID), slog.String("error", err.Error()))
	}
}
