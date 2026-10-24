// Command niche-profiler extracts niche interests from active users' recent interactions
// and writes them to the serendipity:user:niche:{uid} key (meant to run once a day).
// The curated Fetcher's niche matching reads this key.
//
// OFF by default: does nothing without NICHE_PROFILER_ENABLED=true.
//
// Env: REDIS_ADDR/REDIS_PASSWORD, EMBED_GRPC_TARGET (localhost:50061),
// LOCAL_LLM_BASE_URL + LOCAL_LLM_MODEL (required; OpenAI-compatible local server),
// LOCAL_LLM_API_KEY (optional), NICHE_ACTIVE_KEY (serendipity:users:active).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"serendipity/internal/activeusers"
	"serendipity/internal/curated/niche"
	"serendipity/internal/embedclient"
	"serendipity/internal/envutil"
	"serendipity/internal/interactionlog"
	"serendipity/internal/postmeta"
	"serendipity/internal/redisenv"
)

type userLister interface {
	All(ctx context.Context, pageSize int) ([]string, error)
}

type runner interface {
	Run(ctx context.Context, users []string) (niche.Summary, error)
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if !envutil.Bool("NICHE_PROFILER_ENABLED", false) {
		logger.Info("NICHE_PROFILER_ENABLED=false — nothing done")
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	rdb := redisenv.New()
	defer rdb.Close()
	emb, err := embedclient.New(envutil.String("EMBED_GRPC_TARGET", "localhost:50061"), 60*time.Second)
	if err != nil {
		logger.Error("failed to set up embedding client", slog.Any("error", err))
		os.Exit(1)
	}
	defer emb.Close()
	cat, err := niche.NewLLMCategorizer(
		envutil.String("LOCAL_LLM_BASE_URL", ""), envutil.String("LOCAL_LLM_MODEL", ""),
		envutil.String("LOCAL_LLM_API_KEY", ""), envutil.Duration("LOCAL_LLM_TIMEOUT", 30*time.Second))
	if err != nil {
		logger.Error("failed to set up LLM categorizer", slog.Any("error", err))
		os.Exit(1)
	}
	p, err := niche.New(rdb, interactionlog.New(rdb, 0, 0), postmeta.New(rdb, postmeta.Config{}, logger), cat, emb, logger)
	if err != nil {
		logger.Error("failed to set up profiler", slog.Any("error", err))
		os.Exit(1)
	}
	if err := execute(ctx, activeusers.New(rdb, envutil.String("NICHE_ACTIVE_KEY", "")), p, logger); err != nil {
		logger.Error("niche profiling failed", slog.Any("error", err))
		os.Exit(1)
	}
}

// execute lists the active users, runs the profiler and logs the summary.
// Per-user errors do not break the run; an error is returned if the list cannot be read or the context is cancelled.
func execute(ctx context.Context, users userLister, r runner, logger *slog.Logger) error {
	list, err := users.All(ctx, 500)
	if err != nil {
		return fmt.Errorf("failed to read active users: %w", err)
	}
	logger.Info("niche profiling starting", slog.Int("users", len(list)))
	s, err := r.Run(ctx, list)
	logger.Info("niche profiling finished",
		slog.Int("written", s.Counts[niche.Written]),
		slog.Int("no_interactions", s.Counts[niche.NoInteractions]),
		slog.Int("no_text", s.Counts[niche.NoText]),
		slog.Int("no_categories", s.Counts[niche.NoCategories]),
		slog.Int("failed", s.Failed))
	return err
}
