package canonicaldict

import (
	"testing"

	"serendipity/internal/trfold"
)

// The golden parity test moved to internal/trfold/fold_test.go (single
// source). Only a "we delegate" smoke test remains here: it catches it if a copy table is
// dropped into Fold's body again.
func TestFold_DelegatesToTrfold(t *testing.T) {
	for _, in := range []string{"Fenerbahçe", "İstanbul", "IĞDIR", "Ç-Ğ-I-İ-Ö-Ş-Ü", ""} {
		if got, want := Fold(in), trfold.Fold(in); got != want {
			t.Fatalf("Fold(%q) = %q, trfold.Fold %q — delegation broken", in, got, want)
		}
	}
}
