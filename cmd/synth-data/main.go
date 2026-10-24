// Command synth-data generates synthetic data for experiments and writes it to Redis (post HASHes, entity
// index, author posts, active users, F2 sets, interaction log).
//
//	go run ./cmd/synth-data -users 20 -authors 12 -posts-per-author 15 -interactions 40 -seed 1
//
// -dry-run does not touch Redis and only prints a summary. If a -jsonl-out file is given, the interactions
// are also written in the JSONL format that cmd/interaction-seed reads.
//
// After writing: POST_INDEXER_ENABLED=true go run ./cmd/post-indexer (needs embedding + Qdrant),
// USER_INTEREST_ENABLED=true go run ./cmd/user-interest, NICHE_PROFILER_ENABLED=true go run ./cmd/niche-profiler.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"serendipity/internal/interactions"
	"serendipity/internal/redisenv"
	"serendipity/internal/synth"
)

// jsonlLine is the line format of cmd/interaction-seed.
type jsonlLine struct {
	UserID    string  `json:"user_id"`
	PostID    string  `json:"post_id"`
	AuthorID  string  `json:"author_id,omitempty"`
	Type      string  `json:"type"`
	Value     float64 `json:"value,omitempty"`
	DwellMs   int64   `json:"dwell_ms,omitempty"`
	Timestamp string  `json:"timestamp"`
}

// writeJSONL writes the events as JSON, one per line.
func writeJSONL(w io.Writer, items []interactions.RawInteraction) error {
	enc := json.NewEncoder(w)
	for _, it := range items {
		l := jsonlLine{
			UserID: it.UserID, PostID: it.PostID, AuthorID: it.AuthorID, Type: it.InteractionType,
			Value: it.Value, DwellMs: it.DwellMs, Timestamp: it.Timestamp.UTC().Format(time.RFC3339),
		}
		if err := enc.Encode(l); err != nil {
			return err
		}
	}
	return nil
}

func main() {
	cfg := synth.Config{}
	flag.Int64Var(&cfg.Seed, "seed", 1, "random seed (same seed → same structure)")
	flag.IntVar(&cfg.Users, "users", 20, "number of users")
	flag.IntVar(&cfg.Authors, "authors", 12, "number of authors")
	flag.IntVar(&cfg.PostsPerAuthor, "posts-per-author", 15, "posts per author")
	flag.IntVar(&cfg.InteractionsPerUser, "interactions", 40, "interactions per user")
	dry := flag.Bool("dry-run", false, "do not write to Redis, only print a summary")
	jsonlOut := flag.String("jsonl-out", "", "also write interactions to this file as JSONL")
	flag.Parse()
	cfg.Now = time.Now() // entity index and profile windows run against the real clock

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ds := synth.Generate(cfg)

	if *jsonlOut != "" {
		f, err := os.Create(*jsonlOut)
		if err == nil {
			err = writeJSONL(f, ds.Interactions)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
		}
		if err != nil {
			logger.Error("failed to write JSONL", slog.Any("error", err))
			os.Exit(1)
		}
		logger.Info("JSONL written", slog.String("file", *jsonlOut), slog.Int("events", len(ds.Interactions)))
	}

	if *dry {
		logger.Info("dry-run: nothing written to Redis", slog.Int("users", len(ds.Users)), slog.Int("authors", len(ds.Authors)),
			slog.Int("posts", len(ds.Posts)), slog.Int("interactions", len(ds.Interactions)))
		return
	}
	rdb := redisenv.New()
	defer rdb.Close()
	s, err := synth.Write(context.Background(), rdb, ds, logger)
	if err != nil {
		logger.Error("write failed", slog.Any("error", err))
		os.Exit(1)
	}
	logger.Info("synthetic data written", slog.String("summary", fmt.Sprintf("%+v", s)))
}
