// Package m04interest builds a weighted entity interest
// profile from a user's interactions. The output is a single Redis key:
// serendipity:user:interest:{uid}.
//
// The m04 prefix in the package name is deliberate: the interest profile is an input of
// the serving side. postentity, canonicaldict and trfold are unmarked because they are
// shared core, not owned by a single context.
//
// NO LLM, NO VECTORS. All computation is arithmetic.
package m04interest

import "time"

// SchemaVersion is the version of the Profile JSON schema. Incremented when the schema changes;
// if a reader sees an old version it treats the profile as needing regeneration.
const SchemaVersion = 1

// Weights maps entity canonical name → weight.
type Weights map[string]float64

// Confidence is how much data the profile rests on.
//
// Interactions is CUMULATIVE (the number of signals processed over the profile's
// lifetime), not the count of a single run — the only way to tell apart a
// low-interaction user. Concentration is the share of the heaviest entity in the total:
// close to 1 = locked on a single topic, close to 0 = spread out.
type Confidence struct {
	Interactions  int     `json:"i"`
	Concentration float64 `json:"c"`
}

// Profile is the content of serendipity:user:interest:{uid}.
//
// # Size (measured, not estimated)
//
// MAX_ENTITIES is a ceiling PER MAP and there are THREE maps
// (Stable/Recent/Negative). Measured JSON size (measured, not estimated; with realistic
// entity names of ~11 characters, 31k users):
//
//	per map          | unrounded     | 4 decimals | 31k × 4 decimals
//	10               | 1 162 B       |   825 B    |  ~26 MB
//	25               | 2 551 B       | 1 697 B    |  ~53 MB
//	50 (default)     | 4 840 B       | 3 129 B    |  ~97 MB
//
// With key names and Redis object overhead ~110-120 MB at the default —
// a serious item when memory is limited (volatile-lru eviction). The
// "~1-2 KB/user" in design §5 holds only for ~25 entities/map.
// Field names are therefore kept SHORT; weights are also
// rounded to 4 decimals in the Builder before writing (see roundWeights,
// ~35% of the payload).
type Profile struct {
	Version   int       `json:"v"`
	UpdatedAt time.Time `json:"t"`

	// Stable is lasting interest (slow decay — fandom, profession).
	// Curated/discovery matching USES THIS.
	Stable Weights `json:"s"`
	// Recent is current interest (fast decay — this week's agenda).
	Recent Weights `json:"r"`
	// Negative is negative signals (from skip/fast_skip). It is NOT DERIVED from Stable;
	// it is kept separate so that the veto can be calibrated independently.
	Negative Weights `json:"n"`

	// Query is the queryable top-K entities — Phase 3's discovery channel reads this.
	// It is derived from Stable but STORED: so the consumer does not have to rebuild
	// the ranking and threshold logic.
	Query []string `json:"q"`

	Confidence Confidence `json:"cf"`

	// Summary is a natural-language summary. ALWAYS EMPTY in this phase (Decision 1): its only
	// consumer was Alpha's text budget and that integration is out of scope.
	// The field exists already so the schema version does not bump when it is added.
	Summary string `json:"sum,omitempty"`
}

// NewProfile returns an empty profile that does not produce null in JSON.
func NewProfile() Profile {
	return Profile{
		Version:  SchemaVersion,
		Stable:   Weights{},
		Recent:   Weights{},
		Negative: Weights{},
		Query:    []string{},
	}
}

// IsZero says that the profile has never been built.
func (p Profile) IsZero() bool { return p.UpdatedAt.IsZero() }
