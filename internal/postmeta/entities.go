package postmeta

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	goredis "github.com/redis/go-redis/v9"
)

// untaggedChunk is GetUntagged's pipeline chunk size. Queueing the WHOLE
// keyspace in a single pipeline would mean unbounded memory on a large keyspace
// and a huge Redis command stack at once. Precedent: chunking by 500.
const untaggedChunk = 1000

// Coverage is the scan snapshot of GetUntagged — the numbers that measure the design's "entity
// coverage" gate. Untagged + Tagged + NoText = Total.
type Coverage struct {
	Total    int // number of posts asked about
	Tagged   int // entities field is populated
	Untagged int // no entities, text populated → to be processed
	NoText   int // text empty or the post does not exist at all → can never be tagged
}

// GetUntagged returns, from the given posts, those that do NOT have an `entities` field and whose text is NON-EMPTY
// as post_id → text; alongside it gives the coverage counts.
//
// maxTexts is the ceiling of the RETURNED MAP (<=0 → unlimited). The coverage counts are NOT
// AFFECTED by this ceiling: the gauges must see the whole keyspace, otherwise
// the question "is coverage approaching zero" cannot be measured.
//
// # Why two passes
//
// A single pass did `HMGET text entities`, i.e. it also pulled the bodies of
// ALREADY TAGGED posts. After the backfill finishes, the permanent majority is exactly
// those; carrying all the texts needlessly every 15 minutes is a permanent cost
// against a memory-limited Redis. Now:
//
//  1. Pass 1 — only `entities` (a small field) for ALL posts: yields Total,
//     Tagged and the "candidate" (untagged) set.
//  2. Pass 2 — `text` only for candidates. After the ceiling is filled, for the remaining
//     candidates `HSTRLEN text` is used instead of `HMGET text`: the text is
//     NOT CARRIED but the empty/non-empty distinction (Untagged vs NoText) is still made exactly.
//     Applying the ceiling as "don't ask about the remaining candidates" would count textless (media-only)
//     posts as Untagged forever — the pending gauge
//     would never drop to zero and the §9 gate would lose its meaning.
//
// Not reprocessing a tagged post is the contract of this function:
// even if extraction yields an empty result the caller writes `{"entities":[],"topics":[]}`,
// so the field exists and the post is never selected again.
func (s *Store) GetUntagged(ctx context.Context, postIDs []string, maxTexts int) (map[string]string, Coverage, error) {
	out := map[string]string{}
	var cov Coverage
	if len(postIDs) == 0 {
		return out, cov, nil
	}

	candidates, cov, err := s.scanTagged(ctx, postIDs)
	if err != nil {
		return nil, Coverage{}, err
	}
	if err := s.fillTexts(ctx, candidates, maxTexts, out, &cov); err != nil {
		return nil, Coverage{}, err
	}

	s.log().Debug("postdata GetUntagged",
		slog.Int("total", cov.Total),
		slog.Int("tagged", cov.Tagged),
		slog.Int("untagged", cov.Untagged),
		slog.Int("no_text", cov.NoText),
		slog.Int("texts_returned", len(out)),
	)
	return out, cov, nil
}

// scanTagged, pass 1: reads only the `entities` field. Fills Total/Tagged
// and returns the untagged candidates PRESERVING ORDER.
func (s *Store) scanTagged(ctx context.Context, postIDs []string) ([]string, Coverage, error) {
	var cov Coverage
	candidates := make([]string, 0, len(postIDs))

	for start := 0; start < len(postIDs); start += untaggedChunk {
		end := min(start+untaggedChunk, len(postIDs))
		chunk := postIDs[start:end]

		pipe := s.rdb.Pipeline()
		cmds := make([]*goredis.SliceCmd, len(chunk))
		for i, id := range chunk {
			cmds[i] = pipe.HMGet(ctx, s.key(id), fieldEntities)
		}
		if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, goredis.Nil) {
			return nil, Coverage{}, wrapErr("GetUntagged", "", err)
		}

		for i, cmd := range cmds {
			cov.Total++
			vals, err := cmd.Result()
			if err != nil || len(vals) < 1 {
				// Key could not be read → the text is also treated as absent.
				cov.NoText++
				continue
			}
			if ents, _ := vals[0].(string); ents != "" {
				cov.Tagged++
				continue
			}
			candidates = append(candidates, chunk[i])
		}
	}
	return candidates, cov, nil
}

