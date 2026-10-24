package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"

	"serendipity/internal/core"
	"serendipity/internal/postindex"
	"serendipity/internal/postmeta"
)

type recIndexer struct {
	got [][]postindex.Post
	err error
}

func (r *recIndexer) Index(_ context.Context, p []postindex.Post) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	r.got = append(r.got, append([]postindex.Post(nil), p...))
	return len(p), nil
}

func setup(t *testing.T, n int) (*goredis.Client, *postmeta.Store) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	pm := postmeta.New(rdb, postmeta.Config{TTL: time.Hour}, nil)
	posts := make([]core.RawPost, n)
	for i := range posts {
		posts[i] = core.RawPost{PostID: string(rune('a' + i)), AuthorID: "au", Text: "metin " + string(rune('a'+i)), CreatedAt: time.Now()}
	}
	if err := pm.SaveBatch(context.Background(), posts); err != nil {
		t.Fatal(err)
	}
	return rdb, pm
}

func TestRun_ScansHydratesAndIndexesInBatches(t *testing.T) {
	rdb, pm := setup(t, 5)
	ix := &recIndexer{}
	scanned, written, err := run(context.Background(), rdb, pm, ix, 2, 0)
	if err != nil || scanned != 5 || written != 5 {
		t.Fatalf("5 posts must be scanned and written: %d %d %v", scanned, written, err)
	}
	if len(ix.got) != 3 {
		t.Fatalf("expected 2+2+1 batches: %d", len(ix.got))
	}
	seen := map[string]postindex.Post{}
	for _, b := range ix.got {
		for _, p := range b {
			seen[p.ID] = p
		}
	}
	if len(seen) != 5 || seen["a"].AuthorID != "au" || !strings.HasPrefix(seen["a"].Text, "metin") {
		t.Fatalf("fields were carried over incorrectly: %+v", seen)
	}
}

func TestRun_MaxPostsStopsEarly(t *testing.T) {
	rdb, pm := setup(t, 5)
	scanned, written, err := run(context.Background(), rdb, pm, &recIndexer{}, 2, 3)
	if err != nil || scanned != 3 || written != 3 {
		t.Fatalf("should have stopped at 3: %d %d %v", scanned, written, err)
	}
}

func TestRun_IndexErrorStopsAndReports(t *testing.T) {
	rdb, pm := setup(t, 3)
	_, _, err := run(context.Background(), rdb, pm, &recIndexer{err: errors.New("qdrant unavailable")}, 2, 0)
	if err == nil || !strings.Contains(err.Error(), "qdrant unavailable") {
		t.Fatalf("index error expected: %v", err)
	}
}

func TestRun_EmptyKeyspace(t *testing.T) {
	rdb, pm := setup(t, 0)
	scanned, written, err := run(context.Background(), rdb, pm, &recIndexer{}, 4, 0)
	if err != nil || scanned != 0 || written != 0 {
		t.Fatalf("empty keyspace must return zero: %d %d %v", scanned, written, err)
	}
}
