package synth

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"

	"serendipity/internal/activeusers"
	"serendipity/internal/interactionlog"
	"serendipity/internal/interactions"
	"serendipity/internal/m04interest"
	"serendipity/internal/m04interest/interestsrc"
	"serendipity/internal/postmeta"
	"serendipity/internal/trfold"
)

func TestGenerate_DeterministicForSameSeed(t *testing.T) {
	a := Generate(Config{Seed: 7, Users: 5, Authors: 6, PostsPerAuthor: 4, InteractionsPerUser: 10})
	b := Generate(Config{Seed: 7, Users: 5, Authors: 6, PostsPerAuthor: 4, InteractionsPerUser: 10})
	if !reflect.DeepEqual(a, b) {
		t.Fatal("same seed must produce the same data")
	}
	c := Generate(Config{Seed: 8, Users: 5, Authors: 6, PostsPerAuthor: 4, InteractionsPerUser: 10})
	if reflect.DeepEqual(a.Posts, c.Posts) {
		t.Fatal("different seed must produce different data")
	}
}

func TestGenerate_SizesAndDefaults(t *testing.T) {
	d := Generate(Config{Seed: 1})
	if len(d.Users) != 20 || len(d.Authors) != 12 || len(d.Posts) != 12*15 {
		t.Fatalf("default sizes wrong: %d %d %d", len(d.Users), len(d.Authors), len(d.Posts))
	}
	if len(d.Interactions) != 20*40 {
		t.Fatalf("expected 40 events per user: %d", len(d.Interactions))
	}
}

func TestGenerate_PostsAreCoherent(t *testing.T) {
	d := Generate(Config{Seed: 3, Authors: 6, PostsPerAuthor: 5})
	ids := map[string]bool{}
	topics := map[string]Topic{}
	for _, tp := range Topics {
		topics[tp.Name] = tp
	}
	for _, p := range d.Posts {
		if ids[p.ID] {
			t.Fatalf("duplicate post id: %s", p.ID)
		}
		ids[p.ID] = true
		if len(p.Entities) == 0 {
			t.Fatalf("post without entities: %+v", p)
		}
		for _, e := range p.Entities {
			if !strings.Contains(p.Text, e) {
				t.Fatalf("entity must appear in the text: %q / %q", e, p.Text)
			}
			if !contains(topics[p.Topic].Entities, e) {
				t.Fatalf("entity must belong to its own topic: %s ∉ %s", e, p.Topic)
			}
		}
	}
}

func TestGenerate_InteractionsFavorPreferredTopicsAndAreChronological(t *testing.T) {
	d := Generate(Config{Seed: 5, Users: 10, InteractionsPerUser: 60})
	topicOf := map[string]string{}
	for _, p := range d.Posts {
		topicOf[p.ID] = p.Topic
	}
	pref := map[string]map[string]bool{}
	for _, u := range d.Users {
		pref[u.ID] = map[string]bool{}
		for _, tp := range u.Topics {
			pref[u.ID][tp] = true
		}
	}
	var onTopic, total int
	lastTS := map[string]time.Time{}
	for _, it := range d.Interactions {
		if it.Timestamp.Before(lastTS[it.UserID]) {
			t.Fatalf("user events must be in ascending time order: %s", it.UserID)
		}
		lastTS[it.UserID] = it.Timestamp
		if it.InteractionType == interactions.InteractionSkip {
			if pref[it.UserID][topicOf[it.PostID]] {
				t.Fatal("no skip must be produced on a preferred topic")
			}
			continue
		}
		total++
		if pref[it.UserID][topicOf[it.PostID]] {
			onTopic++
		}
	}
	if float64(onTopic)/float64(total) < 0.7 {
		t.Fatalf("most positives must be on a preferred topic: %d/%d", onTopic, total)
	}
}

func TestGenerate_F2OnlyForPreferredTopics(t *testing.T) {
	d := Generate(Config{Seed: 9, Users: 15})
	authorTopic := map[string]string{}
	for _, a := range d.Authors {
		authorTopic[a.ID] = a.Topic
	}
	for _, u := range d.Users {
		for a, mutual := range d.F2[u.ID] {
			if !contains(u.Topics, authorTopic[a]) || mutual < 1 || mutual > 5 {
				t.Fatalf("F2 author must be from a preferred topic with 1-5 mutual: %s %s %v", u.ID, a, mutual)
			}
		}
	}
}

