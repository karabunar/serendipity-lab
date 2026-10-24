// Command user-interest builds an entity-based interest
// profile for active users (Phase 2).
//
// It uses NO LLM and NO embedder — it is purely arithmetic and therefore does not wait
// for an LLM server to come up (unlike Phase 1).
//
// This service does NOT CHANGE feed behavior: the
// serendipity:user:interest:{uid} key it writes has no consumer in this phase.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"serendipity/internal/activeusers"
	"serendipity/internal/envutil"
	"serendipity/internal/interactionlog"
	"serendipity/internal/m04interest"
	"serendipity/internal/m04interest/interestsrc"
	"serendipity/internal/postmeta"
	"serendipity/internal/redisenv"
)

// jobName is the fixed job name for the cronstatus labels.

// serviceConfig: this name was chosen because "config" clashes with the
// config package import (same rationale as the wiringConfig
// pattern in other services).
type serviceConfig struct {
	StableHalfLife time.Duration
	RecentHalfLife time.Duration
	MaxEntities    int
	QueryTopK      int
	MinQueryWeight float64
	RingLimit      int
	LookbackDays   int
	ProfileTTL     time.Duration
	BatchSize      int
	Workers        int
	GraphEnabled   bool
	GraphWeight    float64
	GraphMaxAuthor int
	GraphPostsPer  int
	SinceGrace     time.Duration
}

