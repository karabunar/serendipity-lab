package trfold

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// goldenPath is the fixture SHARED with the Python side. The path leaves the module: the script that produces the dictionary is not in the Go module, it is under
// python/.
//
// This file is the SOLE owner of the golden parity test: canonicaldict and
// postentity no longer carry their own copies but call Fold here, so
// the tests there are only "we delegate" smoke tests.
const goldenPath = "../../python/canonical_dict/testdata/fold_golden.json"

type goldenCase struct {
	In  string `json:"in"`
	Out string `json:"out"`
}

func loadGolden(t *testing.T) []goldenCase {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(goldenPath))
	if err != nil {
		t.Fatalf("could not read golden fixture (%s): %v", goldenPath, err)
	}
	var cases []goldenCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("golden fixture empty")
	}
	return cases
}

func TestFold_PythonParity(t *testing.T) {
	for _, c := range loadGolden(t) {
		if got := Fold(c.In); got != c.Out {
			t.Errorf("Fold(%q) = %q, Python produces %q — the two sides DIVERGED", c.In, got, c.Out)
		}
	}
}

func TestFold_CapitalIDoesNotProduceCombiningDot(t *testing.T) {
	// If the order were reversed ("ToLower first, then folding") Go and Python would
	// diverge on the letter "İ".
	if got := Fold("İ"); got != "i" {
		t.Fatalf("Fold(\"İ\") = %q, want \"i\"", got)
	}
}

func TestFold_DotlessAndDottedIFoldToSamePlace(t *testing.T) {
	// Distinct letters in Turkish; deliberately merged for dictionary lookup,
	// because on social media the two are used interchangeably.
	if Fold("ısparta") != Fold("isparta") || Fold("isparta") != "isparta" {
		t.Fatalf("Fold(ısparta)=%q Fold(isparta)=%q", Fold("ısparta"), Fold("isparta"))
	}
}

func TestFold_Idempotent(t *testing.T) {
	for _, c := range loadGolden(t) {
		once := Fold(c.In)
		if twice := Fold(once); twice != once {
			t.Errorf("Fold not idempotent: %q → %q → %q", c.In, once, twice)
		}
	}
}
