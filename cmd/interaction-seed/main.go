// Command interaction-seed reads JSONL interaction events from stdin (or from the file given with -file)
// and writes them to the interaction log (interactionlog). It is the shortest way to try user-interest and
// niche-profiler with your own data.
//
// Line format:
//
//	{"user_id":"u1","post_id":"p1","author_id":"a1","type":"like","timestamp":"2025-06-15T12:00:00Z"}
//
// type: like | reply | reblog | dwell | image_view | detail_view | video_watch | video_completion | skip.
// Required: user_id, post_id, type. Optional: author_id (needed for favorite authors),
// value (default: the type weight), dwell_ms, timestamp (RFC3339; default now).
//
// Invalid lines are skipped and counted; valid ones are written. If there are invalid lines the exit code is 2.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"serendipity/internal/interactionlog"
	"serendipity/internal/interactions"
	"serendipity/internal/redisenv"
)

type recorder interface {
	RecordBatch(ctx context.Context, items []interactions.RawInteraction) error
}

// Result is the summary of a load run.
type Result struct {
	Read, Written, Invalid int
}

type line struct {
	UserID    string  `json:"user_id"`
	PostID    string  `json:"post_id"`
	AuthorID  string  `json:"author_id"`
	Type      string  `json:"type"`
	Value     float64 `json:"value"`
	DwellMs   int64   `json:"dwell_ms"`
	Timestamp string  `json:"timestamp"`
}

func parse(raw []byte) (interactions.RawInteraction, error) {
	var l line
	if err := json.Unmarshal(raw, &l); err != nil {
		return interactions.RawInteraction{}, err
	}
	it := interactions.RawInteraction{
		UserID: l.UserID, PostID: l.PostID, AuthorID: l.AuthorID,
		InteractionType: l.Type, Value: l.Value, DwellMs: l.DwellMs,
	}
	if l.Timestamp != "" {
		ts, err := time.Parse(time.RFC3339, l.Timestamp)
		if err != nil {
			return it, fmt.Errorf("timestamp is not RFC3339: %w", err)
		}
		it.Timestamp = ts
	}
	return it, interactionlog.Validate(it)
}

// seed writes the JSONL lines in r to rec in batches. Blank lines are ignored.
func seed(ctx context.Context, r io.Reader, rec recorder, batch int, logger *slog.Logger) (Result, error) {
	if batch <= 0 {
		batch = 500
	}
	var res Result
	buf := make([]interactions.RawInteraction, 0, batch)
	flush := func() error {
		if len(buf) == 0 {
			return nil
		}
		if err := rec.RecordBatch(ctx, buf); err != nil {
			return err
		}
		res.Written += len(buf)
		buf = buf[:0]
		return nil
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for n := 1; sc.Scan(); n++ {
		raw := sc.Bytes()
		if len(trim(raw)) == 0 {
			continue
		}
		res.Read++
		it, err := parse(raw)
		if err != nil {
			res.Invalid++
			if res.Invalid <= 5 {
				logger.Warn("invalid line skipped", slog.Int("line", n), slog.Any("error", err))
			}
			continue
		}
		buf = append(buf, it)
		if len(buf) >= batch {
			if err := flush(); err != nil {
				return res, err
			}
		}
		if err := ctx.Err(); err != nil {
			return res, err
		}
	}
	if err := sc.Err(); err != nil {
		return res, fmt.Errorf("failed to read input: %w", err)
	}
	return res, flush()
}

func trim(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t' || b[0] == '\r') {
		b = b[1:]
	}
	return b
}

func main() {
	file := flag.String("file", "-", "JSONL file ('-' = stdin)")
	batch := flag.Int("batch", 500, "number of events per pipeline")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	in := io.Reader(os.Stdin)
	if *file != "-" {
		f, err := os.Open(*file)
		if err != nil {
			logger.Error("failed to open file", slog.Any("error", err))
			os.Exit(1)
		}
		defer f.Close()
		in = f
	}
	rdb := redisenv.New()
	defer rdb.Close()

	res, err := seed(context.Background(), in, interactionlog.New(rdb, 0, 0), *batch, logger)
	logger.Info("load finished", slog.Int("read", res.Read), slog.Int("written", res.Written), slog.Int("invalid", res.Invalid))
	if err != nil {
		logger.Error("load aborted midway", slog.Any("error", err))
		os.Exit(1)
	}
	if res.Invalid > 0 {
		os.Exit(2)
	}
}
