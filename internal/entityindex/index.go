// Package entityindex is the SOLE owner of the entity → post inverted index:
// key generation, age window, pruning and reading all live here.
//
// Why a single package: the writer (entity scanner) and the reader
// (entityslot) are separate components. Defining the key format and window in two
// places is the shortest way to silently split the contract — the key
// exists in Redis, the reader cannot find it, and no error or metric is produced. The same class of
// bug bit this repo once (trfold's Go/Python implementations could only be kept
// together with the fold_golden.json fixture).
package entityindex

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"serendipity/internal/trfold"
)

const keyPrefix = "serendipity:entity:posts:"

// DefaultWindow is the index's age window. Wider than the 24-hour ANN discovery window but
// not "old content": in a small corpus a single entity may have no post in 24
// hours, the channel silently returns empty and the measurement reads as "entity
// doesn't work".
const DefaultWindow = 72 * time.Hour

// Key produces the Redis key for a canonical entity name.
//
// trfold.Fold is applied: the dictionary canonical name may come back capitalized
// ("Fenerbahçe"), and the profile carries the same name — but a single letter
// difference between the two sides would split the key. Folding is already the only Turkish
// normalization contract in the repo.
func Key(entity string) string { return keyPrefix + trfold.Fold(entity) }

// IndexedKey is the ZSET of ALL indexed posts in the window (member = post_id,
// score = created_at). Its only job is IDF's N: the entity slot scales query weight
// by log(1 + N/df). It is subject to the same pruning/TTL rule as the entity keys. The prefix is deliberately
// NOT `serendipity:entity:posts:` — no
// entity name can fold onto this key.
const IndexedKey = "serendipity:entity:indexed"

// Post is the single post to be indexed.
type Post struct {
	PostID           string
	Entities         []string
	PreviousEntities []string // old snapshot for Replace; Add does not use it.
	CreatedAt        time.Time
}

// OverflowMode selects what to do when a post carries more than maxEntities
// entities.
//
// Why an option: DROPPING turned out expensive when measured. On a real post sample
// with the gate at 3, about 10% of posts never entered the index and the share of posts reachable
// via a non-singleton key stayed low. On the same corpus
// truncation raised it noticeably — the gain comes entirely from "not dropping the post",
// it does NOT MATTER which entities are selected (first-3, most-frequent-3 and rarest-3
// gave the same result within 0.1 points in the measurement).
type OverflowMode string

const (
	// OverflowDrop, the historical behavior: the post never enters the index.
	OverflowDrop OverflowMode = "drop"
	// OverflowTruncate indexes the post with its first maxEntities entities.
	OverflowTruncate OverflowMode = "truncate"
)

// DefaultHardDrop is the DROP threshold in truncate mode instead of truncation.
//
// Truncation alone lets spam slip past layer 1: POST_ENTITY_MAX_COUNT
// is already 8, so a post "stuffed with entities" appears as 4-8 entities
// and gets into the index if truncated. The upper cap protects the edge of that signal.
// Measured cost: a ≤6 truncate / >6 drop policy gave about 2 points lower reachability
// than unconditional truncation, and the remaining posts continued to be dropped.
const DefaultHardDrop = 6

// Writer writes to the inverted index. All writes are collected in a single pipeline.
type Writer struct {
	rdb         goredis.Cmdable
	window      time.Duration
	maxEntities int
	overflow    OverflowMode
	hardDrop    int
	logger      *slog.Logger
}

// Option is an optional Writer setting.
type Option func(*Writer)

// WithOverflow selects the overflow behavior. For hardDrop<=0 DefaultHardDrop is used,
// for hardDrop<maxEntities maxEntities is used (a smaller cap
// would make truncation meaningless: every overflowing post would be dropped anyway).
func WithOverflow(mode OverflowMode, hardDrop int) Option {
	return func(w *Writer) {
		if mode == OverflowTruncate {
			w.overflow = OverflowTruncate
		} else {
			w.overflow = OverflowDrop
		}
		if hardDrop <= 0 {
			hardDrop = DefaultHardDrop
		}
		if hardDrop < w.maxEntities {
			hardDrop = w.maxEntities
		}
		w.hardDrop = hardDrop
	}
}

// NewWriter uses DefaultWindow for window<=0 and 3 for maxEntities<=0.
// A call without options keeps the historical behavior (OverflowDrop).
func NewWriter(rdb goredis.Cmdable, window time.Duration, maxEntities int, logger *slog.Logger, opts ...Option) *Writer {
	if window <= 0 {
		window = DefaultWindow
	}
	if maxEntities <= 0 {
		maxEntities = 3
	}
	if logger == nil {
		logger = slog.Default()
	}
	w := &Writer{
		rdb: rdb, window: window, maxEntities: maxEntities,
		overflow: OverflowDrop, hardDrop: DefaultHardDrop, logger: logger,
	}
	for _, o := range opts {
		o(w)
	}
	if w.hardDrop < w.maxEntities {
		w.hardDrop = w.maxEntities
	}
	return w
}

// Add writes the given posts to the index, prunes the keys it touched and
// refreshes their TTLs.
//
// Pruning and TTL in the same pipeline: this way no separate cleanup CronJob is
// NEEDED. ZREMRANGEBYSCORE bounds the ZSET size by the window and EXPIRE
// drops a fully dead entity key.
func (w *Writer) Add(ctx context.Context, posts []Post) error {
	return w.write(ctx, posts, false)
}

