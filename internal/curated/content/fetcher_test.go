package content

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"testing"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"

	"serendipity/internal/curated/model"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type fakeHydrator struct {
	data map[string]model.PostData
	err  error
}

func (f *fakeHydrator) Hydrate(_ context.Context, postIDs []string) (map[string]model.PostData, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make(map[string]model.PostData, len(postIDs))
	for _, id := range postIDs {
		if pd, ok := f.data[id]; ok {
			out[id] = pd
		}
	}
	return out, nil
}

// fakePostVectorSource is the test double of IPostVectorSource — instead of the real
// Qdrant-backed store, it returns an in-memory post_id → embedding map
// (Strategy 3: the original post embedding is read from the vector store — the test
// imitates this without building a real store).
type fakePostVectorSource struct {
	vectors map[string][]float64
	err     error
}

func (f *fakePostVectorSource) GetVectorsByPostIDs(_ context.Context, postIDs []string) (map[string][]float64, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make(map[string][]float64, len(postIDs))
	for _, id := range postIDs {
		if v, ok := f.vectors[id]; ok {
			out[id] = v
		}
	}
	return out, nil
}

func TestFetcher_ReturnsScoredCandidatesInZSetOrder(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})

	mr.ZAdd(boostedZSetKey, 3.0, "p1")
	mr.ZAdd(boostedZSetKey, 5.0, "p2")
	mr.ZAdd(boostedZSetKey, 1.0, "p3")

	hydrator := &fakeHydrator{data: map[string]model.PostData{
		"p1": {AuthorID: "a1"},
		"p2": {AuthorID: "a2"},
		"p3": {AuthorID: "a3"},
	}}

	f := New(rdb, hydrator, &fakePostVectorSource{}, Config{FetchLimit: 10}, testLogger())
	got, err := f.Fetch(context.Background(), "any-user")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 candidates, got %d", len(got))
	}
	// ZREVRANGE highest score first: p2(5.0) > p1(3.0) > p3(1.0)
	if got[0].Candidate.PostID != "p2" || got[1].Candidate.PostID != "p1" || got[2].Candidate.PostID != "p3" {
		t.Errorf("wrong order: %s, %s, %s", got[0].Candidate.PostID, got[1].Candidate.PostID, got[2].Candidate.PostID)
	}
	if got[0].Candidate.AuthorID != "a2" {
		t.Errorf("expected AuthorID from hydrate, got %s", got[0].Candidate.AuthorID)
	}
}

func TestFetcher_EmptyZSet_ReturnsEmptyNoError(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})

	f := New(rdb, &fakeHydrator{}, &fakePostVectorSource{}, Config{}, testLogger())
	got, err := f.Fetch(context.Background(), "any-user")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want 0 candidates, got %d", len(got))
	}
}

func TestFetcher_StalePostID_SkippedNotErrored(t *testing.T) {
	// The post is in the ZSET but postdata:{id} was deleted by TTL (7 days) — Hydrate returns empty
	// AuthorID, the Fetcher MUST SKIP this candidate (so a nil candidate does not leak into the feed).
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})

	mr.ZAdd(boostedZSetKey, 3.0, "stale1")
	mr.ZAdd(boostedZSetKey, 5.0, "fresh1")

	hydrator := &fakeHydrator{data: map[string]model.PostData{
		"fresh1": {AuthorID: "a1"},
		// "stale1" is deliberately missing from the hydrator map → empty PostData
	}}

	f := New(rdb, hydrator, &fakePostVectorSource{}, Config{FetchLimit: 10}, testLogger())
	got, err := f.Fetch(context.Background(), "any-user")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(got) != 1 || got[0].Candidate.PostID != "fresh1" {
		t.Errorf("expected only fresh1 (stale1 skipped for empty AuthorID), got %+v", got)
	}
}

