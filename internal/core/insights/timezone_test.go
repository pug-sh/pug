package insights

import (
	"strings"
	"testing"

	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
)

func TestBucketExpr_UTCIsUnwrapped(t *testing.T) {
	for _, tz := range []string{"", "UTC"} {
		got, err := bucketExpr(insightsv1.Granularity_GRANULARITY_DAY, "occur_time", tz)
		if err != nil {
			t.Fatalf("bucketExpr(%q) error: %v", tz, err)
		}
		if got != "toStartOfDay(occur_time)" {
			t.Errorf("bucketExpr(%q) = %q, want plain toStartOfDay(occur_time)", tz, got)
		}
		if strings.Contains(got, "toTimeZone") {
			t.Errorf("bucketExpr(%q) wrapped in toTimeZone, want UTC unwrapped", tz)
		}
	}
}

func TestBucketExpr_NonUTCWrapsInToTimeZone(t *testing.T) {
	got, err := bucketExpr(insightsv1.Granularity_GRANULARITY_DAY, "occur_time", "Asia/Kolkata")
	if err != nil {
		t.Fatalf("bucketExpr error: %v", err)
	}
	want := "toStartOfDay(toTimeZone(occur_time, 'Asia/Kolkata'))"
	if got != want {
		t.Errorf("bucketExpr = %q, want %q", got, want)
	}
}

// Every granularity wraps the column in toTimeZone() the same way — the point of the
// uniform template is that week/month/hour/minute all truncate in-zone with no
// per-function special casing.
func TestBucketExpr_NonUTCWrapsEveryGranularity(t *testing.T) {
	cases := []struct {
		gran insightsv1.Granularity
		fn   string
	}{
		{insightsv1.Granularity_GRANULARITY_MINUTE, "toStartOfMinute"},
		{insightsv1.Granularity_GRANULARITY_HOUR, "toStartOfHour"},
		{insightsv1.Granularity_GRANULARITY_DAY, "toStartOfDay"},
		{insightsv1.Granularity_GRANULARITY_WEEK, "toStartOfWeek"},
		{insightsv1.Granularity_GRANULARITY_MONTH, "toStartOfMonth"},
	}
	for _, c := range cases {
		got, err := bucketExpr(c.gran, "occur_time", "Asia/Kolkata")
		if err != nil {
			t.Fatalf("bucketExpr(%v) error: %v", c.gran, err)
		}
		want := c.fn + "(toTimeZone(occur_time, 'Asia/Kolkata'))"
		if got != want {
			t.Errorf("bucketExpr(%v) = %q, want %q", c.gran, got, want)
		}
	}
}

func TestBucketExpr_RejectsInjection(t *testing.T) {
	// A value bypassing upstream validation must not reach the SQL literal.
	if _, err := bucketExpr(insightsv1.Granularity_GRANULARITY_DAY, "occur_time", "evil'); drop"); err == nil {
		t.Error("bucketExpr accepted an injection-shaped timezone, want error")
	}
}

func TestUTCBucketing(t *testing.T) {
	if !utcBucketing("") || !utcBucketing("UTC") {
		t.Error("utcBucketing should be true for \"\" and \"UTC\"")
	}
	if utcBucketing("Asia/Kolkata") {
		t.Error("utcBucketing(Asia/Kolkata) = true, want false")
	}
}
