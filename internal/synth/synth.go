// Package synth produces consistent synthetic data for experiments: authors and posts tied to topics
// (text + entity), users with topic preferences, interactions matching those preferences, and F2 author
// sets. Generation is pure and deterministic (same Config → same data); writing to Redis is in redis.go.
//
// Synthetic data does not imitate a real distribution; the aim is to see the pipelines (interest profile, niche profile,
// entity slot, similar-post search) work end to end and to play with the parts.
package synth

import (
	"fmt"
	"math/rand"
	"sort"
	"time"

	"serendipity/internal/interactions"
)

// Topic is the vocabulary of a topic: entities are used like "proper names", words like ordinary text.
type Topic struct {
	Name     string
	Entities []string
	Words    []string
}

// Topics is the fixed topic table used in generation.
var Topics = []Topic{
	{"futbol", []string{"Fenerbahçe", "Galatasaray", "Beşiktaş", "Trabzonspor"}, []string{"maç", "gol", "transfer", "teknik direktör", "tribün", "lig"}},
	{"teknoloji", []string{"Go", "Redis", "Qdrant", "Kubernetes"}, []string{"sunucu", "veritabanı", "hız", "ölçek", "dağıtım", "kod"}},
	{"müzik", []string{"Anadolu rock", "Saz", "Zeybek", "Arabesk"}, []string{"albüm", "konser", "şarkı", "sahne", "festival", "beste"}},
	{"sinema", []string{"Yeşilçam", "Altın Portakal", "Cannes", "Belgesel"}, []string{"film", "vizyon", "senaryo", "yönetmen", "sahne", "oyuncu"}},
	{"seyahat", []string{"Kapadokya", "İstanbul", "Antalya", "Mardin"}, []string{"gezi", "rota", "otel", "manzara", "bilet", "yolculuk"}},
	{"yemek", []string{"baklava", "mantı", "iskender", "lahmacun"}, []string{"tarif", "lezzet", "restoran", "mutfak", "malzeme", "sofra"}},
}

// Config determines the size and seed of the generation. Zero fields are pulled to defaults.
type Config struct {
	Seed                int64
	Users               int
	Authors             int
	PostsPerAuthor      int
	InteractionsPerUser int
	// Now is the reference for timestamps (events spread over the last 7 days). If zero, a fixed date is used
	// so that the output stays the same between runs.
	Now time.Time
}

func (c Config) withDefaults() Config {
	if c.Users <= 0 {
		c.Users = 20
	}
	if c.Authors <= 0 {
		c.Authors = 12
	}
	if c.PostsPerAuthor <= 0 {
		c.PostsPerAuthor = 15
	}
	if c.InteractionsPerUser <= 0 {
		c.InteractionsPerUser = 40
	}
	if c.Now.IsZero() {
		c.Now = time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	}
	return c
}

// Author is a generated account that writes on a topic.
type Author struct{ ID, Topic string }

// Post is a generated post.
type Post struct {
	ID, AuthorID, Topic, Text string
	Entities                  []string
	CreatedAt                 time.Time
	// Engagement counters. Authors have a stable quality level, so an author's average
	// engagement (the booster's "author trust") differs between authors.
	Likes, Reblogs, Replies int64
}

// User is a generated user with known topic preferences.
type User struct {
	ID     string
	Topics []string // 1-2 preferred topics
}

// Dataset is the entire output of generation.
type Dataset struct {
	Users        []User
	Authors      []Author
	Posts        []Post
	Interactions []interactions.RawInteraction // per user in ascending time order
	F2           map[string]map[string]float64 // user → author → mutual count
}

