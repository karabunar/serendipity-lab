package interactionlog

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"

	"serendipity/internal/interactions"
)

var base = time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

func newLog(t *testing.T, maxLen int) (*Log, *goredis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	l := New(rdb, maxLen, time.Hour)
	l.now = func() time.Time { return base }
	return l, rdb
}

func ev(post, author, typ string, at time.Time) interactions.RawInteraction {
	return interactions.RawInteraction{UserID: "u1", PostID: post, AuthorID: author, InteractionType: typ, Timestamp: at}
}

func TestRecord_FillsDefaultsAndValidates(t *testing.T) {
	ctx := context.Background()
	l, _ := newLog(t, 10)
	if err := l.Record(ctx, interactions.RawInteraction{UserID: "u1", PostID: "p1", InteractionType: "reblog"}); err != nil {
		t.Fatal(err)
	}
	got, _ := l.GetRecent(ctx, "u1", 5)
	if len(got) != 1 || got[0].Value != 2.0 || !got[0].Timestamp.Equal(base) {
		t.Fatalf("defaults must be filled: %+v", got)
	}
	bad := []interactions.RawInteraction{
		{PostID: "p", InteractionType: "like"},
		{UserID: "u", InteractionType: "like"},
		{UserID: "u", PostID: "p", InteractionType: "bilinmeyen"},
		{UserID: "u", PostID: "p", InteractionType: "like", Value: -1},
	}
	for i, b := range bad {
		if err := l.Record(ctx, b); err == nil {
			t.Errorf("invalid event %d was accepted", i)
		}
	}
}

func TestRecordBatch_AllOrNothingAndNewestFirst(t *testing.T) {
	ctx := context.Background()
	l, _ := newLog(t, 10)
	err := l.RecordBatch(ctx, []interactions.RawInteraction{ev("p1", "a", "like", base), {UserID: "u1", InteractionType: "like"}})
	if err == nil {
		t.Fatal("invalid event must stop the batch write")
	}
	if got, _ := l.GetRecent(ctx, "u1", 5); len(got) != 0 {
		t.Fatalf("nothing should have been written: %+v", got)
	}
	_ = l.RecordBatch(ctx, []interactions.RawInteraction{ev("p1", "a", "like", base), ev("p2", "a", "like", base)})
	got, _ := l.GetRecent(ctx, "u1", 5)
	if len(got) != 2 || got[0].PostID != "p2" {
		t.Fatalf("newest (last added) must come first: %+v", got)
	}
	if err := l.RecordBatch(ctx, nil); err != nil {
		t.Fatal(err)
	}
}

func TestRecord_TrimsToMaxLenAndSetsTTL(t *testing.T) {
	ctx := context.Background()
	l, rdb := newLog(t, 3)
	for _, p := range []string{"p1", "p2", "p3", "p4", "p5"} {
		_ = l.Record(ctx, ev(p, "a", "like", base))
	}
	got, _ := l.GetRecent(ctx, "u1", 0)
	if len(got) != 3 || got[0].PostID != "p5" || got[2].PostID != "p3" {
		t.Fatalf("only the last 3 events must remain: %+v", got)
	}
	if ttl := rdb.TTL(ctx, KeyPrefix+"u1").Val(); ttl <= 0 || ttl > time.Hour {
		t.Fatalf("TTL not set: %v", ttl)
	}
}

func TestGetRecent_SkipsCorruptItems(t *testing.T) {
	ctx := context.Background()
	l, rdb := newLog(t, 10)
	rdb.RPush(ctx, KeyPrefix+"u1", "bozuk", `{"post_id":""}`, `{"user_id":"u1","post_id":"p1","interaction_type":"like","value":1}`)
	got, err := l.GetRecent(ctx, "u1", 10)
	if err != nil || len(got) != 1 || got[0].PostID != "p1" {
		t.Fatalf("only the valid item must be returned: %+v %v", got, err)
	}
}

