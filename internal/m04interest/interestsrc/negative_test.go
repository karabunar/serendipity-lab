package interestsrc

import (
	"context"
	"testing"
	"time"

	"serendipity/internal/analytics"
	"serendipity/internal/m04interest"
)

type fakeMirror struct {
	records []analytics.AnalyticRecord
	days    int
}

func (f *fakeMirror) FetchUserAnalytics(_ context.Context, _ string, days int) ([]analytics.AnalyticRecord, error) {
	f.days = days
	return f.records, nil
}

func TestMirrorNegatives_SkipCountBecomesWeight(t *testing.T) {
	now := time.Now()
	m := &fakeMirror{records: []analytics.AnalyticRecord{
		{ContentID: "p1", LastTsMs: now.UnixMilli(), Skip: &analytics.SkipAnalytics{Count: 3}},
	}}
	sigs, err := NewMirrorNegatives(m, fakeResolver{"p1": {"Galatasaray"}}, 30).Negatives(context.Background(), "u1", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sigs) != 1 || sigs[0].Entities[0] != "Galatasaray" || sigs[0].Value != 3 {
		t.Fatalf("sigs = %+v", sigs)
	}
}

func TestMirrorNegatives_RecordWithoutSkipSkipped(t *testing.T) {
	now := time.Now()
	m := &fakeMirror{records: []analytics.AnalyticRecord{
		{ContentID: "p1", LastTsMs: now.UnixMilli()},                                           // Skip nil
		{ContentID: "p2", LastTsMs: now.UnixMilli(), Skip: &analytics.SkipAnalytics{Count: 0}}, // zero
	}}
	sigs, err := NewMirrorNegatives(m, fakeResolver{"p1": {"A"}, "p2": {"B"}}, 30).Negatives(context.Background(), "u1", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sigs) != 0 {
		t.Fatalf("sigs = %+v", sigs)
	}
}

func TestMirrorNegatives_BeforeSinceSkipped(t *testing.T) {
	now := time.Now()
	m := &fakeMirror{records: []analytics.AnalyticRecord{
		{ContentID: "eski", LastTsMs: now.Add(-48 * time.Hour).UnixMilli(), Skip: &analytics.SkipAnalytics{Count: 5}},
		{ContentID: "yeni", LastTsMs: now.UnixMilli(), Skip: &analytics.SkipAnalytics{Count: 1}},
	}}
	res := fakeResolver{"eski": {"A"}, "yeni": {"B"}}
	sigs, err := NewMirrorNegatives(m, res, 30).Negatives(context.Background(), "u1", now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(sigs) != 1 || sigs[0].Entities[0] != "B" {
		t.Fatalf("sigs = %+v", sigs)
	}
}

func TestMirrorNegatives_TimelessRecordUsesITSOWNDAY(t *testing.T) {
	// There are records with LastTsMs=0 (null/missing in JSON). They used to be
	// counted as "now" — which meant passing the gate ON EVERY RUN and
	// the record's cumulative skip count being added over and over. Now
	// the record's OWN DAY (UTC end of day) is used: a fresh day passes, an old
	// day is dropped at the gate.
	now := time.Now().UTC()
	res := fakeResolver{"p1": {"A"}}

	for _, tc := range []struct {
		name  string
		day   string
		since time.Time
		want  int
	}{
		{"bugunun kaydi gecer", now.Format("2006-01-02"), now.Add(-time.Hour), 1},
		{"eski gun kapida elenir", now.AddDate(0, 0, -5).Format("2006-01-02"), now.Add(-time.Hour), 0},
		{"since sifirken gecer", now.AddDate(0, 0, -5).Format("2006-01-02"), time.Time{}, 1},
	} {
		m := &fakeMirror{records: []analytics.AnalyticRecord{
			{ContentID: "p1", Day: tc.day, LastTsMs: 0, Skip: &analytics.SkipAnalytics{Count: 2}},
		}}
		sigs, err := NewMirrorNegatives(m, res, 30).Negatives(context.Background(), "u1", tc.since)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(sigs) != tc.want {
			t.Fatalf("%s: len(sigs) = %d, want %d", tc.name, len(sigs), tc.want)
		}
	}
}

func TestMirrorNegatives_TimelessRecordDoesNOTGiveSameSignalTWICE(t *testing.T) {
	// Lock for the accumulation regression: if the daily cron re-added the same record
	// on every run, with a 90-day half-life the weight would
	// converge to ~130×count. On the second run `since` has advanced → the record
	// must no longer pass the gate.
	now := time.Now().UTC()
	rec := analytics.AnalyticRecord{
		ContentID: "p1",
		Day:       now.AddDate(0, 0, -1).Format("2006-01-02"),
		Skip:      &analytics.SkipAnalytics{Count: 4},
	}
	n := NewMirrorNegatives(&fakeMirror{records: []analytics.AnalyticRecord{rec}}, fakeResolver{"p1": {"A"}}, 30)

	first, err := n.Negatives(context.Background(), "u1", now.Add(-48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 {
		t.Fatalf("no signal on first run: %+v", first)
	}

	// The first run wrote the profile → the next `since` = that write moment.
	second, err := n.Negatives(context.Background(), "u1", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 0 {
		t.Fatalf("same record added a second time: %+v", second)
	}
}

func TestMirrorNegatives_UnparsableDaySkipped(t *testing.T) {
	m := &fakeMirror{records: []analytics.AnalyticRecord{
		{ContentID: "p1", Day: "bozuk", Skip: &analytics.SkipAnalytics{Count: 9}},
		{ContentID: "p2", Day: "", Skip: &analytics.SkipAnalytics{Count: 9}},
	}}
	sigs, err := NewMirrorNegatives(m, fakeResolver{"p1": {"A"}, "p2": {"B"}}, 30).Negatives(
		context.Background(), "u1", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(sigs) != 0 {
		t.Fatalf("untimeable record was processed: %+v", sigs)
	}
}

func TestMirrorNegatives_LookbackPassedOn(t *testing.T) {
	m := &fakeMirror{}
	if _, err := NewMirrorNegatives(m, fakeResolver{}, 45).Negatives(context.Background(), "u1", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if m.days != 45 {
		t.Fatalf("days = %d, want 45", m.days)
	}
}

func TestMirrorNegatives_FetchWindowDerivedFromSince(t *testing.T) {
	// I6: an unconditional 30-day scan meant ~4.8M
	// HGETALL per run for 31k users. In steady state (daily cron) the window drops to ~2 days.
	now := time.Now()
	for _, tc := range []struct {
		name  string
		since time.Time
		want  int
	}{
		{"sifir since → tam lookback", time.Time{}, 30},
		{"gunluk cron", now.Add(-24 * time.Hour), 3},
		{"taze yazim", now.Add(-time.Hour), 2},
		{"cok eski profil lookback'i asamaz", now.Add(-90 * 24 * time.Hour), 30},
		{"gelecek tarihli since en az 1 gun", now.Add(72 * time.Hour), 1},
	} {
		m := &fakeMirror{}
		if _, err := NewMirrorNegatives(m, fakeResolver{}, 30).Negatives(context.Background(), "u1", tc.since); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if m.days != tc.want {
			t.Fatalf("%s: days = %d, want %d", tc.name, m.days, tc.want)
		}
	}
}

var _ m04interest.NegativeSource = (*MirrorNegatives)(nil)
