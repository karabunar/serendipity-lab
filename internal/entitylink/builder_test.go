package entitylink

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
)

func TestBuildMappings_InclusionAndSuffix(t *testing.T) {
	got := BuildMappings(map[string]int{
		"İstanbul":            12,
		"İstanbul Havalimanı": 3,
		"Filistin":            20,
		"Filistinli":          2,
		"Tekil Yüzey":         1,
	}, DefaultBuilderConfig())

	if got[normalize("İstanbul Havalimanı")] != "İstanbul" {
		t.Fatalf("inclusion mapping = %q, want İstanbul", got[normalize("İstanbul Havalimanı")])
	}
	if got[normalize("Filistinli")] != "Filistin" {
		t.Fatalf("suffix mapping = %q, want Filistin", got[normalize("Filistinli")])
	}
	if _, ok := got[normalize("Tekil Yüzey")]; ok {
		t.Fatal("single-word surface must not enter the corpus linker")
	}
}

func TestBuildMappings_ResolvesChainsAndIsDeterministic(t *testing.T) {
	counts := map[string]int{
		"Türkiye":                       30,
		"Türkiye Cumhuriyeti":           10,
		"Türkiye Cumhuriyeti vatandaşı": 2,
	}
	first := BuildMappings(counts, DefaultBuilderConfig())
	second := BuildMappings(counts, DefaultBuilderConfig())
	if first[normalize("Türkiye Cumhuriyeti vatandaşı")] != "Türkiye" {
		t.Fatalf("chain mapping = %q, want Türkiye", first[normalize("Türkiye Cumhuriyeti vatandaşı")])
	}
	if len(first) != len(second) {
		t.Fatalf("non-deterministic mapping size: %d vs %d", len(first), len(second))
	}
}

func TestRedisWriterReplaceIsReadableByCache(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	writer := NewRedisWriter(rdb, slog.Default())
	if err := writer.Replace(context.Background(), map[string]string{"istanbul havalimani": "İstanbul"}); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	cache := NewCache(NewRedisLoader(rdb), time.Hour, slog.Default())
	cache.Start(context.Background())
	t.Cleanup(cache.Stop)
	if got, ok := cache.Canonical("İSTANBUL HAVALİMANI"); !ok || got != "İstanbul" {
		t.Fatalf("Canonical = %q,%v", got, ok)
	}
	matched := cache.Match("İstanbul Havalimanı'na uçuş")
	if len(matched) != 1 || matched[0] != "İstanbul" {
		t.Fatalf("Match = %#v, want [İstanbul]", matched)
	}
}

func TestRedisWriterEmptyClearsOldSnapshot(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	writer := NewRedisWriter(rdb, slog.Default())
	if err := writer.Replace(context.Background(), map[string]string{"abcde": "ABC"}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Replace(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if n, err := rdb.HLen(context.Background(), Key).Result(); err != nil || n != 0 {
		t.Fatalf("empty replace left hash: n=%d err=%v", n, err)
	}
}
