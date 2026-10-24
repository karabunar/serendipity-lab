package m04interest

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// weightDecimals is the decimal precision applied to weights before writing.
// It shrinks the Redis footprint by ~35%, see roundWeights.
const weightDecimals = 4

// ProfileStore is the storage contract the builder needs (DIP).
type ProfileStore interface {
	Get(ctx context.Context, userID string) (Profile, error)
	Save(ctx context.Context, userID string, p Profile) error
}

// GraphSource provides graph-prior signals. It is optional: if WithGraph
// is not called, the builder never asks for it.
//
// The `since` contract is THE SAME AS FOR POSITIVES and is mandatory: the source must only
// return neighbor content AFTER that moment. Otherwise the same neighbor post
// is re-added at full weight on every run; after N runs the graph contribution
// overrides the user's own behavior and the "weak prior" guarantee collapses.
type GraphSource interface {
	Prior(ctx context.Context, userID string, since time.Time) ([]Signal, error)
}

// BuildConfig holds the profile build parameters.
type BuildConfig struct {
	// StableHalfLife is the half-life of lasting interest (fandom, profession —
	// changes on a scale of months).
	StableHalfLife time.Duration
	// RecentHalfLife is the half-life of current interest (the week's agenda).
	RecentHalfLife time.Duration
	// MaxEntities is the maximum number of entities kept in each map (budget guard).
	MaxEntities int
	// QueryTopK is the length of the queryable list.
	QueryTopK int
	// MinQueryWeight is the minimum weight required to enter the query list.
	MinQueryWeight float64
	// SinceGrace is a safety margin that shifts the source gate BACKWARD
	// (USER_INTEREST_SINCE_GRACE). 0 = today's behavior.
	//
	// Trade-off: the Timestamp in the ring buffer is the EVENT time, not the ingest time
	// (a backfill re-stamps
	// historical timestamps). An interaction whose event time is BEFORE the last profile
	// write but which lands in the buffer AFTER it — not theoretical
	// (a large message backlog delays ingestion) —
	// is lost PERMANENTLY with a 0 grace, with no second chance.
	// With a >0 grace it is caught, but interactions inside the window may be
	// counted TWICE. There is no free fix: the ring ZSET is scored by event time,
	// so insertion order cannot be recovered.
	SinceGrace time.Duration
}

// DefaultBuildConfig holds the initial values from the design. Decay
// constants are SUBJECT TO CALIBRATION; thresholds deliberately start out inert.
func DefaultBuildConfig() BuildConfig {
	return BuildConfig{
		StableHalfLife: 90 * 24 * time.Hour,
		RecentHalfLife: 7 * 24 * time.Hour,
		MaxEntities:    50,
		QueryTopK:      10,
		MinQueryWeight: 0,
	}
}

// Builder builds the profile of a single user.
type Builder struct {
	pos    PositiveSource
	neg    NegativeSource
	store  ProfileStore
	cfg    BuildConfig
	logger *slog.Logger

	graph       GraphSource
	graphWeight float64
}