func TestFetcher_HydrateError_ReturnsError(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	mr.ZAdd(boostedZSetKey, 3.0, "p1")

	f := New(rdb, &fakeHydrator{err: errors.New("boom")}, &fakePostVectorSource{}, Config{}, testLogger())
	_, err = f.Fetch(context.Background(), "any-user")
	if err == nil {
		t.Error("expected error when hydrate fails")
	}
}

func writeUserNiche(t *testing.T, mr *miniredis.Miniredis, userID string, niches []nicheEntry) {
	t.Helper()
	data, err := json.Marshal(niches)
	if err != nil {
		t.Fatalf("marshal niche: %v", err)
	}
	mr.Set(userNicheKey(userID), string(data))
}

func TestFetcher_NicheMatch_FiltersToSimilarCategory(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})

	// p1: original post embedding CLOSE to Fenerbahçe, p2: far.
	mr.ZAdd(boostedZSetKey, 5.0, "p1")
	mr.ZAdd(boostedZSetKey, 5.0, "p2")

	fenerEmb := []float64{1.0, 0.0, 0.0}
	farEmb := []float64{0.0, 1.0, 0.0}
	postVecs := &fakePostVectorSource{vectors: map[string][]float64{
		"p1": fenerEmb,
		"p2": farEmb,
	}}

	writeUserNiche(t, mr, "user1", []nicheEntry{{Category: "Fenerbahçe", Embedding: fenerEmb}})

	hydrator := &fakeHydrator{data: map[string]model.PostData{
		"p1": {AuthorID: "a1"}, "p2": {AuthorID: "a2"},
	}}
	f := New(rdb, hydrator, postVecs, Config{FetchLimit: 10, SimilarityThreshold: 0.5}, testLogger())
	got, err := f.Fetch(context.Background(), "user1")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	// p1 (similar) must stay in the pool, p2 (far, cosine≈0) must be dropped.
	found := map[string]bool{}
	for _, c := range got {
		found[c.Candidate.PostID] = true
	}
	if !found["p1"] {
		t.Error("expected p1 (niche match) in result")
	}
	if found["p2"] {
		t.Error("expected p2 (niche mismatch) to be filtered out")
	}
}

func TestFetcher_NoEmbedding_MuafFromNicheFilter(t *testing.T) {
	// spec § 3.7 fail-soft: if the original post embedding is MISSING the candidate is not dropped, it is included.
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})

	mr.ZAdd(boostedZSetKey, 5.0, "p1") // postVecs is empty — NO embedding

	writeUserNiche(t, mr, "user1", []nicheEntry{{Category: "Fenerbahçe", Embedding: []float64{1.0, 0.0}}})

	hydrator := &fakeHydrator{data: map[string]model.PostData{"p1": {AuthorID: "a1"}}}
	f := New(rdb, hydrator, &fakePostVectorSource{}, Config{FetchLimit: 10, SimilarityThreshold: 0.5}, testLogger())
	got, err := f.Fetch(context.Background(), "user1")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(got) != 1 || got[0].Candidate.PostID != "p1" {
		t.Errorf("expected p1 to be INCLUDED (muaf, no embedding), got %+v", got)
	}
}

func TestFetcher_NoUserNiche_AllCandidatesIncluded(t *testing.T) {
	// If the user has no niche data at all (Job B has not run yet) the filter
	// is not applied — quality_score order is preserved (the first days of the rollout).
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})

	mr.ZAdd(boostedZSetKey, 5.0, "p1")
	postVecs := &fakePostVectorSource{vectors: map[string][]float64{"p1": {1.0, 0.0}}}

	hydrator := &fakeHydrator{data: map[string]model.PostData{"p1": {AuthorID: "a1"}}}
	f := New(rdb, hydrator, postVecs, Config{FetchLimit: 10, SimilarityThreshold: 0.5}, testLogger())
	got, err := f.Fetch(context.Background(), "user-with-no-niche")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("expected p1 included when user has no niche data, got %+v", got)
	}
}

// vecWithSim returns a unit vector whose cosine to the niche vector [1, 0] is sim.
func vecWithSim(sim float64) []float64 {
	return []float64{sim, math.Sqrt(1 - sim*sim)}
}

