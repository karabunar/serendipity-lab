package content

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"

	"serendipity/internal/core"
	"serendipity/internal/curated/model"
)

func TestRecorder_RecordInjections_AddsToSeenSet(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})

	rec := NewRecorder(rdb, 24*time.Hour, testLogger())
	injected := []model.ScoredCandidate{
		{Candidate: core.Candidate{PostID: "p1", SlotSource: core.SlotCurated}},
	}
	rec.RecordInjections(context.Background(), "u1", injected)

	isMember, err := mr.SIsMember("serendipity:curated:seen:u1", "p1")
	if err != nil {
		t.Fatalf("SIsMember: %v", err)
	}
	if !isMember {
		t.Error("expected p1 in serendipity:curated:seen:u1")
	}
	ttl := mr.TTL("serendipity:curated:seen:u1")
	if ttl <= 0 {
		t.Errorf("expected TTL set, got %v", ttl)
	}
}

func TestRecorder_EmptyInjected_NoOp(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})

	rec := NewRecorder(rdb, 24*time.Hour, testLogger())
	rec.RecordInjections(context.Background(), "u1", nil)

	if mr.Exists("serendipity:curated:seen:u1") {
		t.Error("expected no key created on empty injected list")
	}
}
