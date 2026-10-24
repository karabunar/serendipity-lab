package envutil

import (
	"testing"
	"time"
)

func TestDuration(t *testing.T) {
	tests := []struct {
		name   string
		env    string
		def    time.Duration
		expect time.Duration
	}{
		{"unset", "", 5 * time.Second, 5 * time.Second},
		{"valid", "10s", 5 * time.Second, 10 * time.Second},
		{"invalid", "potato", 5 * time.Second, 5 * time.Second},
		{"zero", "0s", 5 * time.Second, 5 * time.Second},
		{"negative", "-1s", 5 * time.Second, 5 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TEST_DUR", tc.env)
			got := Duration("TEST_DUR", tc.def)
			if got != tc.expect {
				t.Errorf("got %v, want %v", got, tc.expect)
			}
		})
	}
}

func TestInt(t *testing.T) {
	tests := []struct {
		name   string
		env    string
		def    int
		expect int
	}{
		{"unset", "", 5, 5},
		{"valid", "10", 5, 10},
		{"zero", "0", 5, 0}, // 0 is valid — meaningful for some flags
		{"negative", "-1", 5, 5},
		{"invalid", "abc", 5, 5},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TEST_INT", tc.env)
			got := Int("TEST_INT", tc.def)
			if got != tc.expect {
				t.Errorf("got %v, want %v", got, tc.expect)
			}
		})
	}
}

func TestFloat(t *testing.T) {
	tests := []struct {
		name   string
		env    string
		def    float64
		expect float64
	}{
		{"unset", "", 1.5, 1.5},
		{"valid", "2.5", 1.5, 2.5},
		{"zero", "0", 1.5, 1.5},
		{"negative", "-1", 1.5, 1.5},
		{"invalid", "x", 1.5, 1.5},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TEST_F", tc.env)
			got := Float("TEST_F", tc.def)
			if got != tc.expect {
				t.Errorf("got %v, want %v", got, tc.expect)
			}
		})
	}
}

func TestString(t *testing.T) {
	t.Setenv("TEST_STR_KEY", "hello")
	if got := String("TEST_STR_KEY", "def"); got != "hello" {
		t.Fatalf("want 'hello', got: %q", got)
	}
	t.Setenv("TEST_STR_KEY", "")
	if got := String("TEST_STR_KEY", "def"); got != "def" {
		t.Fatalf("want 'def', got: %q", got)
	}
}

func TestBool(t *testing.T) {
	tests := []struct {
		name   string
		env    string
		def    bool
		expect bool
	}{
		{"unset_def_false", "", false, false},
		{"unset_def_true", "", true, true},
		{"true_lower", "true", false, true},
		{"one", "1", false, true},
		{"false_explicit", "false", true, false},
		{"random", "xyz", true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TEST_B", tc.env)
			got := Bool("TEST_B", tc.def)
			if got != tc.expect {
				t.Errorf("got %v, want %v", got, tc.expect)
			}
		})
	}
}

// FloatNonNegative: zero is explicitly KEPT (f < 0 condition, not f <= 0).
// Critical difference from Float: "0" does not fall back to the default. Critical difference from FloatAllowZero:
// a negative value is rejected.
//
// This helper used to live in two services separately as `envFloatOr`
// (a setup other services inherited);
// it was merged here when feedback.BuildProcessorFromEnv became shared.
func TestFloatNonNegative(t *testing.T) {
	cases := []struct {
		name   string
		envVal string
		def    float64
		want   float64
	}{
		{"bos_default_doner", "", 1.0, 1.0},
		{"gecerli_pozitif", "0.7", 1.0, 0.7},
		{"bozuk_string_default_doner", "notafloat", 1.0, 1.0},
		{"negatif_default_doner", "-1.5", 1.0, 1.0},
		{"sifir_korunur", "0", 1.0, 0.0},
		{"sifir_ondalikli_korunur", "0.0", 1.0, 0.0},
		{"buyuk_gecerli", "999.9", 1.0, 999.9},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := "TEST_FLOAT_NONNEG_" + tc.name
			if tc.envVal != "" {
				t.Setenv(key, tc.envVal)
			}
			if got := FloatNonNegative(key, tc.def); got != tc.want {
				t.Errorf("FloatNonNegative(%q, %v) = %v, want %v", tc.envVal, tc.def, got, tc.want)
			}
		})
	}
}

// The three float semantics must stay DISTINCT from each other: two parsers that interpret the same env
// differently split the writer/reader sides (precedent: envBoolOr vs envutil.Bool,
// IMPRESSION_WRITE_ENABLED="TRUE").
func TestFloatSemantics_AreDistinct(t *testing.T) {
	t.Setenv("TEST_FLOAT_SEMANTICS", "0")
	if got := Float("TEST_FLOAT_SEMANTICS", 5); got != 5 {
		t.Errorf("Float must reset zero to default: got %v", got)
	}
	if got := FloatAllowZero("TEST_FLOAT_SEMANTICS", 5); got != 0 {
		t.Errorf("FloatAllowZero must keep zero: got %v", got)
	}
	if got := FloatNonNegative("TEST_FLOAT_SEMANTICS", 5); got != 0 {
		t.Errorf("FloatNonNegative must keep zero: got %v", got)
	}

	t.Setenv("TEST_FLOAT_SEMANTICS", "-2")
	if got := FloatAllowZero("TEST_FLOAT_SEMANTICS", 5); got != -2 {
		t.Errorf("FloatAllowZero must keep negative: got %v", got)
	}
	if got := FloatNonNegative("TEST_FLOAT_SEMANTICS", 5); got != 5 {
		t.Errorf("FloatNonNegative must reject negative: got %v", got)
	}
}

