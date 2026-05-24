package insights

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"google.golang.org/protobuf/proto"

	"github.com/pug-sh/pug/internal/deps/telemetry"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
	"github.com/pug-sh/pug/internal/slogx"
)

// InvalidQueryError indicates client-side query parameters failed to build.
type InvalidQueryError struct {
	Message string
}

func (e *InvalidQueryError) Error() string {
	return e.Message
}

// ExecuteQuery runs an insights query for the given project.
func ExecuteQuery(
	ctx context.Context,
	executor *Executor,
	projectID string,
	req *insightsv1.QueryRequest,
) (*insightsv1.QueryResponse, error) {
	if executor == nil {
		panic("insights: executor is nil")
	}

	resp := &insightsv1.QueryResponse{}

	switch req.GetSpec().GetInsightType() {
	case insightsv1.InsightType_INSIGHT_TYPE_TRENDS:
		q, err := BuildTrendsQuery(req, projectID)
		if err != nil {
			slog.WarnContext(ctx, "failed to build trends query", slogx.Error(err),
				slog.String("project_id", projectID))
			return nil, &InvalidQueryError{Message: err.Error()}
		}
		rows, err := executor.QueryTrends(ctx, projectID, q)
		if err != nil {
			return nil, recordQueryFailure(ctx, projectID, err)
		}
		series, err := GroupSeries(ctx, rows, q.Properties())
		if err != nil {
			return nil, recordQueryFailure(ctx, projectID, err)
		}
		resp.Result = &insightsv1.QueryResponse_Trends{
			Trends: &insightsv1.TrendsResult{Series: series},
		}

	case insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION:
		q, err := BuildSegmentationQuery(req, projectID)
		if err != nil {
			slog.WarnContext(ctx, "failed to build segmentation query", slogx.Error(err),
				slog.String("project_id", projectID))
			return nil, &InvalidQueryError{Message: err.Error()}
		}
		value, err := executor.QueryScalar(ctx, projectID, q)
		if err != nil {
			return nil, recordQueryFailure(ctx, projectID, err)
		}
		resp.Result = &insightsv1.QueryResponse_Segmentation{
			Segmentation: &insightsv1.SegmentationResult{Total: proto.Float64(value)},
		}

	case insightsv1.InsightType_INSIGHT_TYPE_FUNNEL:
		var funnelRows []FunnelRow
		var funnelProperties []string
		if req.GetSpec().GetIncludeStepTiming() {
			q, err := BuildFunnelTimingQuery(req, projectID)
			if err != nil {
				slog.WarnContext(ctx, "failed to build funnel timing query", slogx.Error(err),
					slog.String("project_id", projectID))
				return nil, &InvalidQueryError{Message: err.Error()}
			}
			users, err := executor.QueryFunnelUserEvents(ctx, projectID, q)
			if err != nil {
				return nil, recordQueryFailure(ctx, projectID, err)
			}
			funnelRows, err = ComputeFunnelTiming(ctx, projectID, users, q.Kinds(), q.WindowSec(), q.NumBreakdowns())
			if err != nil {
				return nil, recordQueryFailure(ctx, projectID, err)
			}
			funnelProperties = q.Properties()
		} else {
			q, err := BuildFunnelCountsQuery(req, projectID)
			if err != nil {
				slog.WarnContext(ctx, "failed to build funnel query", slogx.Error(err),
					slog.String("project_id", projectID))
				return nil, &InvalidQueryError{Message: err.Error()}
			}
			funnelRows, err = executor.QueryFunnel(ctx, projectID, q)
			if err != nil {
				return nil, recordQueryFailure(ctx, projectID, err)
			}
			funnelProperties = q.Properties()
		}
		funnelSeries, err := GroupFunnelSeries(ctx, funnelRows, funnelProperties)
		if err != nil {
			return nil, recordQueryFailure(ctx, projectID, err)
		}
		resp.Result = &insightsv1.QueryResponse_Funnel{
			Funnel: &insightsv1.FunnelResult{Series: funnelSeries},
		}

	case insightsv1.InsightType_INSIGHT_TYPE_RETENTION:
		q, err := BuildRetentionQuery(req, projectID)
		if err != nil {
			slog.WarnContext(ctx, "failed to build retention query", slogx.Error(err),
				slog.String("project_id", projectID))
			return nil, &InvalidQueryError{Message: err.Error()}
		}
		rows, err := executor.QueryRetention(ctx, projectID, q)
		if err != nil {
			return nil, recordQueryFailure(ctx, projectID, err)
		}
		retentionSeries, err := GroupRetentionSeries(ctx, rows, q.Properties())
		if err != nil {
			return nil, recordQueryFailure(ctx, projectID, err)
		}
		resp.Result = &insightsv1.QueryResponse_Retention{
			Retention: &insightsv1.RetentionResult{Series: retentionSeries},
		}

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

func recordQueryFailure(ctx context.Context, projectID string, err error) error {
	slog.ErrorContext(ctx, "insights query failed",
		slogx.Error(err),
		slog.String("project_id", projectID))
	telemetry.RecordError(ctx, err)
	return errors.New("query failed")
}
