package postindex

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"

	"serendipity/internal/core"
	"serendipity/internal/curated/content"
	"serendipity/internal/embedclient"
	"serendipity/internal/postmeta"
	"serendipity/internal/vectordb"
)

// fakeEmbedder turns text into a deterministic 3-dimensional normalized vector:
// texts containing "kedi" go on the x axis, those containing "araba" on the y axis.
type fakeEmbedder struct {
	calls [][]string
	kinds []embedclient.Kind
	err   error
}

func (f *fakeEmbedder) Embed(_ context.Context, texts []string, kind embedclient.Kind) ([][]float32, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.calls = append(f.calls, texts)
	f.kinds = append(f.kinds, kind)
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := []float32{0.1, 0.1, 0.1}
		if strings.Contains(t, "kedi") {
			v[0] = 1
		}
		if strings.Contains(t, "araba") {
			v[1] = 1
		}
		var n float64
		for _, x := range v {
			n += float64(x * x)
		}
		n = math.Sqrt(n)
		for j := range v {
			v[j] = float32(float64(v[j]) / n)
		}
		out[i] = v
	}
	return out, nil
}

// memDB is an in-memory cosine implementation of VectorDB (for tests only).
type memDB struct {
	dim       int
	ensured   int
	points    map[string]vectordb.Point
	upsertErr error
}

func newMemDB() *memDB { return &memDB{points: map[string]vectordb.Point{}} }

func (m *memDB) EnsureCollection(_ context.Context, _ string, dim int) error {
	m.ensured++
	m.dim = dim
	return nil
}

func (m *memDB) Upsert(_ context.Context, _ string, pts []vectordb.Point) error {
	if m.upsertErr != nil {
		return m.upsertErr
	}
	for _, p := range pts {
		m.points[p.ID] = p
	}
	return nil
}

func cos(a, b []float32) float64 {
	var d, na, nb float64
	for i := range a {
		d += float64(a[i] * b[i])
		na += float64(a[i] * a[i])
		nb += float64(b[i] * b[i])
	}
	return d / (math.Sqrt(na) * math.Sqrt(nb))
}

