package postmeta

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// PostData is the content summary read from the post HASH.
type PostData struct {
	AuthorID string

	Text      string
	ImageURLs []string
	VideoURLs []string

	Likes        int64
	Reblogs      int64
	Replies      int64
	PenaltyScore int64
	CreatedAt    time.Time

	Category string
}

const (
	fieldMedia        = "media"
	fieldLikes        = "likes"
	fieldReblogs      = "reblogs"
	fieldReplies      = "replies"
	fieldPenaltyScore = "penalty_score"
)

// mediaItem is a single item of the JSON list in the `media` field (raw path).
type mediaItem struct {
	Type   string `json:"type"`
	RawURL string `json:"raw_url"`
}

// Hydrate reads the post_id → PostData mapping in a single pipeline. A post without a HASH
// does not appear in the result. No external content lookup is done here;
// media is returned as a raw path.
func (s *Store) Hydrate(ctx context.Context, postIDs []string) (map[string]PostData, error) {
	out := make(map[string]PostData, len(postIDs))
	if len(postIDs) == 0 {
		return out, nil
	}
	pipe := s.rdb.Pipeline()
	cmds := make([]*goredis.MapStringStringCmd, len(postIDs))
	for i, id := range postIDs {
		cmds[i] = pipe.HGetAll(ctx, s.key(id))
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, wrapErr("Hydrate", "", err)
	}
	for i, cmd := range cmds {
		fields, err := cmd.Result()
		if err != nil || len(fields) == 0 {
			continue
		}
		out[postIDs[i]] = decodeHash(fields)
	}
	return out, nil
}

func decodeHash(f map[string]string) PostData {
	pd := PostData{
		AuthorID: f[fieldAuthorID],
		Text:     f[fieldText],
		Category: f[fieldCategory],
	}
	pd.Likes, _ = strconv.ParseInt(f[fieldLikes], 10, 64)
	pd.Reblogs, _ = strconv.ParseInt(f[fieldReblogs], 10, 64)
	pd.Replies, _ = strconv.ParseInt(f[fieldReplies], 10, 64)
	pd.PenaltyScore, _ = strconv.ParseInt(f[fieldPenaltyScore], 10, 64)
	if t, err := time.Parse(time.RFC3339, f[fieldCreatedAt]); err == nil {
		pd.CreatedAt = t
	}
	var media []mediaItem
	if raw := f[fieldMedia]; raw != "" && json.Unmarshal([]byte(raw), &media) == nil {
		for _, m := range media {
			switch m.Type {
			case "image":
				pd.ImageURLs = append(pd.ImageURLs, m.RawURL)
			case "video":
				pd.VideoURLs = append(pd.VideoURLs, m.RawURL)
			}
		}
	}
	return pd
}
