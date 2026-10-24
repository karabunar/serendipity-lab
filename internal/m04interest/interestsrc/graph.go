package interestsrc

import (
	"context"
	"strconv"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"serendipity/internal/core"
	"serendipity/internal/m04interest"
)

// FavoriteReader reads affinity's favorite authors.
// store.RedisClient already satisfies this signature.
type FavoriteReader interface {
	GetFavoriteAuthors(ctx context.Context, userID string) ([]core.AuthorAffinity, error)
}

// F2Reader reads the serendipity:user:f2_authors:{uid} ZSET (score = mutual_count,
// stored as a ZSET).
//
// ⚠️ It used to be read as a SET with SMembers; the key
// had earlier been moved from SET → ZSET. Every read returned WRONGTYPE
// and since the error was silently swallowed, the graph prior NEVER saw the F2 authors
// (the flag was off, so the feed was unaffected). The tests also used the old SET format,
// so the defect was not caught.
type F2Reader interface {
	ZRevRange(ctx context.Context, key string, start, stop int64) *goredis.StringSliceCmd
}

// AuthorPostsReader reads the serendipity:author:posts:{author} ZSET.
//
// Score = post created_at (Unix seconds) — see postcache/cache.go:66.
// So `since` can be applied directly as a score range and each
// post becomes a signal with its REAL time.
//
// Reading is done with a PIPELINE: each user has up to MaxAuthors
// authors, and reading them one by one pushed the hourly cron to ~50 Redis
// round trips per user (96.8k users: ~minutes → ~20 min).
type AuthorPostsReader interface {
	Pipeline() goredis.Pipeliner
}

// GraphConfig holds the cost ceilings of the graph prior.
type GraphConfig struct {
	MaxAuthors     int // total author ceiling (the F2 set can be large)
	PostsPerAuthor int // number of latest posts to read per author
	// If Until is non-zero, only posts published BEFORE (inclusive) this moment are
	// read. The normal path leaves it empty; a time-split measurement supplies the training cutoff so that the future does not leak.
	Until time.Time
}

// AuthorContentGraph produces the signal "the authors I interact with write
// about these entities".
//
// Design §4.2.5 said "the neighbors' profiles from the previous run"; that
// does not hold because f2/favorite records are AUTHOR ids, whereas profiles are kept
// per CONSUMER user (the plan's Decision 3). The entities of an author's latest
// posts give the same signal directly and with the existing keys,
// without creating a two-pass dependency.
type AuthorContentGraph struct {
	fav   FavoriteReader
	f2    F2Reader
	posts AuthorPostsReader
	res   m04interest.EntityResolver
	cfg   GraphConfig
}

// NewAuthorContentGraph fills zero/negative ceilings with defaults.
func NewAuthorContentGraph(fav FavoriteReader, f2 F2Reader, posts AuthorPostsReader, res m04interest.EntityResolver, cfg GraphConfig) *AuthorContentGraph {
	if cfg.MaxAuthors <= 0 {
		cfg.MaxAuthors = 50
	}
	if cfg.PostsPerAuthor <= 0 {
		cfg.PostsPerAuthor = 5
	}
	return &AuthorContentGraph{fav: fav, f2: f2, posts: posts, res: res, cfg: cfg}
}

// Prior turns neighbor posts published AFTER `since` into signals.
// Weighting is the caller's job (Builder.WithGraph); here each signal
// carries Value=1.
//
// The `since` gate is MANDATORY and has the same reason as for positives: if the same
// neighbor post is re-added on every run, the graph contribution
// accumulates per run and after N runs overrides the user's own behavior. Since the ZSET score is
// post created_at, the gate is applied directly as a score range —
// which also reduces the number of posts read.
func (g *AuthorContentGraph) Prior(ctx context.Context, userID string, since time.Time) ([]m04interest.Signal, error) {
	authors := g.collectAuthors(ctx, userID)
	if len(authors) == 0 {
		return nil, nil
	}

	minScore := "-inf"
	if !since.IsZero() {
		// The score is inclusive; +1 establishes the "strictly AFTER" semantics.
		minScore = strconv.FormatInt(since.Unix()+1, 10)
	}

	// post_id → created_at. The same post cannot come from several authors, but
	// the map does both deduplication and carrying the time.
	maxScore := "+inf"
	if !g.cfg.Until.IsZero() {
		maxScore = strconv.FormatInt(g.cfg.Until.Unix(), 10)
	}
	createdAt := make(map[string]time.Time, len(authors)*g.cfg.PostsPerAuthor)
	pp := g.posts.Pipeline()
	cmds := make([]*goredis.ZSliceCmd, 0, len(authors))
	for _, a := range authors {
		cmds = append(cmds, pp.ZRevRangeByScoreWithScores(ctx, "serendipity:author:posts:"+a, &goredis.ZRangeBy{
			Min:   minScore,
			Max:   maxScore,
			Count: int64(g.cfg.PostsPerAuthor),
		}))
	}
	// Exec's error is that of the first failed command; commands are read one by one so that
	// the failure of a single author does not drop the prior.
	_, _ = pp.Exec(ctx)
	for _, cmd := range cmds {
		zs, err := cmd.Result()
		if err != nil {
			continue
		}
		for _, z := range zs {
			id, ok := z.Member.(string)
			if !ok || id == "" {
				continue
			}
			createdAt[id] = time.Unix(int64(z.Score), 0)
		}
	}
	if len(createdAt) == 0 {
		return nil, nil
	}

	postIDs := make([]string, 0, len(createdAt))
	for id := range createdAt {
		postIDs = append(postIDs, id)
	}
	entities, err := g.res.EntitiesOf(ctx, postIDs)
	if err != nil {
		return nil, err
	}

	out := make([]m04interest.Signal, 0, len(entities))
	for id, ents := range entities {
		if len(ents) == 0 {
			continue
		}
		// At = the post's REAL publish time. This way the graph signal is also
		// decayed by its age like positives, and with the `since` gate is
		// counted exactly ONCE.
		out = append(out, m04interest.Signal{Entities: ents, Value: 1, At: createdAt[id]})
	}
	return out, nil
}

// collectAuthors collects favorite + F2 authors with DEDUPLICATION and
// applies the MaxAuthors ceiling. Favorites come first: they derive from the user's
// own behavior, F2 is only proximity.
func (g *AuthorContentGraph) collectAuthors(ctx context.Context, userID string) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0, g.cfg.MaxAuthors)

	add := func(id string) bool {
		if id == "" {
			return true
		}
		if _, dup := seen[id]; dup {
			return true
		}
		seen[id] = struct{}{}
		out = append(out, id)
		return len(out) < g.cfg.MaxAuthors
	}

	if favs, err := g.fav.GetFavoriteAuthors(ctx, userID); err == nil {
		for _, f := range favs {
			if !add(f.AuthorID) {
				return out
			}
		}
	}
	// mutual_count DESC: when the ceiling truncates, the authors with the most mutual connections stay.
	// An old SET key during the rollout window returns WRONGTYPE → F2 is skipped
	// (an unpopulated key behaves the same way).
	if f2, err := g.f2.ZRevRange(ctx, "serendipity:user:f2_authors:"+userID, 0, int64(g.cfg.MaxAuthors)-1).Result(); err == nil {
		for _, a := range f2 {
			if !add(a) {
				return out
			}
		}
	}
	return out
}
