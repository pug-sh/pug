package insights

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"

	"github.com/pug-sh/pug/internal/deps/telemetry"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
	"github.com/pug-sh/pug/internal/slogx"
)

// InvalidQueryError indicates client-side query parameters failed to build.
// Message carries the client-facing text; err preserves the underlying builder
// error so callers can traverse the chain with errors.Is / errors.As.
type InvalidQueryError struct {
	Message string
	err     error
}

func (e *InvalidQueryError) Error() string { return e.Message }
func (e *InvalidQueryError) Unwrap() error { return e.err }

// ExecuteQuery runs an insights query for the given project. now is the request's
// reference time, used to decide rollup window eligibility (see rollupWindowAligned);
// callers pass the same now used to resolve any preset window so a live "to == now"
// is treated as aligned.
func ExecuteQuery(
	ctx context.Context,
	executor *Executor,
	projectID string,
	req *insightsv1.QueryRequest,
	now time.Time,
) (*insightsv1.QueryResponse, error) {
	if executor == nil {
		panic("insights: executor is nil")
	}

	resp := &insightsv1.QueryResponse{}

	switch req.GetSpec().GetInsightType() {
	case insightsv1.InsightType_INSIGHT_TYPE_TRENDS:
		q, usedRollup, err := trendsQueryForExecution(req, projectID, now)
		if err != nil {
			return nil, buildQueryError(ctx, projectID, "trends", usedRollup, err)
		}
		rows, err := executor.QueryTrends(ctx, projectID, q)
		if err != nil {
			return nil, queryFailed(err)
		}
		if usedRollup && req.GetSpec().GetSession() == nil {
			// Session rollup queries carry no event list and emit a single
			// synthetic series kind ($session), so multi-event-kind zero-filling
			// doesn't apply — skip it for them.
			// fillMultiEventTrendZeros self-guards on len(kinds) <= 1, so single-
			// event rollup queries pass through unchanged.
			events := req.GetSpec().GetEvents()
			kinds := make([]string, len(events))
			for i, ev := range events {
				kinds[i] = ev.GetEvent().GetKind()
			}
			rows = fillMultiEventTrendZeros(rows, kinds)
		}
		series, err := GroupSeries(ctx, rows, q.Properties(), q.BreakdownLimit())
		if err != nil {
			return nil, queryFailed(err)
		}
		resp.Result = &insightsv1.QueryResponse_Trends{
			Trends: &insightsv1.TrendsResult{Series: series},
		}

	case insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION:
		q, usedRollup, err := segmentationQueryForExecution(req, projectID, now)
		if err != nil {
			return nil, buildQueryError(ctx, projectID, "segmentation", usedRollup, err)
		}
		value, err := executor.QueryScalar(ctx, projectID, q)
		if err != nil {
			return nil, queryFailed(err)
		}
		resp.Result = &insightsv1.QueryResponse_Segmentation{
			Segmentation: &insightsv1.SegmentationResult{Total: proto.Float64(value)},
		}

	case insightsv1.InsightType_INSIGHT_TYPE_FUNNEL:
		var funnelRows []FunnelRow
		var funnelProperties []string
		var funnelBreakdownLimit int
		if req.GetSpec().GetIncludeStepTiming() {
			// Run windowFunnel counts and per-user timing in parallel:
			// counts are fast (single-pass windowFunnel), timing is heavier
			// (pre-filtered groupArray). Merging takes counts from windowFunnel
			// and timing stats from ComputeFunnelTiming.
			countsQ, err := BuildFunnelCountsQuery(req, projectID)
			if err != nil {
				slog.WarnContext(ctx, "failed to build funnel counts query", slogx.Error(err),
					slog.String("project_id", projectID))
				return nil, &InvalidQueryError{Message: err.Error(), err: err}
			}
			timingQ, err := BuildFunnelTimingQuery(req, projectID)
			if err != nil {
				slog.WarnContext(ctx, "failed to build funnel timing query", slogx.Error(err),
					slog.String("project_id", projectID))
				return nil, &InvalidQueryError{Message: err.Error(), err: err}
			}

			var countRows []FunnelRow
			var timingRows []FunnelRow
			eg, egCtx := errgroup.WithContext(ctx)

			eg.Go(func() error {
				rows, err := executor.QueryFunnel(egCtx, projectID, countsQ)
				if err != nil {
					return err
				}
				countRows = rows
				return nil
			})

			eg.Go(func() error {
				users, err := executor.QueryFunnelUserEvents(egCtx, projectID, timingQ)
				if err != nil {
					return err
				}
				rows, err := ComputeFunnelTiming(egCtx, projectID, users, timingQ.Kinds(), timingQ.WindowSec(), timingQ.NumBreakdowns())
				if err != nil {
					return err
				}
				timingRows = rows
				return nil
			})

			if err := eg.Wait(); err != nil {
				return nil, queryFailed(err)
			}

			funnelRows = MergeFunnelCountsAndTiming(countRows, timingRows)
			funnelProperties = countsQ.Properties()
			funnelBreakdownLimit = countsQ.BreakdownLimit()
		} else {
			q, err := BuildFunnelCountsQuery(req, projectID)
			if err != nil {
				slog.WarnContext(ctx, "failed to build funnel query", slogx.Error(err),
					slog.String("project_id", projectID))
				return nil, &InvalidQueryError{Message: err.Error(), err: err}
			}
			funnelRows, err = executor.QueryFunnel(ctx, projectID, q)
			if err != nil {
				return nil, queryFailed(err)
			}
			funnelProperties = q.Properties()
			funnelBreakdownLimit = q.BreakdownLimit()
		}
		funnelSeries, err := GroupFunnelSeries(ctx, funnelRows, funnelProperties, funnelBreakdownLimit)
		if err != nil {
			return nil, queryFailed(err)
		}
		resp.Result = &insightsv1.QueryResponse_Funnel{
			Funnel: &insightsv1.FunnelResult{Series: funnelSeries},
		}

	case insightsv1.InsightType_INSIGHT_TYPE_RETENTION:
		q, err := BuildRetentionQuery(req, projectID)
		if err != nil {
			slog.WarnContext(ctx, "failed to build retention query", slogx.Error(err),
				slog.String("project_id", projectID))
			return nil, &InvalidQueryError{Message: err.Error(), err: err}
		}
		rows, err := executor.QueryRetention(ctx, projectID, q)
		if err != nil {
			return nil, queryFailed(err)
		}
		retentionSeries, err := GroupRetentionSeries(ctx, rows, q.Properties(), q.BreakdownLimit())
		if err != nil {
			return nil, queryFailed(err)
		}
		resp.Result = &insightsv1.QueryResponse_Retention{
			Retention: &insightsv1.RetentionResult{Series: retentionSeries},
		}

	case insightsv1.InsightType_INSIGHT_TYPE_USER_FLOW:
		q, err := BuildUserFlowQuery(req, projectID)
		if err != nil {
			slog.WarnContext(ctx, "failed to build user flow query", slogx.Error(err),
				slog.String("project_id", projectID))
			return nil, &InvalidQueryError{Message: err.Error(), err: err}
		}
		rows, err := executor.QueryUserFlow(ctx, projectID, q)
		if err != nil {
			return nil, queryFailed(err)
		}
		result := GroupUserFlowResult(ctx, rows, q.MaxNodes(), q.MaxLinks())
		resp.Result = &insightsv1.QueryResponse_UserFlow{UserFlow: result}

	case insightsv1.InsightType_INSIGHT_TYPE_TOP_K:
		q, usedRollup, err := topKQueryForExecution(req, projectID, now)
		if err != nil {
			return nil, buildQueryError(ctx, projectID, "top k", usedRollup, err)
		}
		rows, err := executor.QueryTopK(ctx, projectID, q)
		if err != nil {
			return nil, queryFailed(err)
		}
		result, err := buildTopKResult(ctx, executor, projectID, q, rows)
		if err != nil {
			return nil, queryFailed(err)
		}
		resp.Result = &insightsv1.QueryResponse_TopK{TopK: result}

	default:
		err := fmt.Errorf("unsupported insight type %s", req.GetSpec().GetInsightType().String())
		slog.ErrorContext(ctx, "unsupported insight type reached ExecuteQuery default",
			slogx.Error(err),
			slog.String("project_id", projectID),
			slog.String("insight_type", req.GetSpec().GetInsightType().String()))
		telemetry.RecordError(ctx, err)
		return nil, errors.New("query failed")
	}

	return resp, nil
}