func newRedis(t *testing.T) *goredis.Client {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type resolver struct{ pm *postmeta.Store }

func (r resolver) EntitiesOf(ctx context.Context, ids []string) (map[string][]string, error) {
	return r.pm.GetEntitiesBatch(ctx, ids)
}

// TestWrittenDataFeedsRealConsumers: when the generated data is written to Redis the real readers work —
// interaction log, entity resolution, active set and the interest profile builder. Each user's
// heaviest "lasting" entity must be one of their own preferred topics.
func TestWrittenDataFeedsRealConsumers(t *testing.T) {
	ctx := context.Background()
	rdb := newRedis(t)
	ds := Generate(Config{Seed: 11, Users: 8, Authors: 6, PostsPerAuthor: 10, InteractionsPerUser: 50, Now: time.Now()})
	sum, err := Write(ctx, rdb, ds, quiet())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Posts != 60 || sum.Users != 8 || sum.Interactions != 8*50 {
		t.Fatalf("summary wrong: %+v", sum)
	}

	users, err := activeusers.New(rdb, "").All(ctx, 3)
	if err != nil || len(users) != 8 {
		t.Fatalf("expected 8 active users: %v %v", users, err)
	}

	pm := postmeta.New(rdb, postmeta.Config{}, quiet())
	ilog := interactionlog.New(rdb, 0, 0)
	res := resolver{pm}
	builder := m04interest.NewBuilder(
		interestsrc.NewRingPositives(ilog, res, 200),
		interestsrc.NewMirrorNegatives(ilog, res, 30),
		m04interest.NewRedisStore(rdb, 24*time.Hour, quiet()),
		m04interest.BuildConfig{}, quiet())

	topicEntities := map[string][]string{}
	for _, tp := range Topics {
		topicEntities[tp.Name] = tp.Entities
	}
	for _, u := range ds.Users {
		prof, err := builder.BuildUser(ctx, u.ID, time.Now())
		if err != nil {
			t.Fatalf("%s: %v", u.ID, err)
		}
		top := heaviest(prof.Stable)
		var preferred []string
		for _, tp := range u.Topics {
			for _, e := range topicEntities[tp] {
				preferred = append(preferred, trfold.Fold(e)) // profile keys are folded
			}
		}
		if !contains(preferred, top) {
			t.Fatalf("%s: heaviest entity %q must be one of the preferred topics (%v); profil=%v", u.ID, top, u.Topics, prof.Stable)
		}
	}

	for u, edges := range ds.F2 {
		n, _ := rdb.ZCard(ctx, F2Key(u)).Result()
		if int(n) != len(edges) {
			t.Fatalf("F2 edge count mismatch: %s %d/%d", u, n, len(edges))
		}
	}
	ids, _ := ilog.RecentPostIDs(ctx, ds.Users[0].ID, 100)
	if len(ids) == 0 {
		t.Fatal("post ids the niche profiler would read are empty")
	}
}

func heaviest(w m04interest.Weights) string {
	keys := make([]string, 0, len(w))
	for k := range w {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if w[keys[i]] != w[keys[j]] {
			return w[keys[i]] > w[keys[j]]
		}
		return keys[i] < keys[j]
	})
	if len(keys) == 0 {
		return ""
	}
	return keys[0]
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// Authors must differ in popularity (otherwise the booster's author trust is the same, or zero,
// for everyone), and the counters must reach the post HASHes where the booster reads them.
func TestWrittenDataCarriesEngagementCounters(t *testing.T) {
	ctx := context.Background()
	rdb := newRedis(t)
	ds := Generate(Config{Seed: 3, Users: 4, Authors: 6, PostsPerAuthor: 10, InteractionsPerUser: 10, Now: time.Now()})
	if _, err := Write(ctx, rdb, ds, quiet()); err != nil {
		t.Fatal(err)
	}
	pm := postmeta.New(rdb, postmeta.Config{}, quiet())
	ids := make([]string, len(ds.Posts))
	for i, p := range ds.Posts {
		ids[i] = p.ID
	}
	got, err := pm.Hydrate(ctx, ids)
	if err != nil {
		t.Fatal(err)
	}
	sumByAuthor := map[string]int64{}
	for _, p := range ds.Posts {
		if got[p.ID].Likes != p.Likes || got[p.ID].Reblogs != p.Reblogs {
			t.Fatalf("%s: stored counters %d/%d, generated %d/%d", p.ID, got[p.ID].Likes, got[p.ID].Reblogs, p.Likes, p.Reblogs)
		}
		sumByAuthor[p.AuthorID] += p.Likes + p.Reblogs
	}
	distinct := map[int64]struct{}{}
	for _, v := range sumByAuthor {
		if v <= 0 {
			t.Fatalf("an author has zero engagement: %v", sumByAuthor)
		}
		distinct[v] = struct{}{}
	}
	if len(distinct) < 2 {
		t.Fatalf("all authors have the same popularity: %v", sumByAuthor)
	}
}
