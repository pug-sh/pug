// Package tzx centralizes IANA timezone validation, normalization, and location
// loading shared across project creation (where a reporting zone is captured and
// stored) and the insights/dashboards query paths (where it aligns time-bucket
// boundaries and preset windows to a viewer's local calendar).
//
// The empty string and the literal "UTC" are treated identically as "UTC bucketing"
// — the historical default — so callers can store/compare a single canonical zero
// value ("") and keep byte-identical SQL and rollup eligibility for UTC.
package tzx

import (
	"fmt"
	"regexp"
	"time"
)

// nameRe restricts a zone name to the IANA charset. It is the injection guard for
// embedding a stored value directly in a ClickHouse toTimeZone() literal: a string
// matching this pattern cannot contain a quote or SQL metacharacter. It is
// deliberately simpler than full IANA grammar but admits every real zone name
// (e.g. "Asia/Kolkata", "America/Argentina/Buenos_Aires", "Etc/GMT+5").
var nameRe = regexp.MustCompile(`^[A-Za-z0-9_+/-]+$`)

// maxNameLen caps the stored/validated length, matching the proto string.max_len
// and the projects.reporting_timezone column width.
const maxNameLen = 64

// IsUTC reports whether name means "UTC bucketing": empty or the literal "UTC".
func IsUTC(name string) bool {
	return name == "" || name == "UTC"
}

// ValidChars reports whether name is non-empty and matches the injection-safe IANA
// charset. It does not check that the zone actually exists.
func ValidChars(name string) bool {
	return len(name) <= maxNameLen && nameRe.MatchString(name)
}

// Normalize collapses the two UTC spellings to "" and otherwise returns name
// unchanged. It does not validate; pair with Validate when the input is untrusted.
func Normalize(name string) string {
	if IsUTC(name) {
		return ""
	}
	return name
}

// Validate accepts ""/"UTC" (UTC) and otherwise requires the injection-safe charset
// and a loadable location. ClickHouse carries its own tz database, so a name accepted
// here but unknown there would surface as a query error at execution rather than here.
// It is Load with the resolved location discarded — the two share one validation ladder.
func Validate(name string) error {
	_, err := Load(name)
	return err
}

// Coerce returns the normalized name when it validates, otherwise "" (UTC). Use at
// lenient capture points (project creation, signup) where a malformed client value
// must never fail the operation — it silently falls back to UTC.
func Coerce(name string) string {
	if Validate(name) != nil {
		return ""
	}
	return Normalize(name)
}

// Load resolves name to a *time.Location for window alignment. ""/"UTC" yield
// time.UTC; any other value must validate. A malformed/unknown zone is an error.
func Load(name string) (*time.Location, error) {
	if IsUTC(name) {
		return time.UTC, nil
	}
	if !ValidChars(name) {
		return nil, fmt.Errorf("invalid timezone %q", name)
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("invalid timezone %q: %w", name, err)
	}
	return loc, nil
}
