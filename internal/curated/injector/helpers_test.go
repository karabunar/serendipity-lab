package injector

import (
	"io"
	"log/slog"

	"serendipity/internal/core"
	"serendipity/internal/curated/model"
)

func sc(postID, authorID string, score float64) model.ScoredCandidate {
	return model.ScoredCandidate{
		Candidate:  core.Candidate{PostID: postID, AuthorID: authorID},
		FinalScore: score,
	}
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