func TestRecentPostIDs_DedupsAndExcludesSkips(t *testing.T) {
	ctx := context.Background()
	l, _ := newLog(t, 20)
	_ = l.RecordBatch(ctx, []interactions.RawInteraction{
		ev("p1", "a", "like", base), ev("p2", "a", "skip", base), ev("p1", "a", "reblog", base), ev("p3", "b", "like", base),
	})
	ids, err := l.RecentPostIDs(ctx, "u1", 10)
	if err != nil || len(ids) != 2 || ids[0] != "p3" || ids[1] != "p1" {
		t.Fatalf("expected excluding skip, unique, newest first: %v %v", ids, err)
	}
}

func TestFetchUserAnalytics_AggregatesSkipsPerDayAndPost(t *testing.T) {
	ctx := context.Background()
	l, _ := newLog(t, 50)
	d0, d1, old := base, base.Add(-24*time.Hour), base.AddDate(0, 0, -10)
	s := func(post string, at time.Time, dwell int64) interactions.RawInteraction {
		it := ev(post, "a", "skip", at)
		it.DwellMs = dwell
		return it
	}
	_ = l.RecordBatch(ctx, []interactions.RawInteraction{
		s("p1", d0, 100), s("p1", d0.Add(time.Minute), 300), s("p1", d1, 50), s("p2", d0, 0), s("eski", old, 1),
		ev("p9", "a", "like", d0),
	})
	recs, err := l.FetchUserAnalytics(ctx, "u1", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 {
		t.Fatalf("p1@d0, p1@d1, p2@d0 expected (excluding old and like): %+v", recs)
	}
	var p1d0 *int64
	for _, r := range recs {
		if r.ContentID == "p1" && r.Day == "2025-06-15" {
			c := r.Skip.Count
			p1d0 = &c
			if r.Skip.DwellMsSum != 400 || r.LastTsMs != d0.Add(time.Minute).UnixMilli() {
				t.Fatalf("total/last time wrong: %+v", r)
			}
		}
	}
	if p1d0 == nil || *p1d0 != 2 {
		t.Fatal("p1's two skips on the same day must merge")
	}
	if all, _ := l.FetchUserAnalytics(ctx, "u1", 0); len(all) != 4 {
		t.Fatalf("old skip must also appear within the default 30 days: %d", len(all))
	}
}

func TestGetFavoriteAuthors_NormalizedAndRanked(t *testing.T) {
	ctx := context.Background()
	l, _ := newLog(t, 50)
	_ = l.RecordBatch(ctx, []interactions.RawInteraction{
		ev("p1", "a1", "reblog", base), ev("p2", "a1", "like", base), // 3.0
		ev("p3", "a2", "like", base), // 1.0
		ev("p4", "a3", "skip", base), // not counted
		ev("p5", "", "like", base),   // no author: not counted
	})
	fav, err := l.GetFavoriteAuthors(ctx, "u1")
	if err != nil || len(fav) != 2 {
		t.Fatalf("expected two authors: %+v %v", fav, err)
	}
	if fav[0].AuthorID != "a1" || fav[0].AffinityScore != 1 || fav[0].InteractionCount != 2 {
		t.Fatalf("nearest author must be 1.0: %+v", fav[0])
	}
	if fav[1].AuthorID != "a2" || fav[1].AffinityScore < 0.33 || fav[1].AffinityScore > 0.34 {
		t.Fatalf("second author must be 1/3: %+v", fav[1])
	}
	if none, _ := l.GetFavoriteAuthors(ctx, "yok"); none != nil {
		t.Fatalf("expected nil for a user with no interactions: %v", none)
	}
}

func TestGetFavoriteAuthors_CapsAtMaxFavorites(t *testing.T) {
	ctx := context.Background()
	l, _ := newLog(t, 500)
	var batch []interactions.RawInteraction
	for i := 0; i < MaxFavorites+10; i++ {
		batch = append(batch, ev("p"+string(rune('A'+i%26))+string(rune('a'+i/26)), "a"+string(rune('A'+i%26))+string(rune('a'+i/26)), "like", base))
	}
	_ = l.RecordBatch(ctx, batch)
	fav, _ := l.GetFavoriteAuthors(ctx, "u1")
	if len(fav) != MaxFavorites {
		t.Fatalf("must be limited to %d authors: %d", MaxFavorites, len(fav))
	}
}
