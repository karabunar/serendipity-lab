package m04interest

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
)

func testStore(t *testing.T) (*RedisStore, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	lg := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return NewRedisStore(rdb, time.Hour, lg), mr
}

func TestRedisStore_WriteReadRoundTrip(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()

	p := NewProfile()
	p.UpdatedAt = time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)
	p.Stable = Weights{"Fenerbahçe": 8.4}
	p.Query = []string{"Fenerbahçe"}
	p.Confidence = Confidence{Interactions: 143, Concentration: 0.62}

	if err := s.Save(ctx, "u1", p); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, "u1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Stable["Fenerbahçe"] != 8.4 || got.Confidence.Interactions != 143 {
		t.Fatalf("got = %+v", got)
	}
	if !got.UpdatedAt.Equal(p.UpdatedAt) {
		t.Fatalf("UpdatedAt = %v", got.UpdatedAt)
	}
}

func TestRedisStore_MissingUserEmptyProfileNoError(t *testing.T) {
	// On the first run there is no profile; this is NOT AN ERROR, it means "start from scratch".
	s, _ := testStore(t)
	got, err := s.Get(context.Background(), "hic-yok")
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsZero() {
		t.Fatalf("got = %+v", got)
	}
}

func TestRedisStore_CorruptJSONReturnsEmptyProfile(t *testing.T) {
	// A manually corrupted record must not block the user permanently;
	// the profile can be rebuilt from scratch.
	s, mr := testStore(t)
	mr.Set(ProfileKey("u1"), "{bozuk")
	got, err := s.Get(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsZero() {
		t.Fatalf("got = %+v", got)
	}
}

func TestRedisStore_OldSchemaVersionTreatedAsEmpty(t *testing.T) {
	// When the schema changes the profile must be rebuilt; continuing with
	// half-read old fields would be silent corruption.
	s, mr := testStore(t)
	mr.Set(ProfileKey("u1"), `{"v":0,"t":"2026-08-12T09:00:00Z","s":{"A":1}}`)
	got, err := s.Get(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsZero() {
		t.Fatalf("old schema accepted: %+v", got)
	}
}

func TestRedisStore_TTLApplied(t *testing.T) {
	s, mr := testStore(t)
	if err := s.Save(context.Background(), "u1", NewProfile()); err != nil {
		t.Fatal(err)
	}
	if ttl := mr.TTL(ProfileKey("u1")); ttl <= 0 {
		t.Fatalf("TTL = %v, want > 0", ttl)
	}
}

func TestProfileKey(t *testing.T) {
	if got := ProfileKey("u1"); got != "serendipity:user:interest:u1" {
		t.Fatalf("ProfileKey = %q", got)
	}
}
