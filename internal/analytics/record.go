// Package analytics carries the record types of the per-user-day engagement summary (analytics mirror).
// Only the fields used by the
// read side are present; the storage implementation lives elsewhere.
package analytics

// SkipAnalytics holds the counters of skipped content.
type SkipAnalytics struct {
	Count      int64 `json:"count"`
	DwellMsSum int64 `json:"dwell_ms_sum"`
}

// AnalyticRecord is a user's daily summary for one piece of content.
type AnalyticRecord struct {
	UserID    string `json:"user_id"`
	ContentID string `json:"content_id"`
	PostID    string `json:"post_id,omitempty"`
	AuthorID  string `json:"author_id,omitempty"`
	Day       string `json:"day"`        // "YYYY-MM-DD" format
	LastTsMs  int64  `json:"last_ts_ms"` // last interaction time (epoch ms); missing → 0

	Skip *SkipAnalytics `json:"skip,omitempty"`
}