// fillTexts, pass 2: fetches the candidates' text and makes the Untagged/NoText
// distinction. After the ceiling is filled the text is NOT CARRIED, only the length is asked.
func (s *Store) fillTexts(ctx context.Context, candidates []string, maxTexts int, out map[string]string, cov *Coverage) error {
	for start := 0; start < len(candidates); start += untaggedChunk {
		end := min(start+untaggedChunk, len(candidates))
		chunk := candidates[start:end]

		// If the ceiling is filled, only the length is enough for classification from now on.
		lengthOnly := maxTexts > 0 && len(out) >= maxTexts

		pipe := s.rdb.Pipeline()
		textCmds := make([]*goredis.SliceCmd, 0, len(chunk))
		lenCmds := make([]*goredis.IntCmd, 0, len(chunk))
		for _, id := range chunk {
			if lengthOnly {
				lenCmds = append(lenCmds, pipe.HStrLen(ctx, s.key(id), fieldText))
			} else {
				textCmds = append(textCmds, pipe.HMGet(ctx, s.key(id), fieldText))
			}
		}
		if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, goredis.Nil) {
			return wrapErr("GetUntagged", "", err)
		}

		if lengthOnly {
			for _, cmd := range lenCmds {
				if n, err := cmd.Result(); err != nil || n == 0 {
					cov.NoText++
				} else {
					cov.Untagged++
				}
			}
			continue
		}

		for i, cmd := range textCmds {
			vals, err := cmd.Result()
			if err != nil || len(vals) < 1 {
				cov.NoText++
				continue
			}
			text, _ := vals[0].(string)
			if text == "" {
				cov.NoText++
				continue
			}
			cov.Untagged++
			// The ceiling can fill in the MIDDLE of a chunk: counting continues, the map
			// does not grow.
			if maxTexts <= 0 || len(out) < maxTexts {
				out[chunk[i]] = text
			}
		}
	}
	return nil
}

// setFieldIfExistsScript writes the given field only if the HASH ALREADY EXISTS and
// refreshes the TTL; if the key does not exist it does nothing (return 0).
//
// ⚠️ The IDENTITY gate that SetCategoryBatch passes through
// (setCategoryIfIndexedScript) is DELIBERATELY not used here: the defect that gate closed
// was the clustering round writing to textless shells and refreshing their TTLs.
// The entity scanner only writes GetUntagged's posts WITH TEXT —
// shells are never selected — and a post without created_at also getting entities
// is the contract (TestEntityScanOnce_CreatedAtYoksaIndekseGirmez).
const setFieldIfExistsScript = `
if redis.call('EXISTS', KEYS[1]) == 1 then
  redis.call('HSET', KEYS[1], ARGV[1], ARGV[2])
  redis.call('EXPIRE', KEYS[1], ARGV[3])
  return 1
end
return 0
`