// Generate produces a deterministic dataset according to cfg.
func Generate(cfg Config) Dataset {
	cfg = cfg.withDefaults()
	rng := rand.New(rand.NewSource(cfg.Seed))
	ds := Dataset{F2: map[string]map[string]float64{}}

	for i := 0; i < cfg.Authors; i++ {
		ds.Authors = append(ds.Authors, Author{ID: fmt.Sprintf("a%02d", i+1), Topic: Topics[i%len(Topics)].Name})
	}
	topicByName := map[string]Topic{}
	for _, t := range Topics {
		topicByName[t.Name] = t
	}

	// Engagement uses its own stream so that adding it does not shift the rest of the dataset.
	engRng := rand.New(rand.NewSource(cfg.Seed + 1))
	postsByTopic := map[string][]Post{}
	n := 0
	for _, a := range ds.Authors {
		quality := 1 + engRng.Intn(5) // 1 (quiet author) .. 5 (popular author)
		t := topicByName[a.Topic]
		for j := 0; j < cfg.PostsPerAuthor; j++ {
			n++
			ents := pickDistinct(rng, t.Entities, 1+rng.Intn(2))
			w := pickDistinct(rng, t.Words, 3)
			text := fmt.Sprintf("%s hakkında %s ve %s: %s üzerine kısa bir not.", joinTR(ents), w[0], w[1], w[2])
			p := Post{
				ID: fmt.Sprintf("p%04d", n), AuthorID: a.ID, Topic: a.Topic, Text: text, Entities: ents,
				CreatedAt: cfg.Now.Add(-time.Duration(rng.Intn(7*24*60)) * time.Minute),
				Likes:     int64(quality * (2 + engRng.Intn(9))),
				Reblogs:   int64(quality * engRng.Intn(3)),
				Replies:   int64(engRng.Intn(quality + 1)),
			}
			ds.Posts = append(ds.Posts, p)
			postsByTopic[a.Topic] = append(postsByTopic[a.Topic], p)
		}
	}

	for i := 0; i < cfg.Users; i++ {
		u := User{ID: fmt.Sprintf("u%03d", i+1)}
		k := 1 + rng.Intn(2)
		for _, idx := range rng.Perm(len(Topics))[:k] {
			u.Topics = append(u.Topics, Topics[idx].Name)
		}
		sort.Strings(u.Topics)
		ds.Users = append(ds.Users, u)

		pref := map[string]bool{}
		for _, t := range u.Topics {
			pref[t] = true
		}
		var own, other []Post
		for _, p := range ds.Posts {
			if pref[p.Topic] {
				own = append(own, p)
			} else {
				other = append(other, p)
			}
		}

		events := make([]interactions.RawInteraction, 0, cfg.InteractionsPerUser)
		for e := 0; e < cfg.InteractionsPerUser; e++ {
			var it interactions.RawInteraction
			if rng.Float64() < 0.75 && len(own) > 0 {
				p := own[rng.Intn(len(own))]
				it = interactions.RawInteraction{UserID: u.ID, PostID: p.ID, AuthorID: p.AuthorID, InteractionType: pickPositive(rng)}
			} else if len(other) > 0 {
				p := other[rng.Intn(len(other))]
				if rng.Float64() < 0.7 {
					it = interactions.RawInteraction{UserID: u.ID, PostID: p.ID, AuthorID: p.AuthorID, InteractionType: interactions.InteractionSkip, DwellMs: int64(200 + rng.Intn(1300))}
				} else {
					it = interactions.RawInteraction{UserID: u.ID, PostID: p.ID, AuthorID: p.AuthorID, InteractionType: interactions.InteractionLike}
				}
			} else {
				continue
			}
			it.Timestamp = cfg.Now.Add(-time.Duration(rng.Intn(7*24*60)) * time.Minute)
			events = append(events, it)
		}
		sort.SliceStable(events, func(a, b int) bool { return events[a].Timestamp.Before(events[b].Timestamp) })
		ds.Interactions = append(ds.Interactions, events...)

		// F2: a few of the authors on the preferred topics, with 1-5 mutual connections.
		f2 := map[string]float64{}
		for _, a := range ds.Authors {
			if pref[a.Topic] && rng.Float64() < 0.5 {
				f2[a.ID] = float64(1 + rng.Intn(5))
			}
		}
		if len(f2) > 0 {
			ds.F2[u.ID] = f2
		}
	}
	return ds
}

func pickPositive(rng *rand.Rand) string {
	switch r := rng.Float64(); {
	case r < 0.60:
		return interactions.InteractionLike
	case r < 0.75:
		return interactions.InteractionReblog
	case r < 0.90:
		return interactions.InteractionDetailView
	default:
		return interactions.InteractionReply
	}
}

func pickDistinct(rng *rand.Rand, from []string, n int) []string {
	if n > len(from) {
		n = len(from)
	}
	out := make([]string, n)
	for i, idx := range rng.Perm(len(from))[:n] {
		out[i] = from[idx]
	}
	return out
}

func joinTR(items []string) string {
	if len(items) == 1 {
		return items[0]
	}
	return items[0] + " ve " + items[1]
}
