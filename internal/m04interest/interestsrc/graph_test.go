package interestsrc

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"

	"serendipity/internal/core"
	"serendipity/internal/m04interest"
)

type fakeFav []core.AuthorAffinity

func (f fakeFav) GetFavoriteAuthors(_ context.Context, _ string) ([]core.AuthorAffinity, error) {
	return f, nil
}

func graphEnv(t *testing.T) (*goredis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb, mr
}

func TestAuthorContentGraph_FavoriteAuthorPostEntities(t *testing.T) {
	rdb, mr := graphEnv(t)
	at := time.Now().Add(-time.Hour)
	mr.ZAdd("serendipity:author:posts:a1", float64(at.Unix()), "p1")

	g := NewAuthorContentGraph(
		fakeFav{{AuthorID: "a1"}},
		rdb, rdb,
		fakeResolver{"p1": {"Fenerbahçe"}},
		GraphConfig{MaxAuthors: 10, PostsPerAuthor: 5},
	)
	sigs, err := g.Prior(context.Background(), "u1", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sigs) != 1 || sigs[0].Entities[0] != "Fenerbahçe" {
		t.Fatalf("sigs = %+v", sigs)
	}
	// At must be the post's REAL publish time, not "now" — otherwise the graph
	// signal never ages.
	if sigs[0].At.Unix() != at.Unix() {
		t.Fatalf("At = %v, want %v", sigs[0].At, at)
	}
}

