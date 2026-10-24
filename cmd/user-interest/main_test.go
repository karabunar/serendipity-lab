package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"serendipity/internal/m04interest"
)

func TestLoadConfig_Defaults(t *testing.T) {
	cfg := loadConfig()
	if cfg.StableHalfLife != 90*24*time.Hour {
		t.Fatalf("StableHalfLife = %v", cfg.StableHalfLife)
	}
	if cfg.RecentHalfLife != 7*24*time.Hour {
		t.Fatalf("RecentHalfLife = %v", cfg.RecentHalfLife)
	}
	if cfg.GraphWeight != 0.2 {
		t.Fatalf("GraphWeight = %v", cfg.GraphWeight)
	}
	if cfg.GraphEnabled {
		t.Fatal("graph-first must be OFF by default")
	}
	if cfg.Workers <= 0 || cfg.BatchSize <= 0 {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestLoadConfig_EnvOverrides(t *testing.T) {
	t.Setenv("USER_INTEREST_DECAY_STABLE", "48h")
	t.Setenv("USER_INTEREST_GRAPH_ENABLED", "true")
	t.Setenv("USER_INTEREST_GRAPH_WEIGHT", "0.35")
	cfg := loadConfig()
	if cfg.StableHalfLife != 48*time.Hour {
		t.Fatalf("StableHalfLife = %v", cfg.StableHalfLife)
	}
	if !cfg.GraphEnabled || cfg.GraphWeight != 0.35 {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestLoadConfig_GraphWeightIsCapped(t *testing.T) {
	// The graph must be WEAK. A value above 1.0 would put the neighbor
	// ahead of the user's own behavior.
	t.Setenv("USER_INTEREST_GRAPH_WEIGHT", "5")
	if got := loadConfig().GraphWeight; got > 1.0 {
		t.Fatalf("GraphWeight = %v, should have been capped at 1.0", got)
	}
}

func TestLoadConfig_SinceGraceDefaultZero(t *testing.T) {
	// 0 = exactly-once (today's behavior). Enabling the trade-off must be a DELIBERATE
	// operator decision.
	if got := loadConfig().SinceGrace; got != 0 {
		t.Fatalf("SinceGrace = %v, default should have been 0", got)
	}
	t.Setenv("USER_INTEREST_SINCE_GRACE", "2h")
	if got := loadConfig().SinceGrace; got != 2*time.Hour {
		t.Fatalf("SinceGrace = %v, want 2h", got)
	}
}

// --- runOnce ---

type fakePager struct {
	pages [][]string
	err   error
	calls int
}

func (f *fakePager) PageByIndex(_ context.Context, _, _ int) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.calls >= len(f.pages) {
		return nil, nil
	}
	p := f.pages[f.calls]
	f.calls++
	return p, nil
}

type fakeBuilder struct {
	mu     sync.Mutex
	seen   []string
	failOn string
	panics string
}

func (f *fakeBuilder) BuildUser(_ context.Context, uid string, _ time.Time) (m04interest.Profile, error) {
	f.mu.Lock()
	f.seen = append(f.seen, uid)
	f.mu.Unlock()
	if uid == f.panics {
		panic("bozuk veri")
	}
	if uid == f.failOn {
		return m04interest.Profile{}, errors.New("boom")
	}
	return m04interest.Profile{}, nil
}

func rLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestRunOnce_SingleUserErrorDoesNotStopWalk(t *testing.T) {
	pager := &fakePager{pages: [][]string{{"u1", "u2", "u3"}}}
	b := &fakeBuilder{failOn: "u2"}

	processed, err := runOnce(context.Background(), b, pager, 500, 2, rLogger())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if processed != 3 {
		t.Fatalf("processed = %d, want 3", processed)
	}
	if len(b.seen) != 3 {
		t.Fatalf("processed = %v, all should have been attempted", b.seen)
	}
}

func TestRunOnce_SingleUserPanicDoesNotDropWalk(t *testing.T) {
	pager := &fakePager{pages: [][]string{{"u1", "u2"}, {"u3"}}}
	b := &fakeBuilder{panics: "u1"}

	processed, err := runOnce(context.Background(), b, pager, 500, 2, rLogger())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if processed != 3 {
		t.Fatalf("processed = %d, want 3", processed)
	}
}

func TestRunOnce_PagerErrorRETURNS(t *testing.T) {
	// It used to be logged and returned; k8s marked a run that had covered a small
	// slice of the active set as Successful.
	sentinel := errors.New("zset read failed")
	processed, err := runOnce(context.Background(), &fakeBuilder{}, &fakePager{err: sentinel}, 500, 2, rLogger())
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
	if processed != 0 {
		t.Fatalf("processed = %d", processed)
	}
}

func TestRunOnce_ExitsEarlyOnCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	b := &fakeBuilder{}
	processed, err := runOnce(ctx, b, &fakePager{pages: [][]string{{"u1"}}}, 500, 2, rLogger())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if processed != 0 || len(b.seen) != 0 {
		t.Fatalf("work done after cancel: processed=%d seen=%v", processed, b.seen)
	}
}

func TestRunOnce_WalksAllPages(t *testing.T) {
	pager := &fakePager{pages: [][]string{{"u1", "u2"}, {"u3"}}}
	processed, err := runOnce(context.Background(), &fakeBuilder{}, pager, 2, 2, rLogger())
	if err != nil {
		t.Fatal(err)
	}
	if processed != 3 {
		t.Fatalf("processed = %d, want 3", processed)
	}
}

var _ profileBuilder = (*fakeBuilder)(nil)
var _ userPager = (*fakePager)(nil)
