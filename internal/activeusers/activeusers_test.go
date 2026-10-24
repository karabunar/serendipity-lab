package activeusers

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
)

func setup(t *testing.T) (*goredis.Client, *ZSet) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	for i, u := range []string{"u1", "u2", "u3", "u4", "u5"} {
		rdb.ZAdd(context.Background(), DefaultKey, goredis.Z{Score: float64(i), Member: u})
	}
	return rdb, New(rdb, "")
}

func TestPageByIndex_PagesAndEndsEmpty(t *testing.T) {
	_, z := setup(t)
	p, _ := z.PageByIndex(context.Background(), 0, 2)
	if len(p) != 2 || p[0] != "u1" {
		t.Fatalf("first page wrong: %v", p)
	}
	p, _ = z.PageByIndex(context.Background(), 4, 2)
	if len(p) != 1 || p[0] != "u5" {
		t.Fatalf("last page wrong: %v", p)
	}
	p, _ = z.PageByIndex(context.Background(), 10, 2)
	if len(p) != 0 {
		t.Fatalf("end of set must be empty: %v", p)
	}
}

func TestPageByIndex_InvalidArgsReturnNothing(t *testing.T) {
	_, z := setup(t)
	if p, err := z.PageByIndex(context.Background(), 0, 0); err != nil || p != nil {
		t.Fatalf("count=0 must return empty: %v %v", p, err)
	}
	if p, err := z.PageByIndex(context.Background(), -1, 3); err != nil || p != nil {
		t.Fatalf("negative offset must return empty: %v %v", p, err)
	}
}

func TestAll_ReadsEveryUserAcrossPages(t *testing.T) {
	_, z := setup(t)
	all, err := z.All(context.Background(), 2)
	if err != nil || len(all) != 5 {
		t.Fatalf("expected 5 users: %v %v", all, err)
	}
}
