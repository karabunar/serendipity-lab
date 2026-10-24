package entityindex

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
)

func testRedis(t *testing.T) (*goredis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb, mr
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func TestKey_FoldsAccents(t *testing.T) {
	if got := Key("Fenerbahçe"); got != "serendipity:entity:posts:fenerbahce" {
		t.Fatalf("Key = %q", got)
	}
	// Write and read must reach the same key: an upper/lower case difference
	// must not split the contract.
	if Key("FENERBAHÇE") != Key("fenerbahçe") {
		t.Fatal("Key is case-sensitive")
	}
}

func TestWriter_WriteReadRoundTrip(t *testing.T) {
	rdb, _ := testRedis(t)
	w := NewWriter(rdb, DefaultWindow, 3, testLogger())
	r := NewReader(rdb, DefaultWindow)
	ctx := context.Background()
	now := time.Now()

	err := w.Add(ctx, []Post{
		{PostID: "p1", Entities: []string{"Fenerbahçe"}, CreatedAt: now.Add(-time.Hour)},
		{PostID: "p2", Entities: []string{"Fenerbahçe"}, CreatedAt: now.Add(-2 * time.Hour)},
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	got, err := r.RecentBatch(ctx, map[string]int{"Fenerbahçe": 10})
	if err != nil {
		t.Fatalf("RecentBatch: %v", err)
	}
	ids := got["Fenerbahçe"]
	// created_at DESCENDING order: newest first.
	if len(ids) != 2 || ids[0] != "p1" || ids[1] != "p2" {
		t.Fatalf("ids = %v, want [p1 p2]", ids)
	}
}

func TestWriter_DensePostNotIndexed(t *testing.T) {
	rdb, _ := testRedis(t)
	w := NewWriter(rdb, DefaultWindow, 3, testLogger())
	r := NewReader(rdb, DefaultWindow)
	ctx := context.Background()

	// 4 entities > gate(3): the post is NOT written at all — spam layer 1.
	if err := w.Add(ctx, []Post{{
		PostID:    "spam",
		Entities:  []string{"Fenerbahçe", "Galatasaray", "Beşiktaş", "Trabzonspor"},
		CreatedAt: time.Now(),
	}}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	got, _ := r.RecentBatch(ctx, map[string]int{"Fenerbahçe": 10})
	if len(got["Fenerbahçe"]) != 0 {
		t.Fatalf("dense post was indexed: %v", got)
	}
}

func TestWriter_PostAtGateBoundaryIsWritten(t *testing.T) {
	rdb, _ := testRedis(t)
	w := NewWriter(rdb, DefaultWindow, 3, testLogger())
	r := NewReader(rdb, DefaultWindow)
	ctx := context.Background()

	// Exactly 3 entities: the gate was NOT exceeded, must be written.
	if err := w.Add(ctx, []Post{{
		PostID:    "mac-raporu",
		Entities:  []string{"Fenerbahçe", "Galatasaray", "Beşiktaş"},
		CreatedAt: time.Now(),
	}}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	got, _ := r.RecentBatch(ctx, map[string]int{"Beşiktaş": 10})
	if len(got["Beşiktaş"]) != 1 {
		t.Fatalf("post at the limit was not written: %v", got)
	}
}

func TestWriter_MemberOutsideWindowPruned(t *testing.T) {
	rdb, _ := testRedis(t)
	window := time.Hour
	w := NewWriter(rdb, window, 3, testLogger())
	ctx := context.Background()
	now := time.Now()

	// Manually add an old member (Add would already reject it as "too_old").
	if err := rdb.ZAdd(ctx, Key("Fenerbahçe"), goredis.Z{
		Score: float64(now.Add(-10 * time.Hour).Unix()), Member: "bayat",
	}).Err(); err != nil {
		t.Fatalf("setup ZAdd: %v", err)
	}

	// A new write must prune the same key.
	if err := w.Add(ctx, []Post{{
		PostID: "taze", Entities: []string{"Fenerbahçe"}, CreatedAt: now,
	}}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	n, err := rdb.ZCard(ctx, Key("Fenerbahçe")).Result()
	if err != nil {
		t.Fatalf("ZCard: %v", err)
	}
	if n != 1 {
		t.Fatalf("pruning did not work, ZCard = %d", n)
	}
}

func TestWriter_TTLIsSet(t *testing.T) {
	rdb, mr := testRedis(t)
	window := time.Hour
	w := NewWriter(rdb, window, 3, testLogger())
	ctx := context.Background()

	if err := w.Add(ctx, []Post{{
		PostID: "p1", Entities: []string{"Fenerbahçe"}, CreatedAt: time.Now(),
	}}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// TTL = 2 x window → a fully dead entity key drops on its own.
	ttl := mr.TTL(Key("Fenerbahçe"))
	if ttl != 2*window {
		t.Fatalf("TTL = %v, want %v", ttl, 2*window)
	}
}

func TestWriter_Idempotent(t *testing.T) {
	rdb, _ := testRedis(t)
	w := NewWriter(rdb, DefaultWindow, 3, testLogger())
	ctx := context.Background()
	p := []Post{{PostID: "p1", Entities: []string{"Fenerbahçe"}, CreatedAt: time.Now()}}

	for range 3 {
		if err := w.Add(ctx, p); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	n, _ := rdb.ZCard(ctx, Key("Fenerbahçe")).Result()
	if n != 1 {
		t.Fatalf("repeated write duplicated members: ZCard = %d", n)
	}
}

func TestWriter_ReplaceRemovesPreviousEntityMembership(t *testing.T) {
	rdb, _ := testRedis(t)
	w := NewWriter(rdb, DefaultWindow, 3, testLogger())
	r := NewReader(rdb, DefaultWindow)
	ctx := context.Background()
	now := time.Now()
	if err := w.Add(ctx, []Post{{PostID: "p1", Entities: []string{"Eski"}, CreatedAt: now}}); err != nil {
		t.Fatal(err)
	}
	if err := w.Replace(ctx, []Post{{
		PostID: "p1", PreviousEntities: []string{"Eski"}, Entities: []string{"Yeni"}, CreatedAt: now,
	}}); err != nil {
		t.Fatal(err)
	}
	got, err := r.RecentBatch(ctx, map[string]int{"Eski": 10, "Yeni": 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(got["Eski"]) != 0 || len(got["Yeni"]) != 1 {
		t.Fatalf("replace memberships = %#v", got)
	}
}

func TestReader_OutsideWindowNotRead(t *testing.T) {
	rdb, _ := testRedis(t)
	window := time.Hour
	r := NewReader(rdb, window)
	ctx := context.Background()

	if err := rdb.ZAdd(ctx, Key("Fenerbahçe"), goredis.Z{
		Score: float64(time.Now().Add(-10 * time.Hour).Unix()), Member: "bayat",
	}).Err(); err != nil {
		t.Fatalf("setup ZAdd: %v", err)
	}

	got, err := r.RecentBatch(ctx, map[string]int{"Fenerbahçe": 10})
	if err != nil {
		t.Fatalf("RecentBatch: %v", err)
	}
	if len(got["Fenerbahçe"]) != 0 {
		t.Fatalf("out-of-window member was read: %v", got)
	}
}

func TestReader_EmptyAndZeroLimitSkipped(t *testing.T) {
	rdb, _ := testRedis(t)
	r := NewReader(rdb, DefaultWindow)
	ctx := context.Background()

	got, err := r.RecentBatch(ctx, map[string]int{"YokEntity": 5, "SifirLimit": 0})
	if err != nil {
		t.Fatalf("RecentBatch: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty result: %v", got)
	}
}

// --- OverflowTruncate: truncation without lowering the gate ---------------------------
//
// Why these tests: on a real post sample the DROP policy left about 10% of posts
// out of the index and held reachability low;
// truncation raised it on the same corpus (with an upper cap of 6). The three tests below
// pin the three conditions of that policy.

func TestWriter_TruncateModeTrimsPostAboveGate(t *testing.T) {
	rdb, _ := testRedis(t)
	w := NewWriter(rdb, DefaultWindow, 3, testLogger(),
		WithOverflow(OverflowTruncate, 6))
	r := NewReader(rdb, DefaultWindow)
	ctx := context.Background()

	// 5 entities: maxEntities(3) exceeded but hardDrop(6) not → first 3 are written.
	if err := w.Add(ctx, []Post{{
		PostID:    "genis-post",
		Entities:  []string{"Fenerbahçe", "Galatasaray", "Beşiktaş", "Trabzonspor", "Samsunspor"},
		CreatedAt: time.Now(),
	}}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	got, _ := r.RecentBatch(ctx, map[string]int{
		"Fenerbahçe": 10, "Beşiktaş": 10, "Trabzonspor": 10, "Samsunspor": 10,
	})
	if len(got["Fenerbahçe"]) != 1 || len(got["Beşiktaş"]) != 1 {
		t.Fatalf("first 3 entities not written: %v", got)
	}
	// Truncated ones must NOT enter the index — otherwise truncation limits nothing.
	if len(got["Trabzonspor"]) != 0 || len(got["Samsunspor"]) != 0 {
		t.Fatalf("truncated entity was written anyway: %v", got)
	}
}

func TestWriter_TruncateModeDropsPostAboveHardCeiling(t *testing.T) {
	rdb, _ := testRedis(t)
	w := NewWriter(rdb, DefaultWindow, 3, testLogger(),
		WithOverflow(OverflowTruncate, 6))
	r := NewReader(rdb, DefaultWindow)
	ctx := context.Background()

	// 7 entities: hardDrop(6) exceeded → spam layer 1 is in effect, the post is DROPPED.
	if err := w.Add(ctx, []Post{{
		PostID: "doldurulmus-post",
		Entities: []string{"Fenerbahçe", "Galatasaray", "Beşiktaş", "Trabzonspor",
			"Samsunspor", "Rizespor", "Konyaspor"},
		CreatedAt: time.Now(),
	}}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	got, _ := r.RecentBatch(ctx, map[string]int{"Fenerbahçe": 10})
	if len(got["Fenerbahçe"]) != 0 {
		t.Fatalf("post above the hard cap was indexed: %v", got)
	}
}

func TestWriter_DefaultBehaviorIsDrop(t *testing.T) {
	rdb, _ := testRedis(t)
	// Call without options: historical behavior must be kept (rollback guarantee).
	w := NewWriter(rdb, DefaultWindow, 3, testLogger())
	r := NewReader(rdb, DefaultWindow)
	ctx := context.Background()

	if err := w.Add(ctx, []Post{{
		PostID:    "genis-post",
		Entities:  []string{"Fenerbahçe", "Galatasaray", "Beşiktaş", "Trabzonspor"},
		CreatedAt: time.Now(),
	}}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	got, _ := r.RecentBatch(ctx, map[string]int{"Fenerbahçe": 10})
	if len(got["Fenerbahçe"]) != 0 {
		t.Fatalf("default mode truncated — rollback broken: %v", got)
	}
}

func TestWithOverflow_HardCeilingCannotFallBelowMaxEntities(t *testing.T) {
	rdb, _ := testRedis(t)
	// hardDrop(2) < maxEntities(3): every overflowing post would be dropped, truncation would be
	// meaningless. The constructor must raise it to maxEntities.
	w := NewWriter(rdb, DefaultWindow, 3, testLogger(),
		WithOverflow(OverflowTruncate, 2))
	if w.hardDrop != 3 {
		t.Fatalf("hardDrop was not raised to maxEntities: %d", w.hardDrop)
	}
}

// ─── IDF: df and N ─────────────────────────────────────────────────────────
//
// The entity slot scales query weight by inverse document frequency (so that "Türkiye",
// which appears in every post, does not rise to the top of everyone's profile and fill the slot).
// df = the entity's number of posts in the window, N = TOTAL number of indexed posts in the window. For N the
// writer additionally adds every indexed post to the IndexedKey ZSET.
// IndexedKey ZSET'ine ekler.

func TestWriter_IndexedPostsWrittenToTotalSet(t *testing.T) {
	rdb, _ := testRedis(t)
	w := NewWriter(rdb, DefaultWindow, 3, testLogger())
	ctx := context.Background()
	now := time.Now()

	if err := w.Add(ctx, []Post{
		{PostID: "p1", Entities: []string{"Türkiye", "Gazze"}, CreatedAt: now.Add(-time.Hour)},
		{PostID: "p2", Entities: []string{"Türkiye"}, CreatedAt: now.Add(-time.Hour)},
		// A dropped post (density gate) does NOT enter N.
		{PostID: "spam", Entities: []string{"A", "B", "C", "D"}, CreatedAt: now.Add(-time.Hour)},
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if n, _ := rdb.ZCard(ctx, IndexedKey).Result(); n != 2 {
		t.Fatalf("IndexedKey member count = %d, want 2", n)
	}
	if ttl, _ := rdb.TTL(ctx, IndexedKey).Result(); ttl <= 0 {
		t.Fatalf("IndexedKey has no TTL (%v) — cannot prune itself", ttl)
	}
}

func TestReader_DocFreqCountsWithinWindow(t *testing.T) {
	rdb, _ := testRedis(t)
	window := time.Hour
	r := NewReader(rdb, window)
	ctx := context.Background()
	now := time.Now()
	add := func(key, id string, age time.Duration) {
		if err := rdb.ZAdd(ctx, key, goredis.Z{Score: float64(now.Add(-age).Unix()), Member: id}).Err(); err != nil {
			t.Fatal(err)
		}
	}
	add(Key("Türkiye"), "p1", time.Minute)
	add(Key("Türkiye"), "p2", time.Minute)
	add(Key("Türkiye"), "bayat", 10*time.Hour) // outside the window → not counted
	add(Key("Gazze"), "p1", time.Minute)
	for _, id := range []string{"p1", "p2", "p3"} {
		add(IndexedKey, id, time.Minute)
	}
	add(IndexedKey, "bayat", 10*time.Hour)

	df, total, err := r.DocFreq(ctx, []string{"Türkiye", "Gazze", "Yok"})
	if err != nil {
		t.Fatalf("DocFreq: %v", err)
	}
	if total != 3 {
		t.Errorf("N = %d, want 3 (out-of-window must not count)", total)
	}
	if df["Türkiye"] != 2 || df["Gazze"] != 1 || df["Yok"] != 0 {
		t.Errorf("df = %v, want Türkiye:2 Gazze:1 Yok:0", df)
	}
}
