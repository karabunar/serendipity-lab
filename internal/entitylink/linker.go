// Package entitylink links post entity surfaces to more common entity keys
// derived from corpus frequency.
//
// The write path lives in a separate builder service. The read path is an in-pod atomic
// snapshot; NER is not allowed to hit Redis for every post.
package entitylink

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	goredis "github.com/redis/go-redis/v9"

	"serendipity/internal/trfold"
)

const (
	// Key is the Redis HASH key of the corpus linker map. Field is the folded
	// surface, value is the canonical/common surface.
	Key = "serendipity:entity:link"
	// MetaKey is the key of the small JSON metadata of the last successful map build.
	MetaKey        = "serendipity:entity:link:meta"
	minAliasRunes  = 2
	maxAliasRunes  = 64
	minSuffixRunes = 5
)

// Loader fetches the whole map as a single snapshot.
type Loader interface {
	Load(context.Context) (map[string]string, error)
}

// LocatedMatch carries the rune range of a linker match within the text.
// Offsets are [Start, End) and are Unicode rune indices, not UTF-8 bytes; they are in the same space as the Python
// NER service's start_char/end_char contract.
type LocatedMatch struct {
	Canonical string
	Start     int
	End       int
}

// LocatedMatcher is an optional interface so that MergeHybrid can apply rule 2 suppression only to a
// dictionary match inside an NER span.
type LocatedMatcher interface {
	MatchLocated(text string) []LocatedMatch
}

// RedisLoader reads the entity linker HASH.
type RedisLoader struct{ rdb goredis.Cmdable }

func NewRedisLoader(rdb goredis.Cmdable) *RedisLoader { return &RedisLoader{rdb: rdb} }

func (l *RedisLoader) Load(ctx context.Context) (map[string]string, error) {
	values, err := l.rdb.HGetAll(ctx, Key).Result()
	if err != nil && !errors.Is(err, goredis.Nil) {
		return nil, err
	}
	if values == nil {
		return map[string]string{}, nil
	}
	return values, nil
}

type snapshot struct {
	aliases  map[string]string
	maxRunes int
}

func newSnapshot(values map[string]string) *snapshot {
	aliases := make(map[string]string, len(values))
	maxRunes := 0
	for alias, canonical := range values {
		alias = normalize(alias)
		canonical = strings.TrimSpace(canonical)
		if alias == "" || canonical == "" || alias == normalize(canonical) {
			continue
		}
		if n := len([]rune(alias)); n > maxAliasRunes {
			continue
		} else if n > maxRunes {
			maxRunes = n
		}
		aliases[alias] = canonical
	}
	return &snapshot{aliases: aliases, maxRunes: maxRunes}
}

// Cache serves the linker HASH in-pod as an atomic snapshot.
type Cache struct {
	loader      Loader
	interval    time.Duration
	logger      *slog.Logger
	snapshot    atomic.Value // *snapshot
	startOnce   sync.Once
	stopOnce    sync.Once
	loopStarted atomic.Bool
	stopCh      chan struct{}
	doneCh      chan struct{}
}

// NewCache uses hourly refresh for interval<=0.
func NewCache(loader Loader, interval time.Duration, logger *slog.Logger) *Cache {
	if interval <= 0 {
		interval = time.Hour
	}
	if logger == nil {
		logger = slog.Default()
	}
	c := &Cache{
		loader: loader, interval: interval, logger: logger,
		stopCh: make(chan struct{}), doneCh: make(chan struct{}),
	}
	c.snapshot.Store(newSnapshot(nil))
	return c
}

// Canonical returns the common surface if the surface is found
// as an exact alias in the corpus map.
func (c *Cache) Canonical(term string) (string, bool) {
	snap := c.snapshot.Load().(*snapshot)
	v, ok := snap.aliases[normalize(term)]
	return v, ok
}

// Size is the alias count in the last successful snapshot.
func (c *Cache) Size() int { return len(c.snapshot.Load().(*snapshot).aliases) }

// Match returns the linker aliases inside the text in order of pass.
func (c *Cache) Match(text string) []string {
	matches := c.MatchLocated(text)
	out := make([]string, 0, len(matches))
	for _, match := range matches {
		out = append(out, match.Canonical)
	}
	return out
}

// MatchLocated returns the linker aliases in the text with their positions.
func (c *Cache) MatchLocated(text string) []LocatedMatch {
	snap := c.snapshot.Load().(*snapshot)
	if len(snap.aliases) == 0 || text == "" {
		return nil
	}
	r := []rune(trfold.Fold(text))
	var out []LocatedMatch
	for i := 0; i < len(r); i++ {
		if !isWordStart(r, i) {
			continue
		}
		maxLen := snap.maxRunes
		if rem := len(r) - i; maxLen > rem {
			maxLen = rem
		}
		for n := maxLen; n >= minAliasRunes; n-- {
			canonical, ok := snap.aliases[string(r[i:i+n])]
			if !ok {
				continue
			}
			if i+n < len(r) && isWordRune(r[i+n]) && lastTokenRunes(r[i:i+n]) < minSuffixRunes {
				continue
			}
			out = append(out, LocatedMatch{Canonical: canonical, Start: i, End: i + n})
			i += n - 1
			break
		}
	}
	return out
}

// Start loads the first snapshot and starts the refresh loop.
func (c *Cache) Start(ctx context.Context) {
	c.startOnce.Do(func() {
		c.RefreshOnce(ctx)
		c.loopStarted.Store(true)
		go c.loop(ctx)
	})
}

// Stop waits for the refresh goroutine to exit.
func (c *Cache) Stop() {
	c.stopOnce.Do(func() { close(c.stopCh) })
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

// RefreshOnce keeps the old snapshot on error.
func (c *Cache) RefreshOnce(ctx context.Context) {
	values, err := c.loader.Load(ctx)
	if err != nil {
		c.logger.Warn("entity linker refresh failed — keeping old snapshot", slog.Any("error", err))
		return
	}
	snap := newSnapshot(values)
	c.snapshot.Store(snap)
	MappingCount.Set(float64(len(snap.aliases)))
	c.logger.Info("entity linker map loaded", slog.Int("mapping_count", len(snap.aliases)))
}

func normalize(s string) string {
	return strings.Join(strings.Fields(trfold.Fold(strings.TrimSpace(s))), " ")
}

func isWordStart(r []rune, i int) bool {
	if !isWordRune(r[i]) || i == 0 || isWordRune(r[i-1]) {
		return i == 0 && isWordRune(r[i])
	}
	if i >= 2 && isApostrophe(r[i-1]) && isWordRune(r[i-2]) {
		return false
	}
	return true
}

func isWordRune(r rune) bool   { return unicode.IsLetter(r) || unicode.IsDigit(r) }
func isApostrophe(r rune) bool { return r == '\'' || r == '’' || r == '´' }

func lastTokenRunes(r []rune) int {
	n := 0
	for i := len(r) - 1; i >= 0 && isWordRune(r[i]); i-- {
		n++
	}
	return n
}
