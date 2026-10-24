package canonicaldict

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	goredis "github.com/redis/go-redis/v9"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type fakeLoader struct {
	mu    sync.Mutex
	calls int
	data  map[string]string
	err   error
}

func (f *fakeLoader) Load(context.Context) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	out := make(map[string]string, len(f.data))
	for k, v := range f.data {
		out[k] = v
	}
	return out, nil
}

func TestCache_CanonicalSearchedFolded(t *testing.T) {
	l := &fakeLoader{data: map[string]string{"fb": "Fenerbahçe"}}
	c := NewCache(l, time.Hour, testLogger())
	c.Start(context.Background())
	defer c.Stop()

	// The caller passes the raw text; the cache does the folding.
	for _, in := range []string{"FB", "fb", "Fb"} {
		got, ok := c.Canonical(in)
		if !ok || got != "Fenerbahçe" {
			t.Fatalf("Canonical(%q) = %q,%v", in, got, ok)
		}
	}
}

func TestCache_TermNotFound(t *testing.T) {
	c := NewCache(&fakeLoader{data: map[string]string{}}, time.Hour, testLogger())
	c.Start(context.Background())
	defer c.Stop()

	if got, ok := c.Canonical("bilinmeyen"); ok || got != "" {
		t.Fatalf("Canonical = %q,%v", got, ok)
	}
}

func TestCache_ColdStartEmptyDictionary(t *testing.T) {
	// Must be safe even before Start is called (nil map must not panic).
	c := NewCache(&fakeLoader{data: map[string]string{"fb": "F"}}, time.Hour, testLogger())
	if _, ok := c.Canonical("fb"); ok {
		t.Fatal("dictionary must be empty before Start")
	}
	if c.Size() != 0 {
		t.Fatalf("Size = %d, want 0", c.Size())
	}
}

func TestCache_LoadErrorKeepsOldSnapshot(t *testing.T) {
	// Fail-soft: if the dictionary cannot be read normalization is not broken, the old version
	// keeps being served (same contract as lockstatus.Cache).
	l := &fakeLoader{data: map[string]string{"fb": "Fenerbahçe"}}
	c := NewCache(l, time.Hour, testLogger())
	c.Start(context.Background())
	defer c.Stop()

	l.mu.Lock()
	l.err = errors.New("redis down")
	l.mu.Unlock()
	c.RefreshOnce(context.Background())

	if got, ok := c.Canonical("fb"); !ok || got != "Fenerbahçe" {
		t.Fatalf("old snapshot lost after error: %q,%v", got, ok)
	}
}

func TestCache_RefreshReplacesSnapshot(t *testing.T) {
	l := &fakeLoader{data: map[string]string{"fb": "Eski"}}
	c := NewCache(l, time.Hour, testLogger())
	c.Start(context.Background())
	defer c.Stop()

	l.mu.Lock()
	l.data = map[string]string{"fb": "Yeni"}
	l.mu.Unlock()
	c.RefreshOnce(context.Background())

	if got, _ := c.Canonical("fb"); got != "Yeni" {
		t.Fatalf("Canonical = %q, want Yeni", got)
	}
}

func TestCache_StopIdempotent(t *testing.T) {
	c := NewCache(&fakeLoader{data: map[string]string{}}, time.Hour, testLogger())
	c.Start(context.Background())
	c.Stop()
	c.Stop() // second call must not panic and must NOT BLOCK
}

func TestCache_StopWithoutStartIsSafe(t *testing.T) {
	// If Start was never called doneCh never closes; if Stop waited for it
	// it would hang forever.
	c := NewCache(&fakeLoader{data: map[string]string{}}, time.Hour, testLogger())
	done := make(chan struct{})
	go func() {
		c.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop blocked without Start")
	}
}

func TestCache_TickStopsAfterStop(t *testing.T) {
	// A WEAK but valid check: after Stop the ticker must no longer trigger
	// a load. This test does not prove that Stop WAITS (in an asynchronous
	// Stop the goroutine also exits within microseconds) — the lock of that guarantee
	// is TestCache_StopWaitsForInFlightLoad below.
	l := &fakeLoader{data: map[string]string{"fb": "Fenerbahçe"}}
	c := NewCache(l, 10*time.Millisecond, testLogger())
	c.Start(context.Background())
	c.Stop()

	l.mu.Lock()
	callsAtStop := l.calls
	l.mu.Unlock()

	time.Sleep(60 * time.Millisecond) // a few tick intervals
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.calls != callsAtStop {
		t.Fatalf("loading continued after Stop: %d → %d", callsAtStop, l.calls)
	}
}

// gatedLoader hangs on the SECOND Load call. The first call is Start's
// synchronous RefreshOnce and must pass; what must hang is the background
// loop's call.
type gatedLoader struct {
	mu      sync.Mutex
	calls   int
	entered chan struct{} // the 2nd call was entered
	release chan struct{} // the 2nd call was released
}

func (g *gatedLoader) Load(context.Context) (map[string]string, error) {
	g.mu.Lock()
	g.calls++
	n := g.calls
	g.mu.Unlock()
	if n == 2 {
		close(g.entered)
		<-g.release
	}
	return map[string]string{"fb": "Fenerbahçe"}, nil
}