// buildQueryError classifies a trends/segmentation build failure. usedRollup
// (reported by the execution dispatcher, so eligibility is evaluated once) means
// the failing builder was buildTrendsFromRollup / buildSegmentationFromRollup,
// whose preconditions the dispatcher already guaranteed — so any error is an
// internal bug: logged + recorded at source here and returned as a generic
// failure. Otherwise it is a client BuildTrendsQuery / BuildSegmentationQuery
// validation error, which surfaces as InvalidQueryError (WarnContext, not
// recorded — telemetry.md's client-input exception).
func buildQueryError(ctx context.Context, projectID, insight string, usedRollup bool, err error) error {
	if usedRollup {
		slog.ErrorContext(ctx, "rollup query build failed", slogx.Error(err),
			slog.String("project_id", projectID), slog.String("insight", insight))
		telemetry.RecordError(ctx, err)
		return errors.New("query failed")
	}
	slog.WarnContext(ctx, "failed to build query", slogx.Error(err),
		slog.String("project_id", projectID), slog.String("insight", insight))
	return &InvalidQueryError{Message: err.Error(), err: err}
}

// queryFailed is the client-facing translation of an execution failure. The
// detecting layer (Executor / Group*Series / ComputeFunnelTiming) already
// logged + recorded via telemetry.RecordError at source; per telemetry.md this
// downstream layer only translates and must not re-log or re-record.
//
// A context cancellation/deadline is returned unwrapped rather than flattened to
// the generic message: the executor preserves its identity via %w, and callers
// such as dashboards.renderInsightTile rely on errors.Is(err, context.Canceled)
// to propagate it as a request-level failure instead of masking it as a per-tile
// error. recordQueryError already skipped recording it, so it is not logged here.
func queryFailed(err error) error {
	if isContextError(err) {
		return err
	}
	return errors.New("query failed")
}
