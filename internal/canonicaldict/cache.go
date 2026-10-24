package canonicaldict

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// Redis keys — the writing side is python/build_canonical_dict.py.
const (
	KeyAlias  = "serendipity:canonical:alias"
	KeyEntity = "serendipity:canonical:entity"
	KeyMeta   = "serendipity:canonical:meta"
)

// DefaultRefreshInterval is the dictionary refresh period. Since the dictionary is produced rarely by an offline
// script, frequent refreshing has no benefit.
const DefaultRefreshInterval = 30 * time.Minute

// Loader is the source that fetches the whole dictionary in one go (DIP). A fake impl
// is used in tests.
type Loader interface {
	// Load hands OWNERSHIP of the returned map to the Cache: the map
	// is put into the snapshot without copying and concurrent Match readers
	// read it directly. The Loader MUST NOT MODIFY the map it returned again
	// and MUST NOT REUSE it — a Match reading a mutated map
	// kills the process with "fatal error: concurrent map read and map write";
	// this is not a soft failure.
	Load(ctx context.Context) (map[string]string, error)
}

// MetaLoader is an OPTIONAL extension of Loader: sources that can also provide the dictionary's production time
// satisfy it (optional extension interface pattern). A Loader that does not satisfy it loses
// nothing — only the age gauge is not set.
type MetaLoader interface {
	// LoadMeta returns the :meta.generated_at value. If the key is missing or
	// cannot be parsed it returns (zero time, nil error): this is NOT AN ERROR,
	// it only means "age unknown".
	LoadMeta(ctx context.Context) (time.Time, error)
}

// RedisLoader reads the serendipity:canonical:alias HASH with a single HGETALL.
type RedisLoader struct {
	rdb goredis.Cmdable
}

func NewRedisLoader(rdb goredis.Cmdable) *RedisLoader {
	return &RedisLoader{rdb: rdb}
}

// Load returns the dictionary. If the key does NOT EXIST it returns an empty map and gives NO error:
// the dictionary may not have been produced yet and that means the normalization layer
// is disabled — not an error.
func (l *RedisLoader) Load(ctx context.Context) (map[string]string, error) {
	out, err := l.rdb.HGetAll(ctx, KeyAlias).Result()
	if err != nil && !errors.Is(err, goredis.Nil) {
		return nil, err
	}
	if out == nil {
		return map[string]string{}, nil
	}
	return out, nil
}

// metaDocument is the part of the serendipity:canonical:meta STRING we care about.
// The writing side is python/canonical_dict/redis_sink.py.
type metaDocument struct {
	GeneratedAt string `json:"generated_at"`
}

// LoadMeta returns the dictionary's production time. If the key is missing, the JSON is corrupt or
// the timestamp cannot be parsed it returns (zero, nil) — the caller does NOT SET
// the age gauge. A Redis error is a real error and is passed up.
func (l *RedisLoader) LoadMeta(ctx context.Context) (time.Time, error) {
	raw, err := l.rdb.Get(ctx, KeyMeta).Result()
	if errors.Is(err, goredis.Nil) {
		return time.Time{}, nil // the dictionary has not been produced yet
	}
	if err != nil {
		return time.Time{}, err
	}
	var doc metaDocument
	if jsonErr := json.Unmarshal([]byte(raw), &doc); jsonErr != nil {
		return time.Time{}, nil
	}
	// Python isoformat(timespec="seconds") produces RFC3339-compatible output.
	ts, parseErr := time.Parse(time.RFC3339, strings.TrimSpace(doc.GeneratedAt))
	if parseErr != nil {
		return time.Time{}, nil
	}
	return ts, nil
}

// Cache holds the dictionary in pod-local memory and refreshes it periodically. Lookup is a pure
// memory read — Redis is NOT hit per post. Same pattern and same fail-soft contract
// as lockstatus.Cache.
type Cache struct {
	loader   Loader
	interval time.Duration
	logger   *slog.Logger

	snapshot atomic.Value // *dictSnapshot

	startOnce   sync.Once
	stopOnce    sync.Once
	loopStarted atomic.Bool
	stopCh      chan struct{}
	doneCh      chan struct{}
}

