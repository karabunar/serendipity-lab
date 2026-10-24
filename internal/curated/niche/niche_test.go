package niche

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"

	"serendipity/internal/core"
	"serendipity/internal/curated/content"
	"serendipity/internal/curated/model"
	"serendipity/internal/embedclient"
	"serendipity/internal/interactionlog"
	"serendipity/internal/interactions"
	"serendipity/internal/postmeta"
)

type fakeInter map[string][]string

func (f fakeInter) RecentPostIDs(_ context.Context, u string, _ int) ([]string, error) {
	return f[u], nil
}

type fakeHyd map[string]string

func (f fakeHyd) Hydrate(_ context.Context, ids []string) (map[string]model.PostData, error) {
	out := map[string]model.PostData{}
	for _, id := range ids {
		if t, ok := f[id]; ok {
			out[id] = model.PostData{Text: t}
		}
	}
	return out, nil
}

type fakeCat struct {
	cats  []string
	err   error
	calls int
	got   []string
}

func (f *fakeCat) Categorize(_ context.Context, texts []string, _ int) ([]string, error) {
	f.calls++
	f.got = texts
	return f.cats, f.err
}

type fakeEmb struct {
	err   error
	kinds []embedclient.Kind
	short bool
}

func (f *fakeEmb) Embed(_ context.Context, texts []string, k embedclient.Kind) ([][]float32, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.kinds = append(f.kinds, k)
	n := len(texts)
	if f.short {
		n--
	}
	out := make([][]float32, n)
	for i := range out {
		out[i] = []float32{float32(i + 1), 0.5}
	}
	return out, nil
}

