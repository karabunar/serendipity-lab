package injector

import (
	"context"
	"testing"

	"serendipity/internal/curated/model"
)

func curSc(postID, authorID string, score float64) model.ScoredCandidate {
	return sc(postID, authorID, score)
}

func TestCuratedInjector_InsertsAtFixedPosition(t *testing.T) {
	organic := []model.ScoredCandidate{
		sc("o1", "u1", 0.9), sc("o2", "u2", 0.8), sc("o3", "u3", 0.7),
		sc("o4", "u4", 0.6), sc("o5", "u5", 0.5), sc("o6", "u6", 0.4),
		sc("o7", "u7", 0.3), sc("o8", "u8", 0.2),
	}
	pc := model.ProcessContext{
		CuratedCandidates: []model.ScoredCandidate{
			curSc("c1", "author1", 3.0),
		},
	}
	inj := NewCuratedInjector([]int{7}, testLogger())
	out := inj.Apply(context.Background(), organic, pc)

	if len(out) != 9 {
		t.Fatalf("want 9 items (8 organic + 1 injected), got %d", len(out))
	}
	if out[6].Candidate.PostID != "c1" {
		t.Errorf("position 7 (index 6) = %s, want c1", out[6].Candidate.PostID)
	}
}

func TestCuratedInjector_EmptyPool_NoOp(t *testing.T) {
	organic := []model.ScoredCandidate{sc("o1", "u1", 0.9)}
	pc := model.ProcessContext{}
	inj := NewCuratedInjector([]int{7}, testLogger())
	out := inj.Apply(context.Background(), organic, pc)
	if len(out) != 1 || out[0].Candidate.PostID != "o1" {
		t.Errorf("expected no-op on empty pool, got %+v", out)
	}
}

func TestCuratedInjector_AllSeen_NoOp(t *testing.T) {
	organic := []model.ScoredCandidate{sc("o1", "u1", 0.9)}
	pc := model.ProcessContext{
		CuratedCandidates:  []model.ScoredCandidate{curSc("c1", "author1", 3.0)},
		CuratedSeenPostIDs: map[string]struct{}{"c1": {}},
	}
	inj := NewCuratedInjector([]int{7}, testLogger())
	out := inj.Apply(context.Background(), organic, pc)
	if len(out) != 1 || out[0].Candidate.PostID != "o1" {
		t.Errorf("expected no-op when all candidates seen, got %+v", out)
	}
}

func TestCuratedInjector_ShortList_PositionSkipped(t *testing.T) {
	organic := []model.ScoredCandidate{sc("o1", "u1", 0.9), sc("o2", "u2", 0.8)}
	pc := model.ProcessContext{
		CuratedCandidates: []model.ScoredCandidate{curSc("c1", "author1", 3.0)},
	}
	inj := NewCuratedInjector([]int{7}, testLogger())
	out := inj.Apply(context.Background(), organic, pc)
	if len(out) != 2 {
		t.Errorf("want 2 items (position 7 skipped — list too short), got %d", len(out))
	}
}