// NewCache uses DefaultRefreshInterval for interval<=0. The cold-start
// snapshot is EMPTY: until the dictionary is loaded Canonical is always ("", false),
// and Match returns nil. NO FALLBACK — the dictionary is the sole source of extraction;
// not running with an empty dictionary is the caller's responsibility (see the writing side's empty-dictionary gate).
func NewCache(loader Loader, interval time.Duration, logger *slog.Logger) *Cache {
	if interval <= 0 {
		interval = DefaultRefreshInterval
	}
	c := &Cache{
		loader:   loader,
		interval: interval,
		logger:   logger,
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
	}
	c.snapshot.Store(newSnapshot(map[string]string{}))
	return c
}

// Canonical returns the canonical counterpart of a raw term. Folding is done HERE
// — the caller passes the raw text.
func (c *Cache) Canonical(term string) (string, bool) {
	snap := c.snapshot.Load().(*dictSnapshot)
	v, ok := snap.aliases[Fold(term)]
	return v, ok
}

// Size is the alias count in the last successful load.
func (c *Cache) Size() int {
	return len(c.snapshot.Load().(*dictSnapshot).aliases)
}

// Start does the first load synchronously and then starts periodic refresh.
// Idempotent.
func (c *Cache) Start(ctx context.Context) {
	c.startOnce.Do(func() {
		c.RefreshOnce(ctx)
		c.loopStarted.Store(true)
		go c.loop(ctx)
	})
}

// Stop stops the refresh goroutine and WAITS FOR IT TO EXIT (same contract as lockstatus.Cache).
// Being synchronous matters: the caller typically closes the shared Redis client
// right after Stop; if left asynchronous, an in-flight HGETALL
// would race with the closed client.
//
// Idempotent and safe even if Start was never called: while loopStarted is false
// doneCh will never close, so it is not waited on.
func (c *Cache) Stop() {
	c.stopOnce.Do(func() {
		close(c.stopCh)
	})
	if c.loopStarted.Load() {
		<-c.doneCh
	}
}

func (c *Cache) loop(ctx context.Context) {
	defer close(c.doneCh)
	t := time.NewTicker(c.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stopCh:
			return
		case <-t.C:
			c.RefreshOnce(ctx)
		}
	}
}

// RefreshOnce reloads the dictionary. ON ERROR THE OLD SNAPSHOT
// IS KEPT: emptying the dictionary would silently turn off normalization and make every
// variant a separate entity.
func (c *Cache) RefreshOnce(ctx context.Context) {
	data, err := c.loader.Load(ctx)
	if err != nil {
		c.logger.Warn("canonical dict refresh failed — keeping old snapshot",
			slog.String("error", err.Error()),
			slog.Int("current_size", c.Size()),
		)
		return
	}
	c.snapshot.Store(newSnapshot(data))
	AliasCount.Set(float64(len(data)))
	c.refreshAge(ctx)
	c.logger.Info("canonical dict loaded", slog.Int("alias_count", len(data)))
}

// refreshAge updates the age gauge if the loader can also read :meta.
// If it cannot be read the gauge is NOT TOUCHED: a previous valid age is better than a made-up 0,
// and the "never set" state is already distinguishable in Prometheus.
func (c *Cache) refreshAge(ctx context.Context) {
	ml, ok := c.loader.(MetaLoader)
	if !ok {
		return
	}
	ts, err := ml.LoadMeta(ctx)
	if err != nil {
		c.logger.Warn("could not read canonical dict meta — age gauge not updated",
			slog.String("error", err.Error()))
		return
	}
	if ts.IsZero() {
		return
	}
	AgeSeconds.Set(time.Since(ts).Seconds())
}
