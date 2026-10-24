// Package model holds the scored candidate, post data and
// process context types used by the curated pipeline. These are only the fields this pipeline uses.
package model

import (
	"serendipity/internal/core"
	"serendipity/internal/postmeta"
)

// ScoredCandidate is a scored candidate.
type ScoredCandidate struct {
	Candidate  core.Candidate `json:"candidate"`
	FinalScore float64        `json:"final_score"` // ranking score in the curated pool
}

// PostData is the content summary read from the post HASH (the definition is in postmeta).
type PostData = postmeta.PostData

// ProcessContext is the request context fed into the injector.
type ProcessContext struct {
	// CuratedCandidates are the candidates from the curated pool (sorted by score).
	CuratedCandidates []ScoredCandidate
	// CuratedSeenPostIDs are the curated posts the user has seen before.
	CuratedSeenPostIDs map[string]struct{}
}
