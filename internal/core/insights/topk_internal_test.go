package insights

import (
	"context"
	"errors"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
)

// fakeConn is a driver.Conn whose Query fails with a configurable error (a
// generic operational error when err is nil). It lets buildTopKResult's
// enrichment paths be exercised without a live ClickHouse. Every other Conn
// method is nil-embedded and must not be called by the code under test (it would
// panic, surfacing the unexpected call).
type fakeConn struct {
	driver.Conn
	err error
}

func (c fakeConn) Query(_ context.Context, _ string, _ ...any) (driver.Rows, error) {
	if c.err != nil {
		return nil, c.err
	}
	return nil, errors.New("simulated clickhouse failure")
}

// userTopKResult is the common buildTopKResult call: a USER-dimension query
// (the only dimension that triggers profile enrichment) over one ranked row plus
// the synthetic $others bucket.
func userTopKResult(t *testing.T, executor *Executor) (*insightsv1.TopKResult, error) {
	t.Helper()
	q := TopKQuery{dimension: insightsv1.TopKQuery_DIMENSION_USER}
	rows := []TopKRow{
		{DimensionValue: "alice", Value: 175},
		{DimensionValue: topKOthersValue, IsOthers: true, Value: 80},
	}
	return buildTopKResult(context.Background(), executor, "proj_top_k", q, rows)
}

// omitTopKResult mirrors userTopKResult for the omit-$others shape: every row is
// a real ranked user (no synthetic bucket), so every row is eligible for profile
// enrichment.
func omitTopKResult(t *testing.T, executor *Executor) (*insightsv1.TopKResult, error) {
	t.Helper()
	q := TopKQuery{dimension: insightsv1.TopKQuery_DIMENSION_USER}
	rows := []TopKRow{
		{DimensionValue: "alice", Value: 175},
		{DimensionValue: "bob", Value: 120},
	}
	return buildTopKResult(context.Background(), executor, "proj_top_k", q, rows)
}

// TestBuildTopKResult_OmitOthersAllRowsPreserved pins the omit-$others shape (no
// is_others row) through buildTopKResult: every row is collected for enrichment
// (none skipped as the synthetic bucket), and a (non-context) enrichment failure
// degrades every row to un-enriched while preserving the full ranking. No
// synthetic bucket is expected or required.
func TestBuildTopKResult_OmitOthersAllRowsPreserved(t *testing.T) {
	res, err := omitTopKResult(t, NewExecutor(fakeConn{}))
	if err != nil {
		t.Fatalf("enrichment failure should degrade, not error; got %v", err)
	}
	got := res.GetRows()
	if len(got) != 2 {
		t.Fatalf("expected 2 rows preserved, got %d", len(got))
	}
	for i, want := range []struct {
		dim string
		val float64
	}{{"alice", 175}, {"bob", 120}} {
		if got[i].GetDimensionValue() != want.dim || got[i].GetValue() != want.val {
			t.Errorf("row %d: expected %s/%v, got %v", i, want.dim, want.val, got[i])
		}
		if got[i].GetIsOthers() {
			t.Errorf("row %d: omit shape must not flag is_others, got %v", i, got[i])
		}
		if got[i].GetProfile() != nil {
			t.Errorf("row %d: expected un-enriched row after enrichment failure, got %v", i, got[i].GetProfile())
		}
	}
}

// TestBuildTopKResult_EnrichmentFailureDegrades asserts that a (non-context)
// failure of the USER-row profile enrichment query degrades to un-enriched rows
// rather than discarding the already-computed ranking. The ranked rows + values
// are the payload; profile id/external_id/properties are decoration — a transient
// enrichment-query failure must not 5xx an otherwise-good top-K tile.
func TestBuildTopKResult_EnrichmentFailureDegrades(t *testing.T) {
	res, err := userTopKResult(t, NewExecutor(fakeConn{}))
	if err != nil {
		t.Fatalf("enrichment failure should degrade, not error; got %v", err)
	}

	got := res.GetRows()
	if len(got) != 2 {
		t.Fatalf("expected 2 rows preserved, got %d", len(got))
	}
	if got[0].GetDimensionValue() != "alice" || got[0].GetValue() != 175 {
		t.Errorf("row 0: ranking must be preserved (alice/175), got %v", got[0])
	}
	if got[0].GetProfile() != nil {
		t.Errorf("row 0: expected un-enriched row after enrichment failure, got profile %v", got[0].GetProfile())
	}
	if !got[1].GetIsOthers() || got[1].GetValue() != 80 {
		t.Errorf("row 1: $others bucket must be preserved, got %v", got[1])
	}
}

// TestBuildTopKResult_PropagatesContextError pins the distinction the degrade
// path must NOT erase: a context cancellation/deadline during the enrichment
// query is a request-lifecycle signal, not a transient enrichment fault, so it
// must propagate (queryFailed / dashboards.renderInsightTile depend on seeing it
// to return CodeCanceled/CodeDeadlineExceeded) rather than degrade to a 200 with
// partial data.
func TestBuildTopKResult_PropagatesContextError(t *testing.T) {
	for _, ctxErr := range []error{context.Canceled, context.DeadlineExceeded} {
		res, err := userTopKResult(t, NewExecutor(fakeConn{err: ctxErr}))
		if !errors.Is(err, ctxErr) {
			t.Errorf("expected %v to propagate, got err=%v", ctxErr, err)
		}
		if res != nil {
			t.Errorf("expected nil result on context error, got %v", res)
		}
	}
}

// TestBuildTopKResult_EmptyRowsSkipEnrichment asserts that with no ranked rows
// the enrichment query is never issued (so a failing conn cannot error) and an
// empty result is returned — pinning the len(ids) > 0 short-circuit. This is the
// shape a dashboard tile renders for an empty window.
func TestBuildTopKResult_EmptyRowsSkipEnrichment(t *testing.T) {
	// fakeConn would error if Query were called; reaching a nil error proves
	// enrichment was skipped for the zero-id case.
	q := TopKQuery{dimension: insightsv1.TopKQuery_DIMENSION_USER}
	res, err := buildTopKResult(context.Background(), NewExecutor(fakeConn{}), "proj_top_k", q, nil)
	if err != nil {
		t.Fatalf("empty rows must not issue an enrichment query; got %v", err)
	}
	if len(res.GetRows()) != 0 {
		t.Errorf("expected 0 rows, got %d", len(res.GetRows()))
	}
}