func TestCache_StopWaitsForInFlightLoad(t *testing.T) {
	// The REAL lock of F1. An asynchronous Stop() returns immediately here and the first
	// select takes the `stopped` branch and fails the test. A synchronous Stop, on the other hand, MUST block until Load
	// is released — the caller closes the shared Redis client
	// right after Stop, an in-flight HGETALL cannot race with that client.
	// HGETALL o istemciyle yarisamaz.
	g := &gatedLoader{entered: make(chan struct{}), release: make(chan struct{})}
	c := NewCache(g, 10*time.Millisecond, testLogger())
	c.Start(context.Background()) // the 1st call is synchronous, passes

	<-g.entered // the loop is hung on the 2nd call

	stopped := make(chan struct{})
	go func() {
		c.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
		t.Fatal("Stop returned before the in-flight load finished")
	case <-time.After(100 * time.Millisecond):
	}

	close(g.release)

	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return even after the load finished")
	}
}

func TestRedisLoader_ReadsHash(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	mr.HSet(KeyAlias, "fb", "Fenerbahçe")
	mr.HSet(KeyAlias, "cimbom", "Galatasaray")

	got, err := NewRedisLoader(rdb).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["fb"] != "Fenerbahçe" {
		t.Fatalf("Load = %v", got)
	}
}

func TestRedisLoader_MissingKeyReturnsEmpty(t *testing.T) {
	// The dictionary may not have been produced yet — this is not an error, it means "no
	// normalization layer" and the caller falls back to the name the LLM gave.
	mr := miniredis.RunT(t)
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	got, err := NewRedisLoader(rdb).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("Load = %v, want empty", got)
	}
}

// ── I8: visibility metrics ────────────────────────────────

// metaLoader is a fake source that also satisfies MetaLoader.
type metaLoader struct {
	fakeLoader
	ts      time.Time
	metaErr error
}

func (m *metaLoader) LoadMeta(context.Context) (time.Time, error) {
	return m.ts, m.metaErr
}

func TestRefreshOnce_AliasCountGaugeIsSet(t *testing.T) {
	AliasCount.Set(-1)
	l := &fakeLoader{data: map[string]string{"fb": "Fenerbahçe", "gs": "Galatasaray"}}
	c := NewCache(l, time.Hour, testLogger())
	c.RefreshOnce(context.Background())
	if got := promtestutil.ToFloat64(AliasCount); got != 2 {
		t.Fatalf("AliasCount = %v, want 2", got)
	}
}

func TestRefreshOnce_AgeGaugeNotSetIfMetaMissing(t *testing.T) {
	// Setting a false 0 is the lie "the dictionary is fresh" and hides exactly the staleness
	// state we want to monitor.
	AgeSeconds.Set(1234)
	l := &metaLoader{fakeLoader: fakeLoader{data: map[string]string{"fb": "F"}}}
	NewCache(l, time.Hour, testLogger()).RefreshOnce(context.Background())
	if got := promtestutil.ToFloat64(AgeSeconds); got != 1234 {
		t.Fatalf("AgeSeconds = %v — gauge should not be touched when meta is missing", got)
	}
}

func TestRefreshOnce_AgeComputedIfMetaPresent(t *testing.T) {
	AgeSeconds.Set(0)
	l := &metaLoader{
		fakeLoader: fakeLoader{data: map[string]string{"fb": "F"}},
		ts:         time.Now().Add(-2 * time.Hour),
	}
	NewCache(l, time.Hour, testLogger()).RefreshOnce(context.Background())
	got := promtestutil.ToFloat64(AgeSeconds)
	if got < 7000 || got > 7400 {
		t.Fatalf("AgeSeconds = %v, want ~7200", got)
	}
}

func TestRedisLoader_LoadMeta(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	defer func() { _ = rdb.Close() }()
	l := NewRedisLoader(rdb)
	ctx := context.Background()

	// No key → (zero, nil): not an error, "age unknown".
	if ts, err := l.LoadMeta(ctx); err != nil || !ts.IsZero() {
		t.Fatalf("when meta missing ts=%v err=%v", ts, err)
	}

	// Python isoformat(timespec="seconds") output.
	mr.Set(KeyMeta, `{"generated_at":"2026-08-11T09:30:00+00:00","alias_count":5}`)
	ts, err := l.LoadMeta(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 8, 11, 9, 30, 0, 0, time.UTC); !ts.Equal(want) {
		t.Fatalf("ts = %v, want %v", ts, want)
	}

	// Corrupt JSON / unparseable time → silently "unknown".
	mr.Set(KeyMeta, `bu json degil`)
	if ts, err := l.LoadMeta(ctx); err != nil || !ts.IsZero() {
		t.Fatalf("corrupt meta: ts=%v err=%v", ts, err)
	}
	mr.Set(KeyMeta, `{"generated_at":"dun"}`)
	if ts, err := l.LoadMeta(ctx); err != nil || !ts.IsZero() {
		t.Fatalf("unparseable time: ts=%v err=%v", ts, err)
	}
}
