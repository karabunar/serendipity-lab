package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"serendipity/internal/curated/niche"
)

type fakeUsers struct {
	list []string
	err  error
}

func (f fakeUsers) All(context.Context, int) ([]string, error) { return f.list, f.err }

type fakeRunner struct {
	got []string
	err error
}

func (f *fakeRunner) Run(_ context.Context, u []string) (niche.Summary, error) {
	f.got = u
	return niche.Summary{Counts: map[niche.Outcome]int{niche.Written: len(u)}}, f.err
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestExecute_PassesUsersToRunner(t *testing.T) {
	r := &fakeRunner{}
	if err := execute(context.Background(), fakeUsers{list: []string{"u1", "u2"}}, r, quiet()); err != nil {
		t.Fatal(err)
	}
	if len(r.got) != 2 {
		t.Fatalf("users did not reach the runner: %v", r.got)
	}
}

func TestExecute_ListErrorStopsBeforeRun(t *testing.T) {
	r := &fakeRunner{}
	err := execute(context.Background(), fakeUsers{err: errors.New("redis unavailable")}, r, quiet())
	if err == nil || r.got != nil {
		t.Fatalf("run must not start on a list error: %v %v", err, r.got)
	}
}

func TestExecute_RunnerErrorPropagates(t *testing.T) {
	r := &fakeRunner{err: context.Canceled}
	if err := execute(context.Background(), fakeUsers{list: []string{"u"}}, r, quiet()); !errors.Is(err, context.Canceled) {
		t.Fatalf("runner error expected: %v", err)
	}
}
