package main

import (
	"context"
	"log/slog"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"serendipity/internal/core"
	"serendipity/internal/postmeta"
)

func TestCollectCounts_ReadsEntityCorpusAndHonorsLimit(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	pd := postmeta.New(rdb, postmeta.Config{}, slog.Default())
	ctx := context.Background()

	if err := pd.SaveBatch(ctx, []core.RawPost{
		{PostID: "p1", Text: "bir"},
		{PostID: "p2", Text: "iki"},
		{PostID: "p3", Text: "uc"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := pd.SetEntitiesBatch(ctx, map[string]string{
		"p1": `{"entities":["Istanbul","Türkiye"],"topics":[]}`,
		"p2": `{"entities":["Istanbul"],"topics":[]}`,
		"p3": `{"entities":["Ankara"],"topics":[]}`,
	}); err != nil {
		t.Fatal(err)
	}

	counts, scanned, err := collectCounts(ctx, pd, rdb, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if scanned != 2 {
		t.Fatalf("scanned = %d, want 2", scanned)
	}
	total := counts["Istanbul"] + counts["Türkiye"] + counts["Ankara"]
	if total < 2 || total > 3 {
		t.Fatalf("counts = %#v, total=%d", counts, total)
	}
}