func (m *memDB) Search(_ context.Context, _ string, r vectordb.SearchRequest) ([]vectordb.Hit, error) {
	var hits []vectordb.Hit
	for _, p := range m.points {
		ok := true
		for k, v := range r.Match {
			if p.Payload[k] != v {
				ok = false
			}
		}
		if !ok {
			continue
		}
		s := cos(r.Vector, p.Vector)
		if r.MinScore > 0 && s < r.MinScore {
			continue
		}
		hits = append(hits, vectordb.Hit{ID: p.ID, Score: s, Payload: p.Payload})
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if r.Limit > 0 && len(hits) > r.Limit {
		hits = hits[:r.Limit]
	}
	return hits, nil
}

func (m *memDB) GetVectors(_ context.Context, _ string, ids []string) (map[string][]float32, error) {
	out := map[string][]float32{}
	for _, id := range ids {
		if p, ok := m.points[id]; ok {
			out[id] = p.Vector
		}
	}
	return out, nil
}

func (m *memDB) Delete(context.Context, string, []string) error { return nil }

var (
	_ vectordb.VectorDB         = (*memDB)(nil)
	_ content.IPostVectorSource = (*VectorSource)(nil)
	_ embedclient.Embedder      = (*fakeEmbedder)(nil)
)

func TestIndex_SkipsEmptyBatchesAndEnsuresOnce(t *testing.T) {
	emb, db := &fakeEmbedder{}, newMemDB()
	ix, err := NewIndexer(emb, db, "posts", 2)
	if err != nil {
		t.Fatal(err)
	}
	n, err := ix.Index(context.Background(), []Post{
		{ID: "p1", Text: "kedi"}, {ID: "p2", Text: "  "}, {ID: "", Text: "x"},
		{ID: "p3", Text: "araba"}, {ID: "p4", Text: "kedi araba"},
	})
	if err != nil || n != 3 {
		t.Fatalf("3 post should have been written: %d %v", n, err)
	}
	if len(emb.calls) != 2 || len(emb.calls[0]) != 2 || len(emb.calls[1]) != 1 {
		t.Fatalf("wrong batch split: %v", emb.calls)
	}
	if emb.kinds[0] != embedclient.Passage {
		t.Fatal("indeksleme passage olarak embed edilmeli")
	}
	if db.ensured != 1 || db.dim != 3 {
		t.Fatalf("collection must be created once, with the vector size: %d %d", db.ensured, db.dim)
	}
	if db.points["p3"].Payload["author_id"] != "" || len(db.points) != 3 {
		t.Fatalf("wrong points: %v", db.points)
	}
}

func TestIndex_ErrorsKeepWrittenCount(t *testing.T) {
	emb, db := &fakeEmbedder{}, newMemDB()
	ix, _ := NewIndexer(emb, db, "posts", 1)
	db.upsertErr = errors.New("disk dolu")
	n, err := ix.Index(context.Background(), []Post{{ID: "p1", Text: "kedi"}})
	if err == nil || n != 0 || !strings.Contains(err.Error(), "disk dolu") {
		t.Fatalf("write error expected: %d %v", n, err)
	}
	emb.err = errors.New("service unavailable")
	if _, err := ix.Index(context.Background(), []Post{{ID: "p2", Text: "kedi"}}); err == nil {
		t.Fatal("embed error expected")
	}
}

func TestNewIndexer_Validation(t *testing.T) {
	if _, err := NewIndexer(nil, newMemDB(), "c", 0); err == nil {
		t.Fatal("nil embedder was accepted")
	}
	if _, err := NewIndexer(&fakeEmbedder{}, newMemDB(), "", 0); err == nil {
		t.Fatal("empty collection was accepted")
	}
}

func TestSimilar_RanksByMeaningAndFilters(t *testing.T) {
	emb, db := &fakeEmbedder{}, newMemDB()
	ix, _ := NewIndexer(emb, db, "posts", 0)
	_, _ = ix.Index(context.Background(), []Post{
		{ID: "k1", AuthorID: "a", Category: "hayvan", Text: "kedi"},
		{ID: "k2", AuthorID: "b", Category: "hayvan", Text: "kedi kedi"},
		{ID: "c1", AuthorID: "a", Category: "oto", Text: "araba"},
	})
	hits, err := ix.Similar(context.Background(), "kedi", SimilarRequest{Limit: 2})
	if err != nil || len(hits) != 2 || hits[0].ID[0] != 'k' || hits[1].ID[0] != 'k' {
		t.Fatalf("the two nearest posts must be the cat ones: %+v %v", hits, err)
	}
	if emb.kinds[len(emb.kinds)-1] != embedclient.Query {
		t.Fatal("arama query olarak embed edilmeli")
	}
	hits, _ = ix.Similar(context.Background(), "kedi", SimilarRequest{AuthorID: "a"})
	for _, h := range hits {
		if h.Payload["author_id"] != "a" {
			t.Fatalf("author filter was not applied: %+v", hits)
		}
	}
	hits, _ = ix.Similar(context.Background(), "kedi", SimilarRequest{Category: "oto"})
	if len(hits) != 1 || hits[0].ID != "c1" {
		t.Fatalf("wrong category filter: %+v", hits)
	}
	if _, err := ix.Similar(context.Background(), "  ", SimilarRequest{}); err == nil {
		t.Fatal("empty query was accepted")
	}
}

func TestVectorSource_ConvertsToFloat64AndSkipsMissing(t *testing.T) {
	db := newMemDB()
	db.points["p1"] = vectordb.Point{ID: "p1", Vector: []float32{0.5, 0.25}}
	out, err := NewVectorSource(db, "posts").GetVectorsByPostIDs(context.Background(), []string{"p1", "yok"})
	if err != nil || len(out) != 1 || out["p1"][0] != 0.5 || out["p1"][1] != 0.25 {
		t.Fatalf("wrong conversion: %v %v", out, err)
	}
}

// TestCuratedFetcher_UsesVectorSource shows the pipeline is wired end to end:
// the indexer writes the posts, the curated Fetcher reads the vectors for niche matching
// through VectorSource and drops the candidate that does not resemble the niche vector.
func TestCuratedFetcher_UsesVectorSource(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	pm := postmeta.New(rdb, postmeta.Config{TTL: time.Hour}, nil)
	if err := pm.SaveBatch(ctx, []core.RawPost{
		{PostID: "kedi-post", AuthorID: "a1", Text: "kedi", CreatedAt: time.Now()},
		{PostID: "araba-post", AuthorID: "a2", Text: "araba", CreatedAt: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}
	rdb.ZAdd(ctx, "serendipity:curated:boosted",
		goredis.Z{Score: 2, Member: "kedi-post"}, goredis.Z{Score: 1, Member: "araba-post"})

	emb, db := &fakeEmbedder{}, newMemDB()
	ix, _ := NewIndexer(emb, db, "posts", 0)
	if _, err := ix.Index(ctx, []Post{{ID: "kedi-post", Text: "kedi"}, {ID: "araba-post", Text: "araba"}}); err != nil {
		t.Fatal(err)
	}

	// The user's niche vector points in the "kedi" direction.
	nicheVec, _ := emb.Embed(ctx, []string{"kedi"}, embedclient.Query)
	niche := make([]float64, len(nicheVec[0]))
	for i, x := range nicheVec[0] {
		niche[i] = float64(x)
	}
	rdb.Set(ctx, "serendipity:user:niche:u1", `[{"category":"hayvan","embedding":`+jsonFloats(niche)+`}]`, 0)

	f := content.New(rdb, pm, NewVectorSource(db, "posts"),
		content.Config{FetchLimit: 10, SimilarityThreshold: 0.9},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	got, err := f.Fetch(ctx, "u1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Candidate.PostID != "kedi-post" {
		t.Fatalf("only candidates similar to the niche vector must remain: %+v", got)
	}
}

func jsonFloats(v []float64) string {
	b, _ := json.Marshal(v)
	return string(b)
}
