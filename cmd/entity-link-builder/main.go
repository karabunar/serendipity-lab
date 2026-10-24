// Command entity-link-builder counts entity
// surfaces from the post:data.entities corpus and atomically refreshes the serendipity:entity:link HASH.
//
// This service does not change the feed directly. The `post consumer` only
// reads this HASH when ENTITY_LINK_ENABLED=true; if the builder does not run,
// the existing canonical-dictionary + NER behavior is preserved.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/redis/go-redis/v9"

	"serendipity/internal/entitylink"
	"serendipity/internal/envutil"
	"serendipity/internal/postmeta"
	"serendipity/internal/redisenv"
)

const defaultBatchSize = 1000

type scanner interface {
	Scan(ctx context.Context, cursor uint64, match string, count int64) *redis.ScanCmd
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if !envutil.Bool("ENTITY_LINK_BUILD_ENABLED", false) {
		logger.Info("ENTITY_LINK_BUILD_ENABLED=false — nothing done")
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	rdb := redisenv.New()
	defer rdb.Close()

	batchSize := envutil.Int("ENTITY_LINK_BATCH_SIZE", defaultBatchSize)
	if batchSize <= 0 {
		batchSize = defaultBatchSize
	}
	maxPosts := envutil.Int("ENTITY_LINK_MAX_POSTS", 0)
	counts, scanned, err := collectCounts(ctx, postmeta.New(rdb, postmeta.Config{}, logger), rdb, batchSize, maxPosts)
	if err != nil {
		logger.Error("entity linker corpus read failed", slog.Any("error", err), slog.Int("scanned", scanned))
		os.Exit(1)
	}

	mappings := entitylink.BuildMappings(counts, entitylink.BuilderConfig{
		MinCanonicalCount: envutil.Int("ENTITY_LINK_MIN_CANONICAL_COUNT", 2),
		MaxMappings:       envutil.Int("ENTITY_LINK_MAX_MAPPINGS", 0),
	})
	if err := entitylink.NewRedisWriter(rdb, logger).Replace(ctx, mappings); err != nil {
		logger.Error("entity linker map write failed", slog.Any("error", err))
		os.Exit(1)
	}
	logger.Info("entity linker run completed",
		slog.Int("posts_scanned", scanned),
		slog.Int("unique_surfaces", len(counts)),
		slog.Int("mappings", len(mappings)),
	)
}

// collectCounts pages through the post:data keys with SCAN and reads the entities
// fields in batches. SCAN results are unordered; for corpus statistics
// the order does not matter.
func collectCounts(ctx context.Context, pd *postmeta.Store, rdb scanner, batchSize, maxPosts int) (map[string]int, int, error) {
	ids := make([]string, 0, batchSize)
	counts := make(map[string]int)
	var cursor uint64
	scanned := 0
	flush := func() error {
		if len(ids) == 0 {
			return nil
		}
		entities, err := pd.GetEntitiesBatch(ctx, ids)
		if err != nil {
			return err
		}
		for _, names := range entities {
			for _, name := range names {
				name = strings.TrimSpace(name)
				if name != "" {
					counts[name]++
				}
			}
		}
		ids = ids[:0]
		return nil
	}

	for {
		keys, next, err := rdb.Scan(ctx, cursor, postmeta.ScanPattern(), int64(batchSize)).Result()
		if err != nil {
			return nil, scanned, fmt.Errorf("postdata scan: %w", err)
		}
		for _, key := range keys {
			if maxPosts > 0 && scanned >= maxPosts {
				break
			}
			ids = append(ids, strings.TrimPrefix(key, postmeta.KeyPrefix))
			scanned++
			if len(ids) >= batchSize {
				if err := flush(); err != nil {
					return nil, scanned, err
				}
			}
		}
		if maxPosts > 0 && scanned >= maxPosts {
			break
		}
		cursor = next
		if cursor == 0 {
			break
		}
		if err := ctx.Err(); err != nil {
			return nil, scanned, err
		}
	}
	if err := flush(); err != nil {
		return nil, scanned, err
	}
	return counts, scanned, nil
}