// IntAllowNegative is for settings where a negative value has a MEANING. Int drops a negative to
// def; this SILENTLY reverses a documented semantics like "-1 = unlimited"
// (the operator wants unlimited and gets a cap).
// Same family: FloatAllowZero / FloatNonNegative.
func TestIntAllowNegative(t *testing.T) {
	tests := []struct {
		name   string
		set    bool
		val    string
		def    int
		expect int
	}{
		{"unset", false, "", 4, 4},
		{"pozitif", true, "7", 4, 7},
		{"sifir", true, "0", 4, 0},
		{"negatif korunur", true, "-1", 4, -1},
		{"parse hatasi -> def", true, "potato", 4, 4},
		{"bos -> def", true, "", 4, 4},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("SERENDIPITY_TEST_INT_NEG", tc.val)
			}
			if got := IntAllowNegative("SERENDIPITY_TEST_INT_NEG", tc.def); got != tc.expect {
				t.Fatalf("IntAllowNegative()=%d, want %d", got, tc.expect)
			}
		})
	}
}

// DurationNonNegative: zero is explicitly KEPT (d < 0 condition, not d <= 0).
// Critical difference from Duration: "0" does NOT fall back to the default, so for a duration setting that carries
// "0 = off" semantics the single-switch rollback actually works.
//
// ⚠️ WHY IT EXISTS: `FEED_GEN_PREWARM_DEDUP_TTL=0` was documented in code as
// "guard disabled (rollback)" but was UNREACHABLE because of `Duration`'s
// "invalid OR <=0 -> def" contract: "0" is parsed without error and returns 0, the `d <= 0` branch turns it into the default (6h)
// and the guard IS INSTALLED. The defect was not inert but dangerous — the operator, thinking they had
// turned the guard off, would drop the dedup TTL from 40h to 6h and open an
// amplification loop in the prewarm queue.
// amplifikasyon dongusunu acardi.
// Same family: FloatAllowZero / FloatNonNegative / IntAllowNegative.
func TestDurationNonNegative(t *testing.T) {
	cases := []struct {
		name   string
		envVal string
		def    time.Duration
		want   time.Duration
	}{
		{"bos_default_doner", "", 6 * time.Hour, 6 * time.Hour},
		{"gecerli_pozitif", "40h", 6 * time.Hour, 40 * time.Hour},
		{"bozuk_string_default_doner", "notaduration", 6 * time.Hour, 6 * time.Hour},
		{"negatif_default_doner", "-1h", 6 * time.Hour, 6 * time.Hour},
		{"sifir_korunur", "0", 6 * time.Hour, 0},
		{"sifir_saniye_korunur", "0s", 6 * time.Hour, 0},
		{"sifir_saat_korunur", "0h", 6 * time.Hour, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := "TEST_DUR_NONNEG_" + tc.name
			if tc.envVal != "" {
				t.Setenv(key, tc.envVal)
			}
			if got := DurationNonNegative(key, tc.def); got != tc.want {
				t.Errorf("DurationNonNegative(%q, %v) = %v, want %v", tc.envVal, tc.def, got, tc.want)
			}
		})
	}
}

// TestDurationSemantics_DisablePathEXISTS pins that the two semantics are DISTINCT and that the
// "0 = off" path REALLY exists.
//
// ⚠️ This test does not FORBID THE WRONG, it ENFORCES THE RIGHT: it directly measures the claim "there EXISTS a
// value that turns the guard off and that value is named 0". A forbidding
// sentinel (e.g. "Duration must not be called") would FAIL even on a comment
// explaining the same claim.
func TestDurationSemantics_DisablePathEXISTS(t *testing.T) {
	const key = "TEST_DUR_SEMANTICS"

	t.Setenv(key, "0")
	if got := Duration(key, 6*time.Hour); got != 6*time.Hour {
		t.Errorf("Duration must reset zero to default (current contract): got %v", got)
	}
	if got := DurationNonNegative(key, 6*time.Hour); got != 0 {
		t.Errorf("DurationNonNegative must KEEP zero — otherwise there is no way to disable "+
			"tek-anahtarli rollback ulasilamaz: got %v", got)
	}

	t.Setenv(key, "-1h")
	if got := DurationNonNegative(key, 6*time.Hour); got != 6*time.Hour {
		t.Errorf("DurationNonNegative must reject negative (bad config -> safe default): got %v", got)
	}
}
