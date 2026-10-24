package entitylink

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"
)

type fakeLoader struct {
	values map[string]string
	err    error
}

func (f fakeLoader) Load(context.Context) (map[string]string, error) {
	return f.values, f.err
}

func TestCacheMatchHonorsWordAndSuffixBoundaries(t *testing.T) {
	c := NewCache(fakeLoader{values: map[string]string{
		"istanbul havalimani": "İstanbul",
		"filistinli":          "Filistin",
	}}, time.Hour, slog.Default())
	c.Start(context.Background())
	t.Cleanup(c.Stop)

	got := c.Match("İstanbul Havalimanı'nda Filistinli yolcular")
	if len(got) != 2 || got[0] != "İstanbul" || got[1] != "Filistin" {
		t.Fatalf("Match = %#v", got)
	}
	if got := c.Match("alistanbul içinde"); len(got) != 0 {
		t.Fatalf("embedded match = %#v, want empty", got)
	}
}

func TestCacheRefreshKeepsOldSnapshotOnError(t *testing.T) {
	loader := &mutableLoader{values: map[string]string{"istanbul havalimani": "İstanbul"}}
	c := NewCache(loader, time.Hour, slog.Default())
	c.Start(context.Background())
	t.Cleanup(c.Stop)
	loader.err = errors.New("loader failed")
	c.RefreshOnce(context.Background())
	if got, ok := c.Canonical("istanbul havalimani"); !ok || got != "İstanbul" {
		t.Fatalf("old snapshot lost: %q,%v", got, ok)
	}
}

type mutableLoader struct {
	values map[string]string
	err    error
}

func (m *mutableLoader) Load(context.Context) (map[string]string, error) {
	return m.values, m.err
}
