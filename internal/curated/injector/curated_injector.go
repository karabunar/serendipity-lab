package injector

import (
	"context"
	"log/slog"
	"sort"

	"serendipity/internal/curated/model"
)

// DefaultCuratedPositions are the fixed injection positions (1-indexed, final order
// in the chain output). Take care not to target the same position as another injector:
// a second injector targeting the same slot silently overwrites the first one's placement.
var DefaultCuratedPositions = []int{7}

// CuratedInjector is the stage that places the candidates in ProcessContext.CuratedCandidates
// (PRE-scored by the CronJob with author_trust×content_signal) at fixed positions, and
// it must be the LAST stage of the PostProcessorChain. It is a pure function and does no I/O;
// the curated fetch finishes BEFORE the chain (content.Fetcher).
type CuratedInjector struct {
	positions []int
	logger    *slog.Logger
}

// NewCuratedInjector creates an injector with the given positions. If positions is
// empty, DefaultCuratedPositions is used. Positions are sorted ascending.
func NewCuratedInjector(positions []int, logger *slog.Logger) *CuratedInjector {
	if len(positions) == 0 {
		positions = DefaultCuratedPositions
	}
	sorted := append([]int(nil), positions...)
	sort.Ints(sorted)
	return &CuratedInjector{positions: sorted, logger: logger}
}

// Name returns the stage name (for the PostProcessorChain debug log).
func (c *CuratedInjector) Name() string { return "curated_injector" }

// OwnsFinalOrder is the IPostProcessor contract: does this stage own the final order?
// Yes: curated content is placed at FIXED positions, so the order it builds must not be overwritten by the ReRanker.
func (c *CuratedInjector) OwnsFinalOrder() bool { return true }

// Apply inserts the first N candidates (N = number of positions) from pc.CuratedCandidates
// that are NOT in pc.CuratedSeenPostIDs, in order, at the target positions.
// If len(out) < position, that slot is skipped (fail-soft). If the pool is empty or all of it
// is seen, it is a no-op.
func (c *CuratedInjector) Apply(_ context.Context, scored []model.ScoredCandidate, pc model.ProcessContext) []model.ScoredCandidate {
	if len(pc.CuratedCandidates) == 0 || len(c.positions) == 0 {
		CuratedInjectionOutcome.WithLabelValues("skipped_empty_pool").Inc()
		return scored
	}

	winners := make([]model.ScoredCandidate, 0, len(c.positions))
	for _, cand := range pc.CuratedCandidates {
		if len(winners) >= len(c.positions) {
			break
		}
		if pc.CuratedSeenPostIDs != nil {
			if _, seen := pc.CuratedSeenPostIDs[cand.Candidate.PostID]; seen {
				continue
			}
		}
		winners = append(winners, cand)
	}
	if len(winners) == 0 {
		CuratedInjectionOutcome.WithLabelValues("skipped_all_seen").Inc()
		return scored
	}

	out := append([]model.ScoredCandidate(nil), scored...)
	for i, win := range winners {
		pos := c.positions[i] - 1 // 1-indexed -> 0-indexed
		if pos < 0 || pos > len(out) {
			c.logger.Debug("curated injection position skipped — list too short",
				slog.Int("position", c.positions[i]), slog.Int("list_len", len(out)))
			continue
		}
		out = append(out[:pos:pos], append([]model.ScoredCandidate{win}, out[pos:]...)...)
	}
	CuratedInjectionOutcome.WithLabelValues("injected").Inc()
	return out
}