func TestAuthorContentGraph_PostBeforeSinceSkipped(t *testing.T) {
	// Critical: without the `since` gate, the same neighbor post is re-added on every run,
	// the graph contribution accumulates per run and the "weak prior" guarantee
	// collapses.
	rdb, mr := graphEnv(t)
	now := time.Now()
	mr.ZAdd("serendipity:author:posts:a1", float64(now.Add(-48*time.Hour).Unix()), "eski")
	mr.ZAdd("serendipity:author:posts:a1", float64(now.Add(-1*time.Hour).Unix()), "yeni")

	g := NewAuthorContentGraph(fakeFav{{AuthorID: "a1"}}, rdb, rdb,
		fakeResolver{"eski": {"A"}, "yeni": {"B"}},
		GraphConfig{MaxAuthors: 10, PostsPerAuthor: 5})

	sigs, err := g.Prior(context.Background(), "u1", now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(sigs) != 1 || sigs[0].Entities[0] != "B" {
		t.Fatalf("sigs = %+v", sigs)
	}
}

func TestAuthorContentGraph_F2AuthorsAreAlsoRead(t *testing.T) {
	rdb, mr := graphEnv(t)
	mr.ZAdd("serendipity:user:f2_authors:u1", 3, "a2")
	mr.ZAdd("serendipity:author:posts:a2", float64(time.Now().Unix()), "p2")

	g := NewAuthorContentGraph(fakeFav{}, rdb, rdb, fakeResolver{"p2": {"Galatasaray"}},
		GraphConfig{MaxAuthors: 10, PostsPerAuthor: 5})
	sigs, err := g.Prior(context.Background(), "u1", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sigs) != 1 || sigs[0].Entities[0] != "Galatasaray" {
		t.Fatalf("sigs = %+v", sigs)
	}
}

func TestAuthorContentGraph_MaxAuthorsCeilingAppliesOverTWO_SOURCES(t *testing.T) {
	// The F2 set can be large; without a ceiling, hundreds of
	// ZREVRANGEBYSCORE + entity reads would be done for a single user.
	//
	// The ceiling must be applied to the SUM OF THE TWO SOURCES. A test that fills only F2
	// would also pass with a faulty implementation that applies the ceiling PER SOURCE;
	// that is why a favorite is supplied too.
	rdb, mr := graphEnv(t)
	ts := float64(time.Now().Unix())
	for _, a := range []string{"f1", "f2"} {
		mr.ZAdd("serendipity:author:posts:"+a, ts, "p-"+a)
	}
	for _, a := range []string{"g1", "g2", "g3"} {
		mr.ZAdd("serendipity:user:f2_authors:u1", 1, a)
		mr.ZAdd("serendipity:author:posts:"+a, ts, "p-"+a)
	}
	res := fakeResolver{
		"p-f1": {"F1"}, "p-f2": {"F2"},
		"p-g1": {"G1"}, "p-g2": {"G2"}, "p-g3": {"G3"},
	}
	// MaxAuthors=3: two favorites + ONE F2 must be taken (favorites first).
	g := NewAuthorContentGraph(fakeFav{{AuthorID: "f1"}, {AuthorID: "f2"}}, rdb, rdb, res,
		GraphConfig{MaxAuthors: 3, PostsPerAuthor: 5})

	sigs, err := g.Prior(context.Background(), "u1", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sigs) != 3 {
		t.Fatalf("cap not applied to the SUM of both sources: %d signals", len(sigs))
	}
	got := map[string]bool{}
	for _, s := range sigs {
		got[s.Entities[0]] = true
	}
	// Favorites derive from the user's OWN behavior; when the ceiling truncates,
	// F2 must drop first.
	if !got["F1"] || !got["F2"] {
		t.Fatalf("favorites truncated: %v", got)
	}
}

func TestAuthorContentGraph_NoAuthorReturnsEmpty(t *testing.T) {
	rdb, _ := graphEnv(t)
	g := NewAuthorContentGraph(fakeFav{}, rdb, rdb, fakeResolver{}, GraphConfig{MaxAuthors: 10, PostsPerAuthor: 5})
	sigs, err := g.Prior(context.Background(), "u1", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sigs) != 0 {
		t.Fatalf("sigs = %+v", sigs)
	}
}

func TestAuthorContentGraph_SameAuthorInTwoSourcesCountedOnce(t *testing.T) {
	rdb, mr := graphEnv(t)
	mr.ZAdd("serendipity:user:f2_authors:u1", 1, "a1")
	mr.ZAdd("serendipity:author:posts:a1", float64(time.Now().Unix()), "p1")

	g := NewAuthorContentGraph(fakeFav{{AuthorID: "a1"}}, rdb, rdb,
		fakeResolver{"p1": {"A"}}, GraphConfig{MaxAuthors: 10, PostsPerAuthor: 5})
	sigs, err := g.Prior(context.Background(), "u1", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sigs) != 1 {
		t.Fatalf("author counted twice: %+v", sigs)
	}
}

var _ m04interest.GraphSource = (*AuthorContentGraph)(nil)

// The stored format is a ZSET : reading it as a SET returned WRONGTYPE every time
// and F2 silently vanished. Also, when the ceiling truncates, the authors with
// the most mutual connections (high score) must stay.
func TestAuthorContentGraph_F2ZSETInMutualCountOrder(t *testing.T) {
	rdb, mr := graphEnv(t)
	ts := float64(time.Now().Unix())
	for a, mutual := range map[string]float64{"az": 1, "cok": 9, "orta": 5} {
		mr.ZAdd("serendipity:user:f2_authors:u1", mutual, a)
		mr.ZAdd("serendipity:author:posts:"+a, ts, "p-"+a)
	}
	g := NewAuthorContentGraph(fakeFav{}, rdb, rdb,
		fakeResolver{"p-az": {"AZ"}, "p-cok": {"COK"}, "p-orta": {"ORTA"}},
		GraphConfig{MaxAuthors: 2, PostsPerAuthor: 5})
	sigs, err := g.Prior(context.Background(), "u1", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, s := range sigs {
		got[s.Entities[0]] = true
	}
	if len(got) != 2 || !got["COK"] || !got["ORTA"] {
		t.Fatalf("F2 must be trimmed by mutual_count order (MANY+MID), got %v", got)
	}
}

// The old SET key from the rollout window: WRONGTYPE → F2 is skipped, favorites
// still work, the error is not propagated upward.
func TestAuthorContentGraph_OldSETKeySkipsF2(t *testing.T) {
	rdb, mr := graphEnv(t)
	ts := float64(time.Now().Unix())
	mr.SAdd("serendipity:user:f2_authors:u1", "g1")
	mr.ZAdd("serendipity:author:posts:g1", ts, "p-g1")
	mr.ZAdd("serendipity:author:posts:f1", ts, "p-f1")
	g := NewAuthorContentGraph(fakeFav{{AuthorID: "f1"}}, rdb, rdb,
		fakeResolver{"p-g1": {"G1"}, "p-f1": {"F1"}}, GraphConfig{MaxAuthors: 10, PostsPerAuthor: 5})
	sigs, err := g.Prior(context.Background(), "u1", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sigs) != 1 || sigs[0].Entities[0] != "F1" {
		t.Fatalf("F2 must be skipped on old SET, favorite must stay: %+v", sigs)
	}
}

// Until: only posts published before this moment (time-split measurement).
func TestAuthorContentGraph_PostAfterUntilSkipped(t *testing.T) {
	rdb, mr := graphEnv(t)
	now := time.Now()
	mr.ZAdd("serendipity:author:posts:a1", float64(now.Add(-48*time.Hour).Unix()), "eski")
	mr.ZAdd("serendipity:author:posts:a1", float64(now.Add(-1*time.Hour).Unix()), "yeni")
	g := NewAuthorContentGraph(fakeFav{{AuthorID: "a1"}}, rdb, rdb,
		fakeResolver{"eski": {"A"}, "yeni": {"B"}},
		GraphConfig{MaxAuthors: 10, PostsPerAuthor: 5, Until: now.Add(-24 * time.Hour)})
	sigs, err := g.Prior(context.Background(), "u1", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sigs) != 1 || sigs[0].Entities[0] != "A" {
		t.Fatalf("post after Until must be skipped: %+v", sigs)
	}
}

// If a single author's key is corrupt (WRONGTYPE) in the pipeline, the others are still read.
func TestAuthorContentGraph_CorruptAuthorKeyDoesNotDropOthers(t *testing.T) {
	rdb, mr := graphEnv(t)
	ts := float64(time.Now().Unix())
	mr.Set("serendipity:author:posts:bozuk", "string-deger")
	mr.ZAdd("serendipity:author:posts:iyi", ts, "p-iyi")
	g := NewAuthorContentGraph(fakeFav{{AuthorID: "bozuk"}, {AuthorID: "iyi"}}, rdb, rdb,
		fakeResolver{"p-iyi": {"IYI"}}, GraphConfig{MaxAuthors: 10, PostsPerAuthor: 5})
	sigs, err := g.Prior(context.Background(), "u1", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sigs) != 1 || sigs[0].Entities[0] != "IYI" {
		t.Fatalf("a corrupt author must not drop the others: %+v", sigs)
	}
}
