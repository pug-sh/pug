package insights

import (
	"fmt"

	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
	"github.com/pug-sh/pug/internal/tzx"
)

// utcBucketing reports whether tz means "bucket on UTC boundaries" — the historical
// default. Empty and the literal "UTC" both qualify, so they keep byte-identical SQL
// (no toTimeZone wrapper) and stay eligible for the UTC-keyed daily rollup.
func utcBucketing(tz string) bool {
	return tzx.IsUTC(tz)
}

// bucketExpr returns the ClickHouse time-bucket expression for column at the
// request's granularity. When tz selects a non-UTC zone the column is wrapped in
// toTimeZone() so every toStartOf* function truncates in that zone — this aligns
// day/week/month buckets (and hour buckets in fractional-offset zones) to the
// viewer's local calendar uniformly, with no per-function or week-mode special
// casing, and returns the bucket's start instant. tz must already be charset- and
// existence-validated by the caller (ExecuteQuery runs tzx.Validate up front), which
// is what makes embedding it in the toTimeZone() literal injection-safe.
func bucketExpr(g insightsv1.Granularity, column, tz string) (string, error) {
	fn, err := granularityFunc(g)
	if err != nil {
		return "", err
	}
	if utcBucketing(tz) {
		return fmt.Sprintf("%s(%s)", fn, column), nil
	}
	return fmt.Sprintf("%s(toTimeZone(%s, '%s'))", fn, column, tz), nil
}
