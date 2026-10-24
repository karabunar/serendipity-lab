// Package interestsrc consists of adapters that feed m04interest's port interfaces
// with the existing Redis readers. The domain package (m04interest)
// does NOT depend on them; the dependency direction is always from the outside in.
package interestsrc

import (
	"context"
	"time"

	"serendipity/internal/interactions"
	"serendipity/internal/m04interest"
)

// RingReader is the only method needed for reading from the ring buffer.
// interactions.RedisInteractionStore already satisfies this signature.
type RingReader interface {
	GetRecent(ctx context.Context, userID string, limit int) ([]interactions.RawInteraction, error)
}

// RingPositives turns the serendipity:user:interactions:{uid} ring buffer into entity
// signals.
type RingPositives struct {
	ring  RingReader
	res   m04interest.EntityResolver
	limit int
}

// NewRingPositives uses 200 for limit<=0.
func NewRingPositives(ring RingReader, res m04interest.EntityResolver, limit int) *RingPositives {
	if limit <= 0 {
		limit = 200
	}
	return &RingPositives{ring: ring, res: res, limit: limit}
}

// Positives returns the positive interactions AFTER `since`.
//
// Skip is not written to the buffer today, but it is
// filtered out defensively: if it starts being written one day, it must not gain POSITIVE weight.
func (p *RingPositives) Positives(ctx context.Context, userID string, since time.Time) ([]m04interest.Signal, error) {
	items, err := p.ring.GetRecent(ctx, userID, p.limit)
	if err != nil {
		return nil, err
	}

	fresh := make([]interactions.RawInteraction, 0, len(items))
	ids := make([]string, 0, len(items))
	for _, it := range items {
		if it.PostID == "" || it.Value <= 0 {
			continue
		}
		if it.InteractionType == interactions.InteractionSkip {
			continue
		}
		if !since.IsZero() && !it.Timestamp.After(since) {
			continue
		}
		fresh = append(fresh, it)
		ids = append(ids, it.PostID)
	}
	if len(fresh) == 0 {
		return nil, nil
	}

	entities, err := p.res.EntitiesOf(ctx, ids)
	if err != nil {
		return nil, err
	}

	out := make([]m04interest.Signal, 0, len(fresh))
	for _, it := range fresh {
		ents, ok := entities[it.PostID]
		if !ok || len(ents) == 0 {
			continue
		}
		out = append(out, m04interest.Signal{Entities: ents, Value: it.Value, At: it.Timestamp})
	}
	return out, nil
}
