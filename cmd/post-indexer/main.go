// Command post-indexer scans the post HASHes in Redis, turns their texts into vectors via the embedding
// service and writes them to the vector database (Qdrant). The output collection is
// the source for the curated Fetcher's niche matching (postindex.VectorSource) and for similar-post search
// (postindex.Indexer.Similar).
//
// Disabled by default: it does nothing unless POST_INDEXER_ENABLED=true.
//
// Environment: EMBED_GRPC_TARGET (localhost:50061), QDRANT_URL (http://localhost:6333),
// QDRANT_API_KEY, QDRANT_COLLECTION (serendipity_posts), POST_INDEXER_BATCH (64),
// POST_INDEXER_MAX_POSTS (0 = unlimited), REDIS_ADDR/REDIS_PASSWORD.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"serendipity/internal/embedclient"
	"serendipity/internal/envutil"
	"serendipity/internal/postindex"
	"serendipity/internal/postmeta"
	"serendipity/internal/redisenv"
	"serendipity/internal/vectordb"
)

type scanner interface {
	Scan(ctx context.Context, cursor uint64, match string, count int64) *redis.ScanCmd
}

type hydrator interface {
	Hydrate(ctx context.Context, postIDs []string) (map[string]postmeta.PostData, error)
}

type indexer interface {
	Index(ctx context.Context, posts []postindex.Post) (int, error)
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if !envutil.Bool("POST_INDEXER_ENABLED", false) {
		logger.Info("POST_INDEXER_ENABLED=false — nothing done")
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	emb, err := embedclient.New(envutil.String("EMBED_GRPC_TARGET", "localhost:50061"), 60*time.Second)
	if err != nil {
		logger.Error("failed to set up embedding client", slog.Any("error", err))
		os.Exit(1)
	}
	defer emb.Close()
	db := vectordb.NewQdrant(envutil.String("QDRANT_URL", "http://localhost:6333"), envutil.String("QDRANT_API_KEY", ""), 30*time.Second)
	batch := envutil.Int("POST_INDEXER_BATCH", postindex.DefaultBatch)
	ix, err := postindex.NewIndexer(emb, db, envutil.String("QDRANT_COLLECTION", "serendipity_posts"), batch)
	if err != nil {
		logger.Error("failed to set up indexer", slog.Any("error", err))
		os.Exit(1)
	}

	rdb := redisenv.New()
	defer rdb.Close()
	pm := postmeta.New(rdb, postmeta.Config{}, logger)

	scanned, written, err := run(ctx, rdb, pm, ix, batch, envutil.Int("POST_INDEXER_MAX_POSTS", 0))
	if err != nil {
		logger.Error("post indexing aborted midway", slog.Any("error", err), slog.Int("scanned", scanned), slog.Int("written", written))
		os.Exit(1)
	}
	logger.Info("post indexing finished", slog.Int("scanned", scanned), slog.Int("written", written))
}

// run scans the post keys, hydrates them in batches and indexes them.
// It stops once maxPosts>0 posts have been scanned. Returns: number of posts scanned and written.
func run(ctx context.Context, rdb scanner, hyd hydrator, ix indexer, batch, maxPosts int) (int, int, error) {
	if batch <= 0 {
		batch = postindex.DefaultBatch
	}
	ids := make([]string, 0, batch)
	scanned, written := 0, 0
	flush := func() error {
		if len(ids) == 0 {
			return nil
		}
		data, err := hyd.Hydrate(ctx, ids)
		if err != nil {
			return fmt.Errorf("hydrate: %w", err)
		}
		posts := make([]postindex.Post, 0, len(ids))
		for _, id := range ids {
			if d, ok := data[id]; ok {
				posts = append(posts, postindex.Post{ID: id, AuthorID: d.AuthorID, Category: d.Category, Text: d.Text})
			}
		}
		n, err := ix.Index(ctx, posts)
		written += n
		ids = ids[:0]
		return err
	}

	var cursor uint64
	for {
		keys, next, err := rdb.Scan(ctx, cursor, postmeta.ScanPattern(), int64(batch)).Result()
		if err != nil {
			return scanned, written, fmt.Errorf("post scan: %w", err)
		}
		for _, key := range keys {
			if maxPosts > 0 && scanned >= maxPosts {
				break
			}
			ids = append(ids, strings.TrimPrefix(key, postmeta.KeyPrefix))
			scanned++
			if len(ids) >= batch {
				if err := flush(); err != nil {
					return scanned, written, err
				}
			}
		}
		cursor = next
		if cursor == 0 || (maxPosts > 0 && scanned >= maxPosts) {
			break
		}
		if err := ctx.Err(); err != nil {
			return scanned, written, err
		}
	}
	if err := flush(); err != nil {
		return scanned, written, err
	}
	return scanned, written, nil
}