// SetEntitiesBatch writes the post_id → entities JSON map. It uses an EXISTS gate
// (setFieldIfExistsScript): if the key does not exist nothing is done,
// so a contentless "thin hash" is not created for posts that are not ours.
// Other fields are not touched.
func (s *Store) SetEntitiesBatch(ctx context.Context, postIDToJSON map[string]string) error {
	if len(postIDToJSON) == 0 {
		return nil
	}
	ttlSec := int(s.ttl.Seconds())
	pipe := s.rdb.Pipeline()
	cmds := make([]*goredis.Cmd, 0, len(postIDToJSON))
	requested := 0
	for postID, payload := range postIDToJSON {
		if postID == "" || payload == "" {
			continue
		}
		requested++
		cmds = append(cmds, pipe.Eval(ctx, setFieldIfExistsScript, []string{s.key(postID)}, fieldEntities, payload, ttlSec))
	}
	if requested == 0 {
		return nil
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return wrapErr("SetEntitiesBatch", "", err)
	}
	written := 0
	for _, c := range cmds {
		if v, evErr := c.Int(); evErr == nil && v == 1 {
			written++
		}
	}
	s.log().Debug("postdata SetEntitiesBatch",
		slog.Int("requested", requested),
		slog.Int("written", written),
		slog.Int("skipped_absent", requested-written),
	)
	return nil
}

// entitiesPayload is the read side of the JSON in the `entities` field. The same schema as Tags
// in the postentity package.
//
// Why a copy: NOT an import CYCLE — postentity does not depend on
// postmeta, so `postmeta → postentity` was technically possible. The reason for the copy
// is layer direction: postmeta is a generic HASH accessor and must not carry a dependency
// on the cluster labeler's entity extraction feature (the locallm, canonicaldict, trfold chain) —
// that chain would spread to EVERY
// consumer that imports postmeta.
//
// ⚠️ The cost is schema drift: if a field is added to postentity.Tags and the read side
// also needs that field, this place will SILENTLY read incompletely. As long as both sides only use
// the `entities` field it is safe; if that changes, the two files
// must be updated together.
type entitiesPayload struct {
	Entities []string `json:"entities"`
}

// GetEntitiesBatch returns the canonical entity names of the given posts.
//
// `topics` is DELIBERATELY NOT included: the Fenerbahce/Besiktas distinction is carried by proper
// names, a topic label falls back to the "football" resolution (design
// §4.1). Adding topic labels to the profile is a separate decision.
//
// ONLY posts with at least one entity enter the result. The difference between "no entities"
// and "post unknown" is meaningless to the caller — in both
// the contribution is zero — and making them distinguishable would force every caller to
// write an empty-list check.
//
// A corrupt JSON drops a single post, not the batch (fail-soft).
func (s *Store) GetEntitiesBatch(ctx context.Context, postIDs []string) (map[string][]string, error) {
	out := make(map[string][]string, len(postIDs))
	if len(postIDs) == 0 {
		return out, nil
	}

	malformed := 0
	for start := 0; start < len(postIDs); start += untaggedChunk {
		end := min(start+untaggedChunk, len(postIDs))
		chunk := postIDs[start:end]

		pipe := s.rdb.Pipeline()
		cmds := make([]*goredis.StringCmd, len(chunk))
		for i, id := range chunk {
			cmds[i] = pipe.HGet(ctx, s.key(id), fieldEntities)
		}
		if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, goredis.Nil) {
			return nil, wrapErr("GetEntitiesBatch", "", err)
		}

		for i, cmd := range cmds {
			raw, err := cmd.Result()
			if err != nil || raw == "" {
				continue // no field — normal
			}
			var p entitiesPayload
			if uErr := json.Unmarshal([]byte(raw), &p); uErr != nil {
				malformed++
				continue
			}
			if len(p.Entities) == 0 {
				continue
			}
			out[chunk[i]] = p.Entities
		}
	}

	if malformed > 0 {
		s.log().Warn("postdata GetEntitiesBatch: corrupt entities field skipped",
			slog.Int("malformed", malformed),
			slog.Int("requested", len(postIDs)),
		)
	}
	s.log().Debug("postdata GetEntitiesBatch",
		slog.Int("requested", len(postIDs)),
		slog.Int("with_entities", len(out)),
	)
	return out, nil
}

