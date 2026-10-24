package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"serendipity/internal/interactions"
)

type rec struct {
	batches [][]interactions.RawInteraction
	err     error
}

func (r *rec) RecordBatch(_ context.Context, items []interactions.RawInteraction) error {
	if r.err != nil {
		return r.err
	}
	r.batches = append(r.batches, append([]interactions.RawInteraction(nil), items...))
	return nil
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestSeed_ParsesBatchesAndCountsInvalid(t *testing.T) {
	input := strings.Join([]string{
		`{"user_id":"u1","post_id":"p1","author_id":"a1","type":"like","timestamp":"2025-06-15T12:00:00Z"}`,
		``,
		`bozuk json`,
		`{"user_id":"u1","post_id":"p2","type":"yok"}`,
		`{"user_id":"u1","post_id":"p3","type":"skip","dwell_ms":120}`,
		`{"user_id":"u2","post_id":"p4","type":"reblog","value":1.5}`,
		`{"user_id":"u2","post_id":"p5","type":"like","timestamp":"dün"}`,
	}, "\n")
	r := &rec{}
	res, err := seed(context.Background(), strings.NewReader(input), r, 2, quiet())
	if err != nil {
		t.Fatal(err)
	}
	if res.Read != 6 || res.Written != 3 || res.Invalid != 3 {
		t.Fatalf("wrong summary: %+v", res)
	}
	if len(r.batches) != 2 || len(r.batches[0]) != 2 || len(r.batches[1]) != 1 {
		t.Fatalf("expected 2+1 batches: %v", r.batches)
	}
	first := r.batches[0][0]
	if first.AuthorID != "a1" || first.InteractionType != "like" || first.Timestamp.Year() != 2025 {
		t.Fatalf("fields were carried over incorrectly: %+v", first)
	}
	if r.batches[0][1].DwellMs != 120 || r.batches[1][0].Value != 1.5 {
		t.Fatalf("dwell/value were not carried over: %+v", r.batches)
	}
}

func TestSeed_RecorderErrorStops(t *testing.T) {
	_, err := seed(context.Background(), strings.NewReader(`{"user_id":"u","post_id":"p","type":"like"}`), &rec{err: errors.New("redis unavailable")}, 10, quiet())
	if err == nil || !strings.Contains(err.Error(), "redis unavailable") {
		t.Fatalf("writer error expected: %v", err)
	}
}

func TestSeed_EmptyInput(t *testing.T) {
	res, err := seed(context.Background(), strings.NewReader("\n\n"), &rec{}, 10, quiet())
	if err != nil || res != (Result{}) {
		t.Fatalf("empty input must return zero: %+v %v", res, err)
	}
}
