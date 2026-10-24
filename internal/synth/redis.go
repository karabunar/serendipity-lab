package synth

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"serendipity/internal/activeusers"
	"serendipity/internal/core"
	"serendipity/internal/entityindex"
	"serendipity/internal/interactionlog"
	"serendipity/internal/postcache"
	"serendipity/internal/postmeta"
)

// F2Key is the key of the user's F2 author set (ZSET, score = mutual connection count).
func F2Key(userID string) string { return "serendipity:user:f2_authors:" + userID }

// Summary is the record counts Write wrote.
type Summary struct {
	Posts, Authors, Users, Interactions, F2Edges int
}

// Write writes the dataset to the Redis keys that the pipelines read:
// post HASHes (text, category, entity), author → latest posts, entity reverse index,
// active-user ZSET, F2 sets and the interaction log.
//
// Timestamps are values produced by ds; since the entity index and interest profile windows work
// against the real clock, Generate must be given Config.Now = time.Now().
func Write(ctx context.Context, rdb *goredis.Client, ds Dataset, logger *slog.Logger) (Summary, error) {
	var s Summary
	pm := postmeta.New(rdb, postmeta.Config{}, logger)

	raws := make([]core.RawPost, len(ds.Posts))
	cats := make(map[string]string, len(ds.Posts))
	ents := make(map[string]string, len(ds.Posts))
	idx := make([]entityindex.Post, len(ds.Posts))
	byAuthor := map[string][]postcache.PostEntry{}
	for i, p := range ds.Posts {
		raws[i] = core.RawPost{PostID: p.ID, AuthorID: p.AuthorID, Text: p.Text, Category: p.Topic, CreatedAt: p.CreatedAt}
		cats[p.ID] = p.Topic
		payload, err := json.Marshal(map[string]any{"entities": p.Entities, "topics": []string{p.Topic}})
		if err != nil {
			return s, err
		}
		ents[p.ID] = string(payload)
		idx[i] = entityindex.Post{PostID: p.ID, Entities: p.Entities, CreatedAt: p.CreatedAt}
		byAuthor[p.AuthorID] = append(byAuthor[p.AuthorID], postcache.PostEntry{PostID: p.ID, CreatedAt: p.CreatedAt})
	}
	if err := pm.SaveBatch(ctx, raws); err != nil {
		return s, fmt.Errorf("synth: posts: %w", err)
	}
	pipe := rdb.Pipeline()
	for _, p := range ds.Posts {
		pipe.HSet(ctx, postmeta.KeyPrefix+p.ID, "likes", p.Likes, "reblogs", p.Reblogs, "replies", p.Replies)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return s, fmt.Errorf("synth: engagement counters: %w", err)
	}
	if err := pm.SetCategoryBatch(ctx, cats); err != nil {
		return s, fmt.Errorf("synth: categories: %w", err)
	}
	if err := pm.SetEntitiesBatch(ctx, ents); err != nil {
		return s, fmt.Errorf("synth: entities: %w", err)
	}
	if err := entityindex.NewWriter(rdb, 0, 0, logger).Add(ctx, idx); err != nil {
		return s, fmt.Errorf("synth: entity index: %w", err)
	}
	cache := postcache.NewLatestPostsCache(rdb, 0, logger)
	for author, entries := range byAuthor {
		if err := cache.RefreshAuthor(ctx, author, entries); err != nil {
			return s, fmt.Errorf("synth: author posts: %w", err)
		}
	}
	s.Posts, s.Authors = len(ds.Posts), len(byAuthor)

	last := map[string]time.Time{}
	if err := interactionlog.New(rdb, 0, 0).RecordBatch(ctx, ds.Interactions); err != nil {
		return s, fmt.Errorf("synth: interactions: %w", err)
	}
	for _, it := range ds.Interactions {
		if it.Timestamp.After(last[it.UserID]) {
			last[it.UserID] = it.Timestamp
		}
	}
	s.Interactions = len(ds.Interactions)

	for _, u := range ds.Users {
		score := float64(time.Now().Unix())
		if t, ok := last[u.ID]; ok {
			score = float64(t.Unix())
		}
		if err := rdb.ZAdd(ctx, activeusers.DefaultKey, goredis.Z{Score: score, Member: u.ID}).Err(); err != nil {
			return s, fmt.Errorf("synth: active users: %w", err)
		}
		for author, mutual := range ds.F2[u.ID] {
			if err := rdb.ZAdd(ctx, F2Key(u.ID), goredis.Z{Score: mutual, Member: author}).Err(); err != nil {
				return s, fmt.Errorf("synth: F2: %w", err)
			}
			s.F2Edges++
		}
	}
	s.Users = len(ds.Users)
	return s, nil
}