func nicheFixture(t *testing.T, sims map[string]float64, scores map[string]float64) (*miniredis.Miniredis, *goredis.Client, *fakeHydrator, *fakePostVectorSource) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	hyd := &fakeHydrator{data: map[string]model.PostData{}}
	vecs := &fakePostVectorSource{vectors: map[string][]float64{}}
	for id, sim := range sims {
		mr.ZAdd(boostedZSetKey, scores[id], id)
		hyd.data[id] = model.PostData{AuthorID: "a-" + id}
		vecs.vectors[id] = vecWithSim(sim)
	}
	writeUserNiche(t, mr, "u1", []nicheEntry{{Category: "x", Embedding: []float64{1, 0}}})
	return mr, rdb, hyd, vecs
}

// With tied pool scores the top-FetchLimit by score is arbitrary; the niche filter must look
// past it (ScanLimit) and still return the posts that match the niche.
func TestFetcher_ScansBeyondFetchLimitBeforeNicheFilter(t *testing.T) {
	sims := map[string]float64{}
	scores := map[string]float64{}
	for i := 0; i < 30; i++ {
		id := fmt.Sprintf("far%02d", i)
		sims[id] = 0.5
		scores[id] = 0 // all tied
	}
	sims["near1"], sims["near2"] = 0.95, 0.93
	scores["near1"], scores["near2"] = 0, 0
	_, rdb, hyd, vecs := nicheFixture(t, sims, scores)

	f := New(rdb, hyd, vecs, Config{FetchLimit: 3}, testLogger())
	got, err := f.Fetch(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	ids := candidateIDs(got)
	if len(ids) != 2 || ids[0] != "near1" || ids[1] != "near2" {
		t.Fatalf("want [near1 near2] ordered by similarity, got %v", ids)
	}
}

// The relative margin keeps only candidates close to the user's best match, even when the
// absolute threshold would let everything through.
func TestFetcher_RelativeMarginAdaptsToTheUsersBestMatch(t *testing.T) {
	sims := map[string]float64{"best": 0.92, "close": 0.90, "loose": 0.80}
	scores := map[string]float64{"best": 1, "close": 1, "loose": 1}
	_, rdb, hyd, vecs := nicheFixture(t, sims, scores)

	f := New(rdb, hyd, vecs, Config{FetchLimit: 10, SimilarityThreshold: 0.5}, testLogger())
	got, _ := f.Fetch(context.Background(), "u1")
	ids := candidateIDs(got)
	if len(ids) != 2 || ids[0] != "best" || ids[1] != "close" {
		t.Fatalf("margin must drop loose: %v", ids)
	}

	// A negative margin turns the relative cut off: the absolute threshold alone applies.
	f = New(rdb, hyd, vecs, Config{FetchLimit: 10, SimilarityThreshold: 0.5, RelativeMargin: -1}, testLogger())
	got, _ = f.Fetch(context.Background(), "u1")
	if len(got) != 3 {
		t.Fatalf("relative cut off: want 3, got %v", candidateIDs(got))
	}
}

// The absolute threshold still wins when it is stricter than the relative cut.
func TestFetcher_AbsoluteThresholdStillApplies(t *testing.T) {
	sims := map[string]float64{"a": 0.85, "b": 0.84}
	scores := map[string]float64{"a": 1, "b": 1}
	_, rdb, hyd, vecs := nicheFixture(t, sims, scores)
	f := New(rdb, hyd, vecs, Config{FetchLimit: 10, SimilarityThreshold: 0.9}, testLogger())
	if got, _ := f.Fetch(context.Background(), "u1"); len(got) != 0 {
		t.Fatalf("both are below the absolute threshold, got %v", candidateIDs(got))
	}
}

func candidateIDs(in []model.ScoredCandidate) []string {
	out := make([]string, len(in))
	for i, c := range in {
		out[i] = c.Candidate.PostID
	}
	return out
}
