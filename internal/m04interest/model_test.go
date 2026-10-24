package m04interest

import (
	"encoding/json"
	"testing"
	"time"
)

func TestProfile_JSONFieldNamesAreShort(t *testing.T) {
	// a ~1-2 KB budget does not tolerate long field names.
	p := Profile{
		Version:    SchemaVersion,
		UpdatedAt:  time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC),
		Stable:     Weights{"Fenerbahçe": 8.4},
		Recent:     Weights{"transfer": 2.2},
		Negative:   Weights{"Galatasaray": 2.7},
		Query:      []string{"Fenerbahçe"},
		Confidence: Confidence{Interactions: 143, Concentration: 0.62},
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var back Profile
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.Stable["Fenerbahçe"] != 8.4 || back.Query[0] != "Fenerbahçe" {
		t.Fatalf("round-trip broken: %+v", back)
	}
	if !back.UpdatedAt.Equal(p.UpdatedAt) {
		t.Fatalf("UpdatedAt = %v", back.UpdatedAt)
	}
}

func TestProfile_IsZero(t *testing.T) {
	if !(Profile{}).IsZero() {
		t.Fatal("empty profile must be IsZero")
	}
	if (Profile{UpdatedAt: time.Now()}).IsZero() {
		t.Fatal("profile with UpdatedAt set must not be IsZero")
	}
}

func TestProfile_EmptyMapsAreNotNullInJSON(t *testing.T) {
	// So the consumer (Phase 3) does not have to do a nil map check.
	raw, err := json.Marshal(NewProfile())
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, bad := range []string{`"s":null`, `"r":null`, `"n":null`, `"q":null`} {
		if contains(s, bad) {
			t.Fatalf("%s present in JSON: %s", bad, s)
		}
	}
}

func contains(h, n string) bool {
	return len(h) >= len(n) && (func() bool {
		for i := 0; i+len(n) <= len(h); i++ {
			if h[i:i+len(n)] == n {
				return true
			}
		}
		return false
	})()
}
