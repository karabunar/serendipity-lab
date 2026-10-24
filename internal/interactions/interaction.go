// Package interactions carries the single record of the user interaction ring and
// the type constants. It is deliberately minimal.
package interactions

import "time"

// RawInteraction is a user's single interaction with a post.
type RawInteraction struct {
	UserID          string `json:"user_id"`
	PostID          string `json:"post_id"`
	AuthorID        string `json:"author_id,omitempty"`
	InteractionType string `json:"interaction_type"`
	// Value is the signal weight; kept in a BOUNDED range (~[0.5, 2.0]),
	// NOT the raw dwell_ms/completion_rate.
	Value     float64   `json:"value"`
	Timestamp time.Time `json:"timestamp"`
	DwellMs   int64     `json:"dwell_ms,omitempty"`
}

const (
	InteractionLike            = "like"
	InteractionReply           = "reply"
	InteractionReblog          = "reblog"
	InteractionDwell           = "dwell"
	InteractionVideoWatch      = "video_watch"
	InteractionVideoCompletion = "video_completion"
	InteractionImageView       = "image_view"
	InteractionSkip            = "skip"
	InteractionDetailView      = "detail_view"
)
