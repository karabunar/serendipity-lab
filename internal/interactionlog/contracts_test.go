package interactionlog_test

import (
	"serendipity/internal/curated/niche"
	"serendipity/internal/interactionlog"
	"serendipity/internal/m04interest/interestsrc"
)

// Locks at compile time that the log satisfies the four interfaces its consumers expect.
var (
	_ interestsrc.RingReader     = (*interactionlog.Log)(nil)
	_ interestsrc.MirrorReader   = (*interactionlog.Log)(nil)
	_ interestsrc.FavoriteReader = (*interactionlog.Log)(nil)
	_ niche.Interactions         = (*interactionlog.Log)(nil)
)
