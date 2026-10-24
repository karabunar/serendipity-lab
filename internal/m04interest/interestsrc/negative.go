package interestsrc

import (
	"context"
	"time"

	"serendipity/internal/analytics"
	"serendipity/internal/m04interest"
)

// dayLayout is the format of analytics.AnalyticRecord.Day (analyticsmirror.dayKeyToISO
// converts "20060102" → "2006-01-02").
const dayLayout = "2006-01-02"

// MirrorReader is the only method needed for reading from the analytics mirror.
// analyticsmirror.RedisAnalyticsStore already satisfies this signature.
type MirrorReader interface {
	FetchUserAnalytics(ctx context.Context, userID string, days int) ([]analytics.AnalyticRecord, error)
}

// MirrorNegatives turns the skip counts in the analytics mirror into negative
// entity signals.
//
// Why the mirror and not the buffer: `skip` is defined as interactions.InteractionSkip
// but the engagement consumer does NOT write it to the ring buffer; the negative
// signal lives only here.
type MirrorNegatives struct {
	mirror       MirrorReader
	res          m04interest.EntityResolver
	lookbackDays int
}

// NewMirrorNegatives uses 30 for lookbackDays<=0.
func NewMirrorNegatives(mirror MirrorReader, res m04interest.EntityResolver, lookbackDays int) *MirrorNegatives {
	if lookbackDays <= 0 {
		lookbackDays = 30
	}
	return &MirrorNegatives{mirror: mirror, res: res, lookbackDays: lookbackDays}
}

// Negatives returns the skip records touched after `since`.
//
// # The signal value is CUMULATIVE
//
// The mirror keeps a SNAPSHOT: `skip_count` is a running total for the
// (uid, content, day) triple, not a delta. So the returned Value is THAT DAY's
// cumulative skip count, and this source CANNOT GIVE the "every interaction is
// counted exactly once" GUARANTEE: if the record is touched again within the same day
// (a new skip, or an unrelated dwell advances `last_ts_ms`) the day's
// total is added AGAIN on the next run. The exactly-once contract of the positive source (ring
// buffer) does NOT hold here.
//
// The fetch window is derived from `since`; so this double counting stays bounded to same-day
// records, and in steady state the number of records scanned per user
// drops to ~1/30 (an unconditional 30-day scan for 31k users meant
// ~4.8M HGETALL per run).
func (n *MirrorNegatives) Negatives(ctx context.Context, userID string, since time.Time) ([]m04interest.Signal, error) {
	records, err := n.mirror.FetchUserAnalytics(ctx, userID, n.fetchDays(time.Now(), since))
	if err != nil {
		return nil, err
	}

	type entry struct {
		count int64
		at    time.Time
	}
	fresh := make([]entry, 0, len(records))
	ids := make([]string, 0, len(records))
	for _, r := range records {
		if r.ContentID == "" || r.Skip == nil || r.Skip.Count <= 0 {
			continue
		}
		at, ok := recordAt(r)
		if !ok {
			m04interest.NegativeRecordsDropped.WithLabelValues("bad_day").Inc()
			continue
		}
		if !since.IsZero() && !at.After(since) {
			continue
		}
		fresh = append(fresh, entry{count: r.Skip.Count, at: at})
		ids = append(ids, r.ContentID)
	}
	if len(fresh) == 0 {
		return nil, nil
	}

	entities, err := n.res.EntitiesOf(ctx, ids)
	if err != nil {
		return nil, err
	}

	out := make([]m04interest.Signal, 0, len(fresh))
	for i, e := range fresh {
		ents, ok := entities[ids[i]]
		if !ok || len(ents) == 0 {
			continue
		}
		out = append(out, m04interest.Signal{Entities: ents, Value: float64(e.count), At: e.at})
	}
	return out, nil
}

// fetchDays derives the number of days to fetch from the mirror from `since`.
//
// When `since` is zero (first run) the full lookback is scanned; afterwards only
// the time since the last write + a 2-day safety margin. The margin covers two
// things: partial days (half a day = 0 days) and the drift between the mirror's UTC day
// boundaries and the run time.
func (n *MirrorNegatives) fetchDays(now, since time.Time) int {
	if since.IsZero() {
		return n.lookbackDays
	}
	d := int(now.Sub(since).Hours()/24) + 2
	if d < 1 {
		d = 1 // a future-dated `since` (clock skew) → at least today
	}
	if d > n.lookbackDays {
		return n.lookbackDays
	}
	return d
}

// recordAt produces the time to use for the record at the gate. ok=false → the record
// cannot be timed and MUST BE SKIPPED.
//
// Priority is `last_ts_ms`. If it is missing (some records have null/missing in JSON),
// the END of the record's OWN DAY (UTC) is used.
//
// Here there used to be time.Now(), and it was an accumulation bug: a record without
// a timestamp satisfied `at.After(since)` ON EVERY RUN, so that
// record's cumulative skip count was re-added on every run (with a daily cron
// + 90-day permanent half-life the weight converged to ~130×count).
// For the same reason the empty-run gate was also pierced, and the dormant profile's 30-day
// TTL stayed fresh forever. When the record's day is used, the record ages out naturally
// instead of re-firing through the gate.
func recordAt(r analytics.AnalyticRecord) (time.Time, bool) {
	if r.LastTsMs > 0 {
		return time.UnixMilli(r.LastTsMs), true
	}
	day, err := time.Parse(dayLayout, r.Day)
	if err != nil {
		return time.Time{}, false
	}
	// Day END: a job running within the same day must be able to
	// see its own day's record. Falling into the future is not a problem in decay —
	// m04interest.AddSignals clamps negative age to 0.
	return day.Add(24*time.Hour - time.Nanosecond), true
}