// GetTextBatch reads the raw text needed by the rescan/backfill tool
// independently of the entities field. Empty texts do not enter the result; the distinction between a nonexistent
// HASH and a textless post is unimportant to this consumer.
func (s *Store) GetTextBatch(ctx context.Context, postIDs []string) (map[string]string, error) {
	out := make(map[string]string, len(postIDs))
	if len(postIDs) == 0 {
		return out, nil
	}
	for start := 0; start < len(postIDs); start += untaggedChunk {
		end := min(start+untaggedChunk, len(postIDs))
		chunk := postIDs[start:end]
		pipe := s.rdb.Pipeline()
		cmds := make([]*goredis.StringCmd, len(chunk))
		for i, id := range chunk {
			cmds[i] = pipe.HGet(ctx, s.key(id), fieldText)
		}
		if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, goredis.Nil) {
			return nil, wrapErr("GetTextBatch", "", err)
		}
		for i, cmd := range cmds {
			text, err := cmd.Result()
			if err == nil && text != "" {
				out[chunk[i]] = text
			}
		}
	}
	return out, nil
}

// EntityMeta is the two fields the entity discovery channel needs per candidate.
//
// Why a separate method: ResolveAuthors + GetEntitiesBatch would open TWO pipelines
// for the same post set. The channel's whole budget is two Redis round trips
// (index read + meta read); a third needlessly eats into the SLA.
type EntityMeta struct {
	AuthorID string
	Entities []string
}

// GetEntityMeta returns the author_id and canonical entity names of the given posts in a SINGLE
// HMGET pipeline.
//
// EVERY post whose HASH exists enters the result — even without entities. The caller
// needs author_id for the author quota and self-exclusion; an empty entity
// list is harmless in the veto step. This DELIBERATELY differs from GetEntitiesBatch's "only
// posts with entities" contract.
//
// A corrupt `entities` JSON only empties the entity list, it does not drop the post
// (fail-soft).
//
// ⚠️ Exception: if BOTH `author_id` and `entities` fields are missing the post
// is indistinguishable and drops from the result — the post consumer's index_only ingest path
// never writes that field when author_id is empty (a post awaiting author-backfill).
// Telling them apart would need a third round trip (EXISTS); this channel's budget
// is two round trips, and a post without author_id is already unusable to the caller
// (author quota + self-exclusion depend on that field).
func (s *Store) GetEntityMeta(ctx context.Context, postIDs []string) (map[string]EntityMeta, error) {
	out := make(map[string]EntityMeta, len(postIDs))
	if len(postIDs) == 0 {
		return out, nil
	}

	malformed := 0
	for start := 0; start < len(postIDs); start += untaggedChunk {
		end := min(start+untaggedChunk, len(postIDs))
		chunk := postIDs[start:end]

		pipe := s.rdb.Pipeline()
		cmds := make([]*goredis.SliceCmd, len(chunk))
		for i, id := range chunk {
			cmds[i] = pipe.HMGet(ctx, s.key(id), fieldAuthorID, fieldEntities)
		}
		if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, goredis.Nil) {
			return nil, wrapErr("GetEntityMeta", "", err)
		}

		for i, cmd := range cmds {
			vals, err := cmd.Result()
			if err != nil || len(vals) != 2 {
				continue
			}
			author, _ := vals[0].(string)
			rawEntities, _ := vals[1].(string)
			if author == "" && rawEntities == "" {
				continue // post does not exist
			}

			meta := EntityMeta{AuthorID: author}
			if rawEntities != "" {
				var p entitiesPayload
				if uErr := json.Unmarshal([]byte(rawEntities), &p); uErr != nil {
					malformed++
				} else {
					meta.Entities = p.Entities
				}
			}
			out[chunk[i]] = meta
		}
	}

	if malformed > 0 {
		s.log().Warn("postdata GetEntityMeta: corrupt entities field skipped",
			slog.Int("malformed", malformed),
			slog.Int("requested", len(postIDs)),
		)
	}
	return out, nil
}
