// Package niche is the profiler that extracts niche interests from a user's recent interactions and
// writes them to the serendipity:user:niche:{uid} key. curated/content.Fetcher reads this
// key: the cosine threshold between the niche vector and the candidate's post vector decides whether the candidate
// is shown to the user.
//
// Flow: interaction ring (post_ids) → post texts → up to 3 niche categories via the LLM
// → turn category texts into vectors with the embedding service → write as JSON (TTL 48h).
package niche

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	goredis "github.com/redis/go-redis/v9"

	"serendipity/internal/curated/model"
	"serendipity/internal/embedclient"
)

const (
	// KeyPrefix is the key prefix of the niche profile; it must be the SAME as content.Fetcher's.
	KeyPrefix = "serendipity:user:niche:"

	// TTL is the profile's lifetime: a one-night failure refreshes itself the next night.
	TTL = 48 * time.Hour
	// RingLimit is the maximum number of interactions read per user.
	RingLimit = 50
	// MaxCategories is the maximum number of niches written per user.
	MaxCategories = 3
	// MaxTextRunes is the upper limit of a single post text put into the LLM prompt (bounds the prompt size).
	MaxTextRunes = 500
)

// Outcome is the result of a single user operation.
type Outcome string

const (
	Written        Outcome = "written"
	NoInteractions Outcome = "no_interactions" // the ring is empty
	NoText         Outcome = "no_text"         // post_ids could not be resolved to text (expired)
	NoCategories   Outcome = "no_categories"   // the LLM returned no categories
)

// Interactions gives the post IDs the user recently interacted with.
type Interactions interface {
	RecentPostIDs(ctx context.Context, userID string, limit int) ([]string, error)
}

// Hydrator reads text from post_ids (satisfied by postmeta.Store).
type Hydrator interface {
	Hydrate(ctx context.Context, postIDs []string) (map[string]model.PostData, error)
}

// Categorizer extracts up to max niche categories from texts (may be LLM-backed).
type Categorizer interface {
	Categorize(ctx context.Context, texts []string, max int) ([]string, error)
}

// Entry is a single niche item of the written profile (the format content.Fetcher reads).
type Entry struct {
	Category  string    `json:"category"`
	Embedding []float64 `json:"embedding"`
}

// Profiler produces a niche profile per user.
type Profiler struct {
	rdb    goredis.Cmdable
	inter  Interactions
	hyd    Hydrator
	cat    Categorizer
	emb    embedclient.Embedder
	logger *slog.Logger
}

// New makes all dependencies mandatory.
func New(rdb goredis.Cmdable, inter Interactions, hyd Hydrator, cat Categorizer, emb embedclient.Embedder, logger *slog.Logger) (*Profiler, error) {
	if rdb == nil || inter == nil || hyd == nil || cat == nil || emb == nil {
		return nil, errors.New("niche: all dependencies are required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Profiler{rdb: rdb, inter: inter, hyd: hyd, cat: cat, emb: emb, logger: logger}, nil
}

// Profile produces and writes the profile for a single user. If there is no signal (empty ring, unresolvable
// text, response without categories) no LLM/embedding call is made or nothing is written and the reason is returned;
// infrastructure errors are returned as errors and the existing profile is left untouched.
func (p *Profiler) Profile(ctx context.Context, userID string) (Outcome, error) {
	ids, err := p.inter.RecentPostIDs(ctx, userID, RingLimit)
	if err != nil {
		return "", fmt.Errorf("niche: failed to read interactions: %w", err)
	}
	if len(ids) == 0 {
		return NoInteractions, nil
	}
	data, err := p.hyd.Hydrate(ctx, ids)
	if err != nil {
		return "", fmt.Errorf("niche: failed to read post texts: %w", err)
	}
	texts := make([]string, 0, len(ids))
	for _, id := range ids {
		if t := truncate(strings.TrimSpace(data[id].Text), MaxTextRunes); t != "" {
			texts = append(texts, t)
		}
	}
	if len(texts) == 0 {
		return NoText, nil
	}
	cats, err := p.cat.Categorize(ctx, texts, MaxCategories)
	if err != nil {
		return "", fmt.Errorf("niche: categorize: %w", err)
	}
	cats = cleanCategories(cats, MaxCategories)
	if len(cats) == 0 {
		return NoCategories, nil
	}
	vecs, err := p.emb.Embed(ctx, cats, embedclient.Query)
	if err != nil {
		return "", fmt.Errorf("niche: embed: %w", err)
	}
	if len(vecs) != len(cats) {
		return "", fmt.Errorf("niche: got %d vectors for %d categories", len(vecs), len(cats))
	}
	entries := make([]Entry, len(cats))
	for i, c := range cats {
		entries[i] = Entry{Category: c, Embedding: toFloat64(vecs[i])}
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		return "", err
	}
	if err := p.rdb.Set(ctx, KeyPrefix+userID, raw, TTL).Err(); err != nil {
		return "", fmt.Errorf("niche: write: %w", err)
	}
	return Written, nil
}

// Summary is the result breakdown of a run.
type Summary struct {
	Counts map[Outcome]int
	Failed int
}

// Run processes the users in order. One user's error does not stop the others
// (it is counted and logged); only a cancelled context cuts the run short and returns an error.
func (p *Profiler) Run(ctx context.Context, users []string) (Summary, error) {
	s := Summary{Counts: map[Outcome]int{}}
	for _, u := range users {
		if err := ctx.Err(); err != nil {
			return s, err
		}
		out, err := p.Profile(ctx, u)
		if err != nil {
			s.Failed++
			p.logger.Warn("failed to build niche profile", slog.String("user", u), slog.Any("error", err))
			continue
		}
		s.Counts[out]++
	}
	return s, nil
}

func cleanCategories(in []string, max int) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, max)
	for _, c := range in {
		c = strings.TrimSpace(c)
		key := strings.ToLower(c)
		if c == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, c)
		if len(out) == max {
			break
		}
	}
	return out
}

func truncate(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	r := []rune(s)
	return string(r[:maxRunes])
}

func toFloat64(v []float32) []float64 {
	out := make([]float64, len(v))
	for i, x := range v {
		out[i] = float64(x)
	}
	return out
}
