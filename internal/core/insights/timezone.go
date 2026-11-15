package insights

import (
	"fmt"

	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
	"github.com/pug-sh/pug/internal/tzx"
)

// utcBucketing reports whether tz selects SQL that names no zone ("" or "UTC"), the
// only form the trends/segmentation rollup serves. That SQL buckets in the ClickHouse
// server's zone, which need not be UTC.
func utcBucketing(tz string) bool {
	return tzx.IsUTC(tz)
}

// bucketExpr returns the ClickHouse time-bucket expression for column at the
// request's granularity. When tz names a zone (StoredUTC included) the column is wrapped
// in toTimeZone() so every toStartOf* function truncates in that zone: this aligns
// day/week/month buckets (and hour buckets in fractional-offset zones) to the
// project's local calendar uniformly, with no per-function or week-mode special
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
