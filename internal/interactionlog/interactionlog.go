// Package interactionlog is the single source of user interactions: each event is written as JSON
// to a bounded per-user Redis LIST (newest first), and the three read surfaces that other components
// expect are derived from this log:
//
//   - GetRecent                → interestsrc.RingReader (positive signals)
//   - RecentPostIDs            → niche.Interactions (niche profiler)
//   - FetchUserAnalytics       → interestsrc.MirrorReader (skip → negative signals)
//   - GetFavoriteAuthors       → interestsrc.FavoriteReader (author affinity)
//
// The log is bounded (MaxLen): skip counters and author affinity derive only from this window.
package interactionlog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"serendipity/internal/analytics"
	"serendipity/internal/core"
	"serendipity/internal/interactions"
)

const (
	// KeyPrefix is the key prefix of the user log (LIST, item = RawInteraction JSON).
	KeyPrefix = "serendipity:user:interactions:"
	// DefaultMaxLen is the maximum number of events kept per user.
	DefaultMaxLen = 200
	// DefaultTTL is the lifetime of the log after the last write.
	DefaultTTL = 30 * 24 * time.Hour
	// MaxFavorites is the maximum number of authors GetFavoriteAuthors returns.
	MaxFavorites = 50

	dayLayout = "2006-01-02"
)

// weights is the signal weight by event type (bounded range: 0 or [0.5, 2.0]).
// Skip is not a positive signal; its weight is 0 and it only feeds the negative side.
var weights = map[string]float64{
	interactions.InteractionLike:            1.0,
	interactions.InteractionReblog:          2.0,
	interactions.InteractionReply:           0.5,
	interactions.InteractionDwell:           0.5,
	interactions.InteractionImageView:       0.5,
	interactions.InteractionDetailView:      0.75,
	interactions.InteractionVideoWatch:      0.75,
	interactions.InteractionVideoCompletion: 1.0,
	interactions.InteractionSkip:            0,
}

// Weight returns the default signal weight for a type; ok=false for an unknown type.
func Weight(interactionType string) (w float64, ok bool) {
	w, ok = weights[interactionType]
	return w, ok
}

// Log is the interaction log.
type Log struct {
	rdb    goredis.Cmdable
	maxLen int
	ttl    time.Duration
	now    func() time.Time
}

// New uses the defaults for maxLen<=0 and ttl<=0.
func New(rdb goredis.Cmdable, maxLen int, ttl time.Duration) *Log {
	if maxLen <= 0 {
		maxLen = DefaultMaxLen
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return &Log{rdb: rdb, maxLen: maxLen, ttl: ttl, now: time.Now}
}

// Validate checks whether an event can be written (ids, type, value).
func Validate(it interactions.RawInteraction) error {
	if it.UserID == "" || it.PostID == "" {
		return errors.New("interactionlog: user_id and post_id are required")
	}
	if _, ok := Weight(it.InteractionType); !ok {
		return fmt.Errorf("interactionlog: unknown interaction type %q", it.InteractionType)
	}
	if it.Value < 0 {
		return errors.New("interactionlog: negative value")
	}
	return nil
}

// normalize validates the event and fills in missing fields (time → now, weight → type table).
func (l *Log) normalize(it interactions.RawInteraction) (interactions.RawInteraction, error) {
	if err := Validate(it); err != nil {
		return it, err
	}
	if it.Timestamp.IsZero() {
		it.Timestamp = l.now()
	}
	if it.Value == 0 {
		it.Value, _ = Weight(it.InteractionType)
	}
	return it, nil
}

// Record writes a single event. An invalid event returns an error and writes nothing.
func (l *Log) Record(ctx context.Context, it interactions.RawInteraction) error {
	return l.RecordBatch(ctx, []interactions.RawInteraction{it})
}

// RecordBatch writes the events in a single pipeline; if one is invalid, none is written.
// Events of the same user are appended in the given order (the last one counts as the newest).
func (l *Log) RecordBatch(ctx context.Context, items []interactions.RawInteraction) error {
	if len(items) == 0 {
		return nil
	}
	type enc struct {
		user string
		raw  []byte
	}
	encoded := make([]enc, 0, len(items))
	for _, it := range items {
		n, err := l.normalize(it)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(n)
		if err != nil {
			return err
		}
		encoded = append(encoded, enc{n.UserID, raw})
	}
	pipe := l.rdb.Pipeline()
	touched := map[string]struct{}{}
	for _, e := range encoded {
		key := KeyPrefix + e.user
		pipe.LPush(ctx, key, e.raw)
		touched[key] = struct{}{}
	}
	for key := range touched {
		pipe.LTrim(ctx, key, 0, int64(l.maxLen-1))
		pipe.Expire(ctx, key, l.ttl)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("interactionlog: write: %w", err)
	}
	return nil
}

// GetRecent returns at most limit events, newest first (limit<=0 → MaxLen).
// Corrupt items are skipped.
func (l *Log) GetRecent(ctx context.Context, userID string, limit int) ([]interactions.RawInteraction, error) {
	if limit <= 0 || limit > l.maxLen {
		limit = l.maxLen
	}
	raw, err := l.rdb.LRange(ctx, KeyPrefix+userID, 0, int64(limit-1)).Result()
	if err != nil {
		return nil, err
	}
	out := make([]interactions.RawInteraction, 0, len(raw))
	for _, r := range raw {
		var it interactions.RawInteraction
		if json.Unmarshal([]byte(r), &it) != nil || it.PostID == "" {
			continue
		}
		out = append(out, it)
	}
	return out, nil
}

// RecentPostIDs returns the deduplicated post ids of non-skip events (newest first).
// Limit bounds the number of events scanned. Skip is not an indicator of interest; it does not enter niche inference.
func (l *Log) RecentPostIDs(ctx context.Context, userID string, limit int) ([]string, error) {
	items, err := l.GetRecent(ctx, userID, limit)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(items))
	ids := make([]string, 0, len(items))
	for _, it := range items {
		if it.InteractionType == interactions.InteractionSkip {
			continue
		}
		if _, dup := seen[it.PostID]; dup {
			continue
		}
		seen[it.PostID] = struct{}{}
		ids = append(ids, it.PostID)
	}
	return ids, nil
}