// NewBuilder fills zero/negative config fields with defaults.
func NewBuilder(pos PositiveSource, neg NegativeSource, store ProfileStore, cfg BuildConfig, logger *slog.Logger) *Builder {
	d := DefaultBuildConfig()
	if cfg.StableHalfLife <= 0 {
		cfg.StableHalfLife = d.StableHalfLife
	}
	if cfg.RecentHalfLife <= 0 {
		cfg.RecentHalfLife = d.RecentHalfLife
	}
	if cfg.MaxEntities <= 0 {
		cfg.MaxEntities = d.MaxEntities
	}
	if cfg.QueryTopK <= 0 {
		cfg.QueryTopK = d.QueryTopK
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Builder{pos: pos, neg: neg, store: store, cfg: cfg, logger: logger}
}

// WithGraph enables the graph prior. weight must be WEAK (design
// §4.2.5: the user's own behavior always comes before the neighbor's). If weight<=0 the graph
// is not wired — same effect as leaving the flag off.
func (b *Builder) WithGraph(g GraphSource, weight float64) *Builder {
	if g == nil || weight <= 0 {
		return b
	}
	b.graph = g
	b.graphWeight = weight
	return b
}

// BuildUser updates the user's profile in a ROLLING manner: the old profile
// is read → weights are decayed → only interactions AFTER `UpdatedAt`
// are added. It never builds from scratch.
//
// Error policy:
//   - Positive source error → the profile is NOT WRITTEN and an error is returned. Decaying
//     and rewriting the old profile would make data loss look like
//     "interest declined".
//   - Negative source error → negatives stay empty and we CONTINUE
//     with positives. Being unable to read the mirror must not block the profile.
func (b *Builder) BuildUser(ctx context.Context, userID string, now time.Time) (Profile, error) {
	prev, err := b.store.Get(ctx, userID)
	if err != nil {
		ProfilesBuilt.WithLabelValues("error").Inc()
		return Profile{}, fmt.Errorf("could not read interest profile %q: %w", userID, err)
	}

	// zero → everything in the buffer is processed (first run). SinceGrace shifts the gate
	// backward: it catches late-arriving interactions, at the cost of
	// a double-counting risk inside the window (see BuildConfig.SinceGrace).
	since := prev.UpdatedAt
	if !since.IsZero() && b.cfg.SinceGrace > 0 {
		since = since.Add(-b.cfg.SinceGrace)
	}

	positives, err := b.pos.Positives(ctx, userID, since)
	if err != nil {
		ProfilesBuilt.WithLabelValues("error").Inc()
		return Profile{}, fmt.Errorf("could not read positive signals %q: %w", userID, err)
	}

	negatives, nErr := b.neg.Negatives(ctx, userID, since)
	if nErr != nil {
		b.logger.Warn("could not read negative signals — continuing with positives",
			slog.String("user_id", userID),
			slog.String("error", nErr.Error()))
		negatives = nil
	}

	SignalsPerRun.WithLabelValues("positive").Add(float64(len(positives)))
	SignalsPerRun.WithLabelValues("negative").Add(float64(len(negatives)))

	// For a user with no signals of their own, do NOT write — but only if the profile
	// ALREADY EXISTS. Since decay is applied at write time rather than read time,
	// rewriting on an empty run would erode the weights and extend the TTL;
	// a dormant profile would then never expire (human decision).
	//
	// The `!prev.IsZero()` condition preserves the graph's reason for existing (design
	// §4.2.5: "the real value is with low-interaction users"). When the gate was unconditional,
	// a user with ZERO interactions — i.e. the stated
	// beneficiary — never reached the graph block; the graph could only add
	// on top of a user who already had their own signal. Now on a cold start there is one
	// graph-only write; after that write prev is non-zero, so
	// later empty runs are skipped again. The cost is one write per TTL
	// period, not per run.
	if len(positives) == 0 && len(negatives) == 0 && !prev.IsZero() {
		ProfilesBuilt.WithLabelValues("skipped").Inc()
		return prev, nil
	}

	// `since` is THE SAME threshold as for positives: if the same neighbor post is added twice,
	// the graph contribution accumulates per run and the "weak prior"
	// guarantee collapses.
	//
	// An error DROPS the prior but does not drop the profile — its value is auxiliary.
	var graphSignals []Signal
	if b.graph != nil {
		gs, gErr := b.graph.Prior(ctx, userID, since)
		if gErr != nil {
			b.logger.Warn("could not read graph prior — continuing without it",
				slog.String("user_id", userID),
				slog.String("error", gErr.Error()))
		} else {
			graphSignals = make([]Signal, 0, len(gs))
			for _, s := range gs {
				s.Value *= b.graphWeight
				graphSignals = append(graphSignals, s)
			}
		}
	}

	// On a cold start, if the graph is also empty there is NOTHING to write. Writing an empty
	// profile would be pointless: for 31k dormant users it would cost a key
	// + 30-day TTL, with zero information in return.
	if len(positives) == 0 && len(negatives) == 0 && len(graphSignals) == 0 {
		ProfilesBuilt.WithLabelValues("skipped").Inc()
		return prev, nil
	}

	base := prev
	if base.IsZero() {
		base = NewProfile()
		base.UpdatedAt = now // no history to decay on the first run
	}
	// Clock skew between pods can move `now` BACKWARD. DecayWeights already
	// clamps negative elapsed to no-decay, but if out.UpdatedAt is
	// written backward, the next run's `since` also moves back and the
	// interactions in between are added AGAIN.
	if now.Before(base.UpdatedAt) {
		now = base.UpdatedAt
	}
	elapsed := now.Sub(base.UpdatedAt)

	stableL := Lambda(b.cfg.StableHalfLife)
	recentL := Lambda(b.cfg.RecentHalfLife)

	out := NewProfile()
	out.UpdatedAt = now
	out.Summary = base.Summary // Decision 1: not produced in this phase, preserved

	stableBase := AddSignals(DecayWeights(base.Stable, stableL, elapsed), positives, stableL, now)
	recentBase := AddSignals(DecayWeights(base.Recent, recentL, elapsed), positives, recentL, now)
	if len(graphSignals) > 0 {
		stableBase = AddSignals(stableBase, graphSignals, stableL, now)
		recentBase = AddSignals(recentBase, graphSignals, recentL, now)
	}
	// Rounding comes AFTER Prune and BEFORE Query/Confidence: so the stored and
	// in-memory profiles are identical (rounding in MarshalJSON would
	// create a silent asymmetry — the caller uses the returned value).
	out.Stable = roundWeights(Prune(stableBase, b.cfg.MaxEntities), weightDecimals)
	out.Recent = roundWeights(Prune(recentBase, b.cfg.MaxEntities), weightDecimals)
	// Negatives are decayed with the PERMANENT constant: "I dislike this topic"
	// is not a weekly agenda item.
	out.Negative = roundWeights(
		Prune(AddSignals(DecayWeights(base.Negative, stableL, elapsed), negatives, stableL, now), b.cfg.MaxEntities),
		weightDecimals)

	out.Query = TopK(out.Stable, b.cfg.QueryTopK, b.cfg.MinQueryWeight)
	out.Confidence = ComputeConfidence(base.Confidence, len(positives), out.Stable)

	if err := b.store.Save(ctx, userID, out); err != nil {
		ProfilesBuilt.WithLabelValues("error").Inc()
		return Profile{}, fmt.Errorf("could not write interest profile %q: %w", userID, err)
	}

	ProfilesBuilt.WithLabelValues("built").Inc()
	EntitiesPerProfile.Observe(float64(len(out.Stable)))
	return out, nil
}
