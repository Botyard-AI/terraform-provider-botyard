package provider

import (
	"testing"
)

// TestRFC3339InstantCheck pins the anchor grammar: strict RFC 3339 (not Go's
// more lenient time.Parse), and at most microsecond precision, judged on the
// digits as written.
func TestRFC3339InstantCheck(t *testing.T) {
	check := rfc3339Instant().(stringCheck).check
	accept := []string{
		"2026-01-01T09:00:00Z",
		"2026-01-01T09:00:00+00:00",
		"2026-01-01T09:00:00-05:30",
		"2026-01-01T09:00:00.1Z",
		"2026-01-01T09:00:00.123456Z",
		"2026-01-01T09:00:00.1234560Z",      // trailing zeros only
		"2026-01-01T09:00:00.123456000000Z", // past nine digits, all zero
		"2026-01-01T09:00:00.123456+02:00",
	}
	reject := map[string]string{
		"2026-01-01T09:00:00.1234567Z":    "sub-microsecond",
		"2026-01-01T09:00:00.123456789Z":  "sub-microsecond",
		"2026-01-01T09:00:00.1234560001Z": "sub-microsecond past nine digits (Go parses it to .123456)",
		"2026-01-01T09:00:00,123456789Z":  "comma fraction (Go accepts it)",
		"2026-01-01T09:00:00,123456Z":     "comma fraction, microsecond",
		"2026-01-01T9:00:00.123456789Z":   "single-digit hour (Go accepts it)",
		"2026-01-01T9:00:00Z":             "single-digit hour",
		"2026-01-01T09:00:00":             "no offset",
		"2026-01-01T09:00:00.Z":           "empty fraction",
		"2026-01-01t09:00:00z":            "lowercase separators",
		"2026-01-01 09:00:00Z":            "space separator",
		"2026-01-01T09:00:00+0100":        "offset without colon",
		"2026-13-01T09:00:00Z":            "month out of range",
		"2026-01-01T25:00:00Z":            "hour out of range",
		"２０２６-01-01T09:00:00Z":            "non-ASCII digits",
	}
	for _, s := range accept {
		if reason := check(s); reason != "" {
			t.Errorf("%q refused: %s", s, reason)
		}
	}
	for s, why := range reject {
		if check(s) == "" {
			t.Errorf("%q accepted, want refused (%s)", s, why)
		}
	}
}
