// Package core is the small type core shared by both pipelines (candidate, slot type,
// author affinity, raw post). It carries only the fields the pipelines use.
package core

import "time"

// SlotType says which retrieval slot a candidate came from.
type SlotType int

const (
	SlotFavoriteAuthors SlotType = iota + 1
	SlotInNetwork
	SlotOutOfNetwork
	// SlotCurated: candidates marked by the curated post-boost injection.
	SlotCurated
	// SlotEntity: entity-driven discovery slot.
	SlotEntity
)

func (s SlotType) String() string {
	switch s {
	case SlotFavoriteAuthors:
		return "favorite_authors"
	case SlotInNetwork:
		return "in_network"
	case SlotOutOfNetwork:
		return "out_of_network"
	case SlotCurated:
		return "curated"
	case SlotEntity:
		return "entity"
	default:
		return "unknown"
	}
}

// Priority: the smaller value wins in dedup.
func (s SlotType) Priority() int {
	switch s {
	case SlotFavoriteAuthors:
		return 1
	case SlotInNetwork:
		return 2
	case SlotEntity:
		return 3
	case SlotOutOfNetwork:
		return 4
	case SlotCurated:
		return 0
	default:
		return 99
	}
}

// PostMetadata is the candidate's post metadata.
type PostMetadata struct {
	Category  string   `json:"category"`
	Language  string   `json:"language"`
	Hashtags  []string `json:"hashtags"`
	AuthorID  string   `json:"author_id"`
	HasImages bool     `json:"has_images"`
	HasVideo  bool     `json:"has_video"`
}

// Candidate is a candidate returned by a retrieval slot.
type Candidate struct {
	PostID      string       `json:"post_id"`
	AuthorID    string       `json:"author_id"`
	Score       float64      `json:"score"`
	SlotSource  SlotType     `json:"slot_source"`
	Metadata    PostMetadata `json:"metadata"`
	RetrievedAt time.Time    `json:"retrieved_at"`
	ANNScore    float64      `json:"ann_score,omitempty"`
	ColdStart   bool         `json:"cold_start,omitempty"`
}

// AuthorAffinity is a user's affinity to an author.
type AuthorAffinity struct {
	AuthorID         string  `json:"author_id"`
	InteractionCount int     `json:"interaction_count"`
	AffinityScore    float64 `json:"affinity_score"` // normalized [0,1]
}

// RawPost is a raw post record.
type RawPost struct {
	PostID    string    `json:"post_id"`
	AuthorID  string    `json:"author_id"`
	Text      string    `json:"text"`
	ImageURLs []string  `json:"image_urls"`
	VideoURLs []string  `json:"video_urls"`
	Hashtags  []string  `json:"hashtags"`
	Category  string    `json:"category"`
	Language  string    `json:"language"`
	CreatedAt time.Time `json:"created_at"`
	IsReply   bool      `json:"is_reply,omitempty"`
	IsReblog  bool      `json:"is_reblog,omitempty"`
}
