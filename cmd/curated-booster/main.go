// Command curated-booster is a CronJob that scans all authors and writes old-but-quality
// posts to the serendipity:curated:boosted ZSET. A K8s CronJob
// (every 6 hours). A prerequisite of the Phase 1 Curator/Booster agent — once the agent
// is written, this service's scoring logic (contentSignal) is replaced
// by an LLM call, the ZSET interface stays the same.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"serendipity/internal/curated/grpcclient"
	"serendipity/internal/envutil"
	"serendipity/internal/postcache"
	"serendipity/internal/postmeta"
	"serendipity/internal/redisenv"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	shutdownMetrics := startMetricsServer(logger)
	defer func() {
		sCtx, sCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer sCancel()
		if err := shutdownMetrics(sCtx); err != nil {
			logger.Warn("metrics server shutdown error", slog.String("error", err.Error()))
		}
	}()

	rdb := redisenv.New()
	defer rdb.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()

	cache := postcache.NewLatestPostsCache(rdb, 24*time.Hour, logger)
	pdStore := postmeta.New(rdb, postmeta.Config{}, logger)

	curator := buildCuratorClient(logger)
	if c, ok := curator.(*grpcclient.CuratorClient); ok {
		defer c.Close()
	}

	boostCfg := BoostConfig{
		MinAge:              envutil.Duration("CURATED_MIN_AGE", 48*time.Hour),
		QualityThreshold:    envutil.FloatAllowZero("CURATED_QUALITY_THRESHOLD", 0),
		TextLenNorm:         envutil.Float("CURATED_TEXT_LEN_NORM", 200),
		MediaBonus:          envutil.Float("CURATED_MEDIA_BONUS", 0.3),
		LLMQualityThreshold: envutil.FloatAllowZero("CURATED_LLM_QUALITY_THRESHOLD", 0),
	}

	runErr := RunBoost(ctx, rdb, cache, pdStore, curator, boostCfg, logger)
	if runErr != nil {
		logger.Error("curated boost run failed", slog.Any("err", runErr))
		os.Exit(1)
	}
	logger.Info("curated-booster done")
}

// buildCuratorClient builds the curator client according to the CURATOR_GRPC_ENDPOINT env.
// The return TYPE is the curatorClient INTERFACE — NEVER the concrete *grpcclient.CuratorClient
// type. Passing a concrete nil pointer directly as an interface parameter
// falls into the Go "typed nil" trap: the `curator != nil` check inside RunBoost
// ALWAYS returns true, even if the underlying pointer is nil, because the interface
// itself is non-nil (it carries type information)
// — which calls EnrichPosts with a nil receiver and panics. If the endpoint
// is empty (the default, the case where the agent service is not yet deployed) or client init
// fails, a real (untyped) nil interface is returned without ever assigning curator
// — RunBoost's nil check works correctly in this case.
// See TestBuildCuratorClient_NoEndpoint_ReturnsTrueNilInterface and
// TestBuildCuratorClient_DemonstratesTypedNilTrap (main_test.go) — the regression
// protection for this invariant.
func buildCuratorClient(logger *slog.Logger) curatorClient {
	endpoint := envutil.String("CURATOR_GRPC_ENDPOINT", "")
	if endpoint == "" {
		return nil
	}
	c, err := grpcclient.NewCuratorClient(endpoint, envutil.Duration("CURATOR_GRPC_TIMEOUT", 10*time.Second), envutil.Bool("CURATOR_GRPC_USE_TLS", false), logger)
	if err != nil {
		logger.Warn("curator client init failed — heuristic-only fallback", slog.String("error", err.Error()))
		return nil
	}
	return c
}

// startMetricsServer serves the /metrics + /healthz endpoints in the background.
// It is turned off with METRICS_ENABLED=false;
// METRICS_ADDR defaults to ":2112".
func startMetricsServer(logger *slog.Logger) func(context.Context) error {
	if v := os.Getenv("METRICS_ENABLED"); v == "false" || v == "0" {
		logger.Info("metrics server disabled (METRICS_ENABLED=false)")
		return func(context.Context) error { return nil }
	}
	addr := os.Getenv("METRICS_ADDR")
	if addr == "" {
		addr = ":2112"
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Warn("metrics server error", slog.String("error", err.Error()))
		}
	}()
	logger.Info("metrics server listening", slog.String("addr", addr))
	return srv.Shutdown
}