// loadConfig reads from env. The graph weight is CAPPED at 1.0: design
// §4.2.5 requires the graph to stay WEAK; the user's own behavior must always
// come before the neighbor's.
func loadConfig() serviceConfig {
	c := serviceConfig{
		StableHalfLife: envutil.Duration("USER_INTEREST_DECAY_STABLE", 90*24*time.Hour),
		RecentHalfLife: envutil.Duration("USER_INTEREST_DECAY_RECENT", 7*24*time.Hour),
		MaxEntities:    envutil.Int("USER_INTEREST_MAX_ENTITIES", 50),
		QueryTopK:      envutil.Int("USER_INTEREST_QUERY_TOP_K", 10),
		MinQueryWeight: envutil.FloatNonNegative("USER_INTEREST_MIN_QUERY_WEIGHT", 0),
		RingLimit:      envutil.Int("USER_INTEREST_RING_LIMIT", 200),
		LookbackDays:   envutil.Int("USER_INTEREST_LOOKBACK_DAYS", 30),
		ProfileTTL:     envutil.Duration("USER_INTEREST_TTL", m04interest.DefaultTTL),
		BatchSize:      envutil.Int("USER_INTEREST_BATCH_SIZE", 500),
		Workers:        envutil.Int("USER_INTEREST_WORKERS", 4),
		GraphEnabled:   envutil.Bool("USER_INTEREST_GRAPH_ENABLED", false),
		GraphWeight:    envutil.FloatNonNegative("USER_INTEREST_GRAPH_WEIGHT", 0.2),
		GraphMaxAuthor: envutil.Int("USER_INTEREST_GRAPH_MAX_AUTHORS", 50),
		GraphPostsPer:  envutil.Int("USER_INTEREST_GRAPH_POSTS_PER_AUTHOR", 5),
		SinceGrace:     envutil.Duration("USER_INTEREST_SINCE_GRACE", 0),
	}
	if c.SinceGrace < 0 {
		c.SinceGrace = 0
	}
	if c.GraphWeight > 1.0 {
		c.GraphWeight = 1.0
	}
	if c.Workers <= 0 {
		c.Workers = 4
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 500
	}
	return c
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if !envutil.Bool("USER_INTEREST_ENABLED", false) {
		logger.Info("USER_INTEREST_ENABLED=false — nothing done")
		return
	}

	// Prometheus /metrics: even though this is a run-once cron, Prometheus
	// can scrape during the run window; start the metrics server first (precedent:
	// other services). It is never opened when the flag is off — there is
	// no run to measure on that path.
	shutdownMetrics := startMetricsServer(logger)
	defer func() {
		sCtx, sCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer sCancel()
		if err := shutdownMetrics(sCtx); err != nil {
			logger.Warn("metrics server shutdown error", slog.String("error", err.Error()))
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	rdb := redisenv.New()
	defer rdb.Close()
	cfg := loadConfig()

	pd := postmeta.New(rdb, postmeta.Config{}, logger)
	resolver := entityResolverAdapter{pd: pd}

	// Positive signals, negatives derived from skip, and favorite authors all come from the single interaction log.
	ilog := interactionlog.New(rdb, 0, 0)
	ring := interestsrc.NewRingPositives(ilog, resolver, cfg.RingLimit)
	mirror := interestsrc.NewMirrorNegatives(ilog, resolver, cfg.LookbackDays)

	builder := m04interest.NewBuilder(
		ring, mirror,
		m04interest.NewRedisStore(rdb, cfg.ProfileTTL, logger),
		m04interest.BuildConfig{
			StableHalfLife: cfg.StableHalfLife,
			RecentHalfLife: cfg.RecentHalfLife,
			MaxEntities:    cfg.MaxEntities,
			QueryTopK:      cfg.QueryTopK,
			MinQueryWeight: cfg.MinQueryWeight,
			SinceGrace:     cfg.SinceGrace,
		}, logger)

	// WithGraph RETURNS EARLY when weight <=0, so an "enabled" log would
	// give the operator wrong information: the flag is on but nothing is wired.
	switch {
	case cfg.GraphEnabled && cfg.GraphWeight <= 0:
		logger.Warn("USER_INTEREST_GRAPH_ENABLED=true but weight <=0 — graph-first NOT wired",
			slog.Float64("weight", cfg.GraphWeight))
	case cfg.GraphEnabled:
		builder = builder.WithGraph(interestsrc.NewAuthorContentGraph(
			ilog, rdb, rdb, resolver,
			interestsrc.GraphConfig{MaxAuthors: cfg.GraphMaxAuthor, PostsPerAuthor: cfg.GraphPostsPer},
		), cfg.GraphWeight)
		logger.Info("graph-first enabled", slog.Float64("weight", cfg.GraphWeight))
	}

	start := time.Now()
	processed, runErr := runOnce(ctx, builder, activeusers.New(rdb, envutil.String("USER_INTEREST_ACTIVE_KEY", "")), cfg.BatchSize, cfg.Workers, logger)
	if runErr != nil {
		// An error that cuts the walk short is NOT a success: previously the pager
		// error was logged and the function returned, and k8s marked a run that had
		// covered 3% of the active set as Successful.
		logger.Error("user interest run aborted midway",
			slog.Int("processed", processed),
			slog.Duration("took", time.Since(start)),
			slog.Any("err", runErr))
		os.Exit(1)
	}
	logger.Info("user interest run finished",
		slog.Int("processed", processed),
		slog.Duration("took", time.Since(start)))
}

// startMetricsServer serves the /metrics + /healthz endpoints in the background
// (same env names).
// Disabled with METRICS_ENABLED=false. It resolves the address as envutil.String("METRICS_ADDR", ":2112"):
// METRICS_ADDR → PROMETHEUS_METRICS_PORT → ":2112" (reading via os.Getenv
// is forbidden by tools/repoguard).
func startMetricsServer(logger *slog.Logger) func(context.Context) error {
	if v := os.Getenv("METRICS_ENABLED"); v == "false" || v == "0" {
		logger.Info("metrics server disabled (METRICS_ENABLED=false)")
		return func(context.Context) error { return nil }
	}
	addr := envutil.String("METRICS_ADDR", ":2112")
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

// entityResolverAdapter binds postmeta.Store to m04interest.EntityResolver.
// It is a separate type because postdata is a lower layer and must not carry
// an import dependency on m04interest.
type entityResolverAdapter struct{ pd *postmeta.Store }

func (a entityResolverAdapter) EntitiesOf(ctx context.Context, postIDs []string) (map[string][]string, error) {
	return a.pd.GetEntitiesBatch(ctx, postIDs)
}

// userPager is the only method needed from the active-user pager.
// usermaterialize.ActiveSet already satisfies this signature (same contract as affinity.ActivePager):
// when the offset exceeds the set size it returns an empty slice, so the
// loop ends cleanly.
type userPager interface {
	PageByIndex(ctx context.Context, offset, count int) ([]string, error)
}

// profileBuilder is the only method runOnce needs.
// m04interest.Builder satisfies this signature; it is an interface so the walk can be
// tested without a concrete Redis setup.
type profileBuilder interface {
	BuildUser(ctx context.Context, userID string, now time.Time) (m04interest.Profile, error)
}

// runOnce processes active users page by page. One user's error does NOT
// take down the others (per-user isolation) — but a pager
// error or a cancelled context CUTS THE WALK SHORT and is RETURNED as an error:
// a truncated run is not a success.
func runOnce(ctx context.Context, b profileBuilder, pager userPager, pageSize, workers int, logger *slog.Logger) (int, error) {
	var processed, offset int
	for {
		if err := ctx.Err(); err != nil {
			return processed, err
		}
		users, err := pager.PageByIndex(ctx, offset, pageSize)
		if err != nil {
			return processed, err
		}
		if len(users) == 0 {
			return processed, nil
		}
		offset += len(users)

		sem := make(chan struct{}, workers)
		var wg sync.WaitGroup
		for _, uid := range users {
			wg.Add(1)
			go func(uid string) {
				defer wg.Done()
				// A panic in a single user (bad data, nil map) must not take down the
				// whole walk — the counterpart of per-user isolation on the
				// error path.
				defer func() {
					if r := recover(); r != nil {
						logger.Error("panic while building profile",
							slog.String("user_id", uid),
							slog.Any("panic", r))
					}
				}()
				sem <- struct{}{}
				defer func() { <-sem }()
				if _, bErr := b.BuildUser(ctx, uid, time.Now()); bErr != nil {
					logger.Warn("failed to build profile",
						slog.String("user_id", uid),
						slog.Any("err", bErr))
				}
			}(uid)
		}
		wg.Wait()
		processed += len(users)
	}
}