// FetchUserAnalytics summarizes skip events within the last `days` days per (day, post).
// Only records carrying a skip are returned; days are UTC. days<=0 → 30.
func (l *Log) FetchUserAnalytics(ctx context.Context, userID string, days int) ([]analytics.AnalyticRecord, error) {
	if days <= 0 {
		days = 30
	}
	items, err := l.GetRecent(ctx, userID, l.maxLen)
	if err != nil {
		return nil, err
	}
	cutoff := l.now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -(days - 1))
	type key struct{ day, post string }
	agg := map[key]*analytics.AnalyticRecord{}
	var order []key
	for _, it := range items {
		if it.InteractionType != interactions.InteractionSkip {
			continue
		}
		ts := it.Timestamp.UTC()
		if ts.Before(cutoff) {
			continue
		}
		k := key{ts.Format(dayLayout), it.PostID}
		rec, ok := agg[k]
		if !ok {
			rec = &analytics.AnalyticRecord{UserID: userID, ContentID: it.PostID, PostID: it.PostID, AuthorID: it.AuthorID, Day: k.day, Skip: &analytics.SkipAnalytics{}}
			agg[k] = rec
			order = append(order, k)
		}
		rec.Skip.Count++
		rec.Skip.DwellMsSum += it.DwellMs
		if ms := ts.UnixMilli(); ms > rec.LastTsMs {
			rec.LastTsMs = ms
		}
	}
	out := make([]analytics.AnalyticRecord, 0, len(order))
	for _, k := range order {
		out = append(out, *agg[k])
	}
	return out, nil
}

// GetFavoriteAuthors derives affinity from the per-author weight sum of positive events:
// the score is normalized to [0,1] relative to the closest author; at most MaxFavorites authors are
// returned in descending order of score (ties by author id). Events with an unknown author are not counted.
func (l *Log) GetFavoriteAuthors(ctx context.Context, userID string) ([]core.AuthorAffinity, error) {
	items, err := l.GetRecent(ctx, userID, l.maxLen)
	if err != nil {
		return nil, err
	}
	sum := map[string]float64{}
	cnt := map[string]int{}
	for _, it := range items {
		if it.AuthorID == "" || it.InteractionType == interactions.InteractionSkip || it.Value <= 0 {
			continue
		}
		sum[it.AuthorID] += it.Value
		cnt[it.AuthorID]++
	}
	if len(sum) == 0 {
		return nil, nil
	}
	var max float64
	out := make([]core.AuthorAffinity, 0, len(sum))
	for a, s := range sum {
		if s > max {
			max = s
		}
		out = append(out, core.AuthorAffinity{AuthorID: a, InteractionCount: cnt[a], AffinityScore: s})
	}
	for i := range out {
		out[i].AffinityScore /= max
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].AffinityScore != out[j].AffinityScore {
			return out[i].AffinityScore > out[j].AffinityScore
		}
		return out[i].AuthorID < out[j].AuthorID
	})
	if len(out) > MaxFavorites {
		out = out[:MaxFavorites]
	}
	return out, nil
}
