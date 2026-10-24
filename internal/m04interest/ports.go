package m04interest

import (
	"context"
	"time"
)

// The interfaces in this file are defined on the CONSUMER side (Go idiom): concrete
// implementations live under m04interest/interestsrc and this package
// carries NO import dependency on them. Tests run with fake implementations.

// PositiveSource turns the user's positive interactions AFTER `since`
// into entity signals.
//
// # BOTH SIDES of the `since` gate are risky
//
// Too wide: if the same interaction is added twice, the weight inflates on every run.
// Too narrow: the Timestamp in the ring buffer is the EVENT time, not the ingest time
// — an interaction whose event time is BEFORE the last profile write but which lands
// in the buffer AFTER it is lost PERMANENTLY, with no second chance. This is
// not theoretical (message backlogs and backfill outages can delay ingestion by hours or weeks). The trade-off is
// tuned with BuildConfig.SinceGrace; default 0 = exactly-once, i.e. the loss direction is the chosen one.
type PositiveSource interface {
	Positives(ctx context.Context, userID string, since time.Time) ([]Signal, error)
}

// NegativeSource returns negative signals (skip).
//
// The source is the analytics MIRROR, not the ring buffer: `skip` is defined as
// interactions.InteractionSkip but the engagement consumer does NOT write it to the
// buffer. The mirror does NOT CARRY the `skip` vs `fast_skip` distinction —
// analytics.SkipAnalytics is a single Count/DwellMsSum pair.
//
// The `since` contract is NOT as strict as PositiveSource's: the mirror keeps a cumulative
// daily total, see interestsrc.MirrorNegatives.
type NegativeSource interface {
	Negatives(ctx context.Context, userID string, since time.Time) ([]Signal, error)
}

// EntityResolver maps post → canonical entity names. The concrete impl is
// postmeta.Store.GetEntitiesBatch.
type EntityResolver interface {
	EntitiesOf(ctx context.Context, postIDs []string) (map[string][]string, error)
}
