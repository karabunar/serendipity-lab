package interestsrc

import (
	"context"
	"testing"
	"time"

	"serendipity/internal/interactions"
	"serendipity/internal/m04interest"
)

type fakeRing struct {
	items []interactions.RawInteraction
	limit int
}

func (f *fakeRing) GetRecent(_ context.Context, _ string, limit int) ([]interactions.RawInteraction, error) {
	f.limit = limit
	return f.items, nil
}

type fakeResolver map[string][]string

func (f fakeResolver) EntitiesOf(_ context.Context, ids []string) (map[string][]string, error) {
	out := map[string][]string{}
	for _, id := range ids {
		if e, ok := f[id]; ok {
			out[id] = e
		}
	}
	return out, nil
}

func TestRingPositives_InteractionBeforeSinceSkipped(t *testing.T) {
	// Core of the rolling update: if the same interaction is added twice
	// the weight would inflate on every run.
	now := time.Now()
	ring := &fakeRing{items: []interactions.RawInteraction{
		{PostID: "eski", Value: 1, Timestamp: now.Add(-48 * time.Hour)},
		{PostID: "yeni", Value: 2, Timestamp: now.Add(-1 * time.Hour)},
	}}
	res := fakeResolver{"eski": {"A"}, "yeni": {"B"}}

	sigs, err := NewRingPositives(ring, res, 200).Positives(context.Background(), "u1", now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(sigs) != 1 || sigs[0].Entities[0] != "B" {
		t.Fatalf("sigs = %+v", sigs)
	}
	if sigs[0].Value != 2 {
		t.Fatalf("Value = %v, want 2 (RawInteraction.Value as is)", sigs[0].Value)
	}
}

func TestRingPositives_EntitylessPostProducesNoSignal(t *testing.T) {
	now := time.Now()
	ring := &fakeRing{items: []interactions.RawInteraction{{PostID: "p1", Value: 1, Timestamp: now}}}
	sigs, err := NewRingPositives(ring, fakeResolver{}, 200).Positives(context.Background(), "u1", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sigs) != 0 {
		t.Fatalf("sigs = %+v", sigs)
	}
}

func TestRingPositives_ZeroSinceTakesAll(t *testing.T) {
	// First run: no profile → since is zero → everything in the buffer is processed.
	now := time.Now()
	ring := &fakeRing{items: []interactions.RawInteraction{
		{PostID: "a", Value: 1, Timestamp: now.Add(-100 * time.Hour)},
		{PostID: "b", Value: 1, Timestamp: now},
	}}
	res := fakeResolver{"a": {"A"}, "b": {"B"}}
	sigs, err := NewRingPositives(ring, res, 200).Positives(context.Background(), "u1", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sigs) != 2 {
		t.Fatalf("sigs = %+v", sigs)
	}
}

func TestRingPositives_SkipNotWrittenToBufferSoAbsentFromPositives(t *testing.T) {
	// Defensive: if a skip is written to the buffer one day, it must not count as POSITIVE.
	now := time.Now()
	ring := &fakeRing{items: []interactions.RawInteraction{
		{PostID: "p1", InteractionType: interactions.InteractionSkip, Value: 1, Timestamp: now},
	}}
	sigs, err := NewRingPositives(ring, fakeResolver{"p1": {"A"}}, 200).Positives(context.Background(), "u1", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sigs) != 0 {
		t.Fatalf("skip counted as positive: %+v", sigs)
	}
}

func TestRingPositives_LimitPassedOn(t *testing.T) {
	ring := &fakeRing{}
	if _, err := NewRingPositives(ring, fakeResolver{}, 137).Positives(context.Background(), "u1", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if ring.limit != 137 {
		t.Fatalf("limit = %d, want 137", ring.limit)
	}
}

var _ m04interest.PositiveSource = (*RingPositives)(nil)