func newRedis(t *testing.T) *goredis.Client {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

func build(t *testing.T, rdb *goredis.Client, inter Interactions, hyd Hydrator, cat Categorizer, emb embedclient.Embedder) *Profiler {
	t.Helper()
	p, err := New(rdb, inter, hyd, cat, emb, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestProfile_WritesEntriesWithTTLAsQueryEmbeddings(t *testing.T) {
	rdb := newRedis(t)
	cat, emb := &fakeCat{cats: []string{"Fenerbahçe", "yazılım", "kültür sanat"}}, &fakeEmb{}
	p := build(t, rdb, fakeInter{"u1": {"p1", "p2"}}, fakeHyd{"p1": "transfer haberi", "p2": "go dili"}, cat, emb)

	out, err := p.Profile(context.Background(), "u1")
	if err != nil || out != Written {
		t.Fatalf("should have been written: %v %v", out, err)
	}
	var got []Entry
	raw, _ := rdb.Get(context.Background(), KeyPrefix+"u1").Result()
	if err := json.Unmarshal([]byte(raw), &got); err != nil || len(got) != 3 {
		t.Fatalf("expected 3 niches: %v %v", got, err)
	}
	if got[0].Category != "Fenerbahçe" || len(got[0].Embedding) != 2 || got[1].Embedding[0] != 2 {
		t.Fatalf("wrong entries: %+v", got)
	}
	if ttl := rdb.TTL(context.Background(), KeyPrefix+"u1").Val(); ttl <= 0 || ttl > TTL {
		t.Fatalf("TTL must not exceed 48 hours: %v", ttl)
	}
	if emb.kinds[0] != embedclient.Query {
		t.Fatal("category texts must be embedded as queries")
	}
	if len(cat.got) != 2 {
		t.Fatalf("two texts must go to the LLM: %v", cat.got)
	}
}

func TestProfile_NoSignalSkipsCallsAndWrites(t *testing.T) {
	rdb := newRedis(t)
	cat := &fakeCat{cats: []string{"x"}}
	p := build(t, rdb, fakeInter{"stale": {"gone"}, "blank": {"b"}}, fakeHyd{"b": "   "}, cat, &fakeEmb{})

	for u, want := range map[string]Outcome{"yok": NoInteractions, "stale": NoText, "blank": NoText} {
		out, err := p.Profile(context.Background(), u)
		if err != nil || out != want {
			t.Fatalf("%s: %v %v", u, out, err)
		}
	}
	if cat.calls != 0 {
		t.Fatalf("LLM must not be called without signals: %d", cat.calls)
	}
	if rdb.Exists(context.Background(), KeyPrefix+"yok", KeyPrefix+"stale").Val() != 0 {
		t.Fatal("key must not be written without signals")
	}
}

func TestProfile_ErrorsKeepExistingProfile(t *testing.T) {
	ctx := context.Background()
	rdb := newRedis(t)
	rdb.Set(ctx, KeyPrefix+"u1", "ESKI", 0)
	hyd, inter := fakeHyd{"p1": "metin"}, fakeInter{"u1": {"p1"}}

	_, err := build(t, rdb, inter, hyd, &fakeCat{err: errors.New("llm unavailable")}, &fakeEmb{}).Profile(ctx, "u1")
	if err == nil || !strings.Contains(err.Error(), "llm unavailable") {
		t.Fatalf("LLM error expected: %v", err)
	}
	_, err = build(t, rdb, inter, hyd, &fakeCat{cats: []string{"a"}}, &fakeEmb{err: errors.New("embed unavailable")}).Profile(ctx, "u1")
	if err == nil {
		t.Fatal("embed error expected")
	}
	_, err = build(t, rdb, inter, hyd, &fakeCat{cats: []string{"a", "b"}}, &fakeEmb{short: true}).Profile(ctx, "u1")
	if err == nil || !strings.Contains(err.Error(), "vectors for") {
		t.Fatalf("vector count mismatch must be an error: %v", err)
	}
	if rdb.Get(ctx, KeyPrefix+"u1").Val() != "ESKI" {
		t.Fatal("existing profile must survive an error")
	}
}

func TestProfile_EmptyOrDuplicateCategories(t *testing.T) {
	rdb := newRedis(t)
	inter, hyd := fakeInter{"u1": {"p1"}}, fakeHyd{"p1": "metin"}

	out, err := build(t, rdb, inter, hyd, &fakeCat{cats: []string{" ", ""}}, &fakeEmb{}).Profile(context.Background(), "u1")
	if err != nil || out != NoCategories {
		t.Fatalf("empty categories must not be written: %v %v", out, err)
	}
	out, _ = build(t, rdb, inter, hyd, &fakeCat{cats: []string{"Kedi", "kedi", "KEDİ", "araba", "x", "y"}}, &fakeEmb{}).Profile(context.Background(), "u1")
	var got []Entry
	_ = json.Unmarshal([]byte(rdb.Get(context.Background(), KeyPrefix+"u1").Val()), &got)
	if out != Written || len(got) != 3 || got[0].Category != "Kedi" || got[1].Category != "araba" {
		t.Fatalf("duplicates must be removed and truncated to 3: %v %+v", out, got)
	}
}

func TestProfile_LongTextsAreTruncated(t *testing.T) {
	rdb := newRedis(t)
	cat := &fakeCat{cats: []string{"a"}}
	long := strings.Repeat("ş", MaxTextRunes*3)
	_, err := build(t, rdb, fakeInter{"u1": {"p1"}}, fakeHyd{"p1": long}, cat, &fakeEmb{}).Profile(context.Background(), "u1")
	if err != nil || utf8.RuneCountInString(cat.got[0]) != MaxTextRunes || !utf8.ValidString(cat.got[0]) {
		t.Fatalf("text must be truncated at the rune limit: %d %v", utf8.RuneCountInString(cat.got[0]), err)
	}
}

func TestRun_ContinuesPastFailuresAndCounts(t *testing.T) {
	rdb := newRedis(t)
	cat := &fakeCat{cats: []string{"a"}}
	p := build(t, rdb, fakeInter{"ok": {"p1"}, "bad": {"p1"}}, fakeHyd{"p1": "m"}, cat, &fakeEmb{})
	// embed error for "bad": instead of breaking the embedder after the first user, use a user with an empty ring
	s, err := p.Run(context.Background(), []string{"ok", "bos", "ok"})
	if err != nil || s.Counts[Written] != 2 || s.Counts[NoInteractions] != 1 || s.Failed != 0 {
		t.Fatalf("wrong summary: %+v %v", s, err)
	}
	p2 := build(t, rdb, fakeInter{"x": {"p1"}}, fakeHyd{"p1": "m"}, &fakeCat{err: errors.New("x")}, &fakeEmb{})
	s, err = p2.Run(context.Background(), []string{"x", "x"})
	if err != nil || s.Failed != 2 {
		t.Fatalf("errors must be counted and the run must continue: %+v %v", s, err)
	}
}

func TestRun_CanceledContextStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := build(t, newRedis(t), fakeInter{}, fakeHyd{}, &fakeCat{}, &fakeEmb{})
	if _, err := p.Run(ctx, []string{"u1"}); err == nil {
		t.Fatal("a cancelled context must stop the run")
	}
}

func TestNew_RequiresAllDependencies(t *testing.T) {
	if _, err := New(nil, nil, nil, nil, nil, nil); err == nil {
		t.Fatal("missing dependency was accepted")
	}
}

// TestProfilerOutputIsReadableByCuratedFetcher locks in that the writer and reader's key and format
// contract holds: the niche the profiler writes works as a threshold filter in the Fetcher.
func TestProfilerOutputIsReadableByCuratedFetcher(t *testing.T) {
	ctx := context.Background()
	rdb := newRedis(t)
	pm := postmeta.New(rdb, postmeta.Config{TTL: time.Hour}, nil)
	if err := pm.SaveBatch(ctx, []core.RawPost{
		{PostID: "yakin", AuthorID: "a1", Text: "yakın", CreatedAt: time.Now()},
		{PostID: "uzak", AuthorID: "a2", Text: "uzak", CreatedAt: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}
	rdb.ZAdd(ctx, "serendipity:curated:boosted", goredis.Z{Score: 2, Member: "yakin"}, goredis.Z{Score: 1, Member: "uzak"})
	ilog := interactionlog.New(rdb, 0, 0)
	if err := ilog.Record(ctx, interactions.RawInteraction{UserID: "u1", PostID: "yakin", InteractionType: interactions.InteractionLike}); err != nil {
		t.Fatal(err)
	}

	// Niche vector is [1, 0.5] (fakeEmb's first output); "yakin" is in the same direction, "uzak" is orthogonal.
	_, err := build(t, rdb, ilog, pm, &fakeCat{cats: []string{"kategori"}}, &fakeEmb{}).Profile(ctx, "u1")
	if err != nil {
		t.Fatal(err)
	}
	f := content.New(rdb, pm, vecSource{"yakin": {1, 0.5}, "uzak": {-0.5, 1}},
		content.Config{FetchLimit: 10, SimilarityThreshold: 0.9}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	got, err := f.Fetch(ctx, "u1")
	if err != nil || len(got) != 1 || got[0].Candidate.PostID != "yakin" {
		t.Fatalf("only candidates in the niche direction must remain: %+v %v", got, err)
	}
}

type vecSource map[string][]float64

func (v vecSource) GetVectorsByPostIDs(_ context.Context, ids []string) (map[string][]float64, error) {
	out := map[string][]float64{}
	for _, id := range ids {
		if x, ok := v[id]; ok {
			out[id] = x
		}
	}
	return out, nil
}
