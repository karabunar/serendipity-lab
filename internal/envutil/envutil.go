// Package envutil holds small, zero-dependency helpers for reading environment
// variables with safe defaults.
//
// Previous sprawl:
//   - copies of envDuration/envInt/envFloat scattered across services' main.go files
//
// Three copies, all carrying the same semantics. This package is the single source.
//
// Contract: if the env variable is missing, empty, or cannot be parsed, `def` is returned.
// Negative values (Int) and zero/negative Duration / Float fall back to def; with a
// bad configuration the caller always sees a "valid default", never
// half-broken values.
package envutil

import (
	"os"
	"strconv"
	"time"
)

// Duration reads the env with time.ParseDuration. An invalid or <=0 value falls back to def.
func Duration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

// DurationNonNegative reads the env with time.ParseDuration; ZERO IS KEPT but
// negative values fall back to def. The critical difference from Duration: `0` does NOT
// fall back to the default, so for duration settings that carry "0 = off" semantics
// the off switch actually works.
//
// Duration is the WRONG tool for this and is silently wrong: its contract is
// "invalid OR <=0 → def", so even though `time.ParseDuration("0")` returns `0` without error,
// the result becomes the default. That makes a documented "write 0 to turn it off"
// rollback UNREACHABLE, and the operator thinks they turned it off while having turned the setting
// into the default.
//
// ⚠️ Concrete cost: `FEED_GEN_PREWARM_DEDUP_TTL=0`
// was documented as the single-switch rollback of the prewarm dedup guard, but
// `Duration` turned it into 6h and the guard WAS INSTALLED. With a long queue wait,
// "turning it off" drops the value to 6h, the wait
// exceeds the marker lifetime and the dedup guard turns into an AMPLIFIER —
// that was exactly the root cause of the queue incident.
//
// Same family: FloatAllowZero / FloatNonNegative / IntAllowNegative —
// the reading side picks the semantics.
func DurationNonNegative(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return def
	}
	return d
}

// Int reads the env with Atoi. A negative value or parse error falls back to def.
func Int(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return def
	}
	return n
}

// IntAllowNegative is for settings where a negative value has a MEANING (e.g.
// "-1 = unlimited"). Only a parse error and an empty value fall back to def.
//
// A separate function from Int: Int's fallback of negatives to def is the right
// guard for most settings (a negative cap is a broken config). But when a negative has
// a documented meaning, the same guard SILENTLY reverses that meaning
// — the operator wants "unlimited" and gets a cap. Same family as FloatAllowZero / FloatNonNegative:
// the reading side picks the semantics.
func IntAllowNegative(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// Float reads the env with ParseFloat. Zero/negative or a parse error falls back to def.
func Float(key string, def float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f <= 0 {
		return def
	}
	return f
}

// FloatAllowZero reads the env with ParseFloat but does not fall
// zero/negative values back to def — only env missing/empty or a parse error → def. Used for parameters like bonus/weight
// that need "0 = off" semantics.
// (The standard Float is designed for parameters that must be positive, such as
// timeout/limit/capacity.)
func FloatAllowZero(key string, def float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

// FloatNonNegative reads the env with ParseFloat; zero is KEPT but negative
// values fall back to def. The third semantics between Float and FloatAllowZero:
// "0 is a meaningful setting, negative is a broken config".
//
// Used by: THOMPSON_SOFTCAP_MAX_TOTAL (0 = softcap off),
// THOMPSON_FEEDBACK_VIDEO_COMPLETION_SCALE. The local `envFloatOr` copies written separately in two
// services were merged here — two parsers that interpret the same
// flag differently split the writer/reader sides.
func FloatNonNegative(key string, def float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 {
		return def
	}
	return f
}

// String reads the given env variable; returns def if it is empty.
func String(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Bool reads the env as "true"/"1".
//
// ⚠️ `def` is applied ONLY to an EMPTY/UNSET value. Every unrecognized NON-empty value
// (`TRUE`, `True`, `yes`, `on`, `evet`) returns `false` — even if called
// with def=true. The old doc line said "everything else returns def" and was WRONG;
// a safety flag (BACKFILL_LEASE_RENEW_ENABLED) relied on that wrong premise
// (audit D1).
//
// ⚠️ The behavior itself is also questionable: "typo → silently false" is the WRONG direction
// for flags that turn off a guard. NOT CHANGED because it has many callers
// across the repo and a behavior change is a separate job; flags that carry guard
// semantics should build their own explicit switch instead of this function
// (example: the lock settings reader, which builds its own switch).
func Bool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	return v == "true" || v == "1"
}