// Replace removes old entity members and writes the new
// snapshot in the same Redis pipeline on a re-scan of entities. This way, when the dictionary/NER changes,
// no ghost posts remain under old entity keys.
func (w *Writer) Replace(ctx context.Context, posts []Post) error {
	return w.write(ctx, posts, true)
}

func (w *Writer) write(ctx context.Context, posts []Post, replace bool) error {
	if len(posts) == 0 {
		return nil
	}

	cutoff := time.Now().Add(-w.window).Unix()
	pipe := w.rdb.Pipeline()
	touched := make(map[string]struct{})
	indexed := 0

	for _, p := range posts {
		if p.PostID == "" {
			continue
		}
		if replace {
			for _, old := range p.PreviousEntities {
				if old == "" {
					continue
				}
				key := Key(old)
				pipe.ZRem(ctx, key, p.PostID)
				touched[key] = struct{}{}
			}
			pipe.ZRem(ctx, IndexedKey, p.PostID)
			touched[IndexedKey] = struct{}{}
		}
		if len(p.Entities) == 0 {
			continue
		}
		// Spam layer 1: entity density is a direct signature of spam.
		// The post:data.entities field is NOT TOUCHED (POST_ENTITY_MAX_COUNT
		// stays as is) — the only thing cut is discovery reach.
		ents := p.Entities
		if len(ents) > w.maxEntities {
			if w.overflow != OverflowTruncate || len(ents) > w.hardDrop {
				PostSkipped.WithLabelValues("too_many_entities").Inc()
				continue
			}
			ents = ents[:w.maxEntities]
			PostTruncated.Inc()
		}
		if p.CreatedAt.IsZero() {
			PostSkipped.WithLabelValues("no_created_at").Inc()
			continue
		}
		if p.CreatedAt.Unix() <= cutoff {
			// If we wrote it, it would be deleted at the next pruning anyway.
			PostSkipped.WithLabelValues("too_old").Inc()
			continue
		}

		score := float64(p.CreatedAt.Unix())
		for _, e := range ents {
			key := Key(e)
			pipe.ZAdd(ctx, key, goredis.Z{Score: score, Member: p.PostID})
			touched[key] = struct{}{}
		}
		pipe.ZAdd(ctx, IndexedKey, goredis.Z{Score: score, Member: p.PostID})
		touched[IndexedKey] = struct{}{}
		indexed++
	}

	if len(touched) == 0 {
		return nil
	}
	for key := range touched {
		pipe.ZRemRangeByScore(ctx, key, "-inf", fmt.Sprintf("(%d", cutoff))
		pipe.Expire(ctx, key, 2*w.window)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("entityindex add: %w", err)
	}

	PostsIndexed.Add(float64(indexed))
	w.logger.Debug("entityindex written",
		slog.Int("posts", indexed),
		slog.Int("keys", len(touched)),
	)
	return nil
}

// Reader reads from the inverted index with a window.
type Reader struct {
	rdb    goredis.Cmdable
	window time.Duration
}

// NewReader uses DefaultWindow for window<=0.
func NewReader(rdb goredis.Cmdable, window time.Duration) *Reader {
	if window <= 0 {
		window = DefaultWindow
	}
	return &Reader{rdb: rdb, window: window}
}

// DocFreq reads the IDF inputs in a SINGLE pipeline: each entity's number of
// posts in the window (df) and the total number of indexed posts in the window (N). An entity
// with no key returns 0.
func (r *Reader) DocFreq(ctx context.Context, entities []string) (map[string]int64, int64, error) {
	df := make(map[string]int64, len(entities))
	minScore := fmt.Sprintf("(%d", time.Now().Add(-r.window).Unix())
	pipe := r.rdb.Pipeline()
	total := pipe.ZCount(ctx, IndexedKey, minScore, "+inf")
	cmds := make(map[string]*goredis.IntCmd, len(entities))
	for _, e := range entities {
		if e == "" {
			continue
		}
		cmds[e] = pipe.ZCount(ctx, Key(e), minScore, "+inf")
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, goredis.Nil) {
		return nil, 0, fmt.Errorf("entityindex docfreq: %w", err)
	}
	for e, c := range cmds {
		df[e] = c.Val()
	}
	return df, total.Val(), nil
}

// RecentBatch reads the entity → limit map in a SINGLE pipeline and returns entity →
// post IDs (created_at descending).
//
// An entity with an empty pool does NOT enter the result at all: for the caller the distinction between "no key" and
// "no posts in the window" is meaningless, the contribution is zero either way.
func (r *Reader) RecentBatch(ctx context.Context, limits map[string]int) (map[string][]string, error) {
	out := make(map[string][]string, len(limits))
	if len(limits) == 0 {
		return out, nil
	}

	minScore := fmt.Sprintf("(%d", time.Now().Add(-r.window).Unix())
	pipe := r.rdb.Pipeline()
	entities := make([]string, 0, len(limits))
	cmds := make([]*goredis.StringSliceCmd, 0, len(limits))

	for e, n := range limits {
		if e == "" || n <= 0 {
			continue
		}
		entities = append(entities, e)
		cmds = append(cmds, pipe.ZRevRangeByScore(ctx, Key(e), &goredis.ZRangeBy{
			Min:    minScore,
			Max:    "+inf",
			Offset: 0,
			Count:  int64(n),
		}))
	}
	if len(cmds) == 0 {
		return out, nil
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, goredis.Nil) {
		return nil, fmt.Errorf("entityindex recent: %w", err)
	}

	for i, c := range cmds {
		ids, err := c.Result()
		if err != nil || len(ids) == 0 {
			continue
		}
		out[entities[i]] = ids
	}
	return out, nil
}
