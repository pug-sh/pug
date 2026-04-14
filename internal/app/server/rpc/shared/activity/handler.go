package activity

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/fivebitsio/cotton/internal/app/server/rpc"
	"github.com/fivebitsio/cotton/internal/core/events"
	coreinsights "github.com/fivebitsio/cotton/internal/core/insights"
	"github.com/fivebitsio/cotton/internal/deps/telemetry"
	commonv1 "github.com/fivebitsio/cotton/internal/gen/proto/common/v1"
	activityv1 "github.com/fivebitsio/cotton/internal/gen/proto/shared/activity/v1"
	"github.com/fivebitsio/cotton/internal/gen/proto/shared/activity/v1/activityv1connect"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/slogx"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type server struct {
	activityv1connect.UnimplementedActivityServiceHandler
	eventsReader    *events.Reader
	insightsService *coreinsights.Service
	profilesRead    *dbread.Queries
}

func NewServer(ch driver.Conn, insightsService *coreinsights.Service, profilesRead *dbread.Queries) *server {
	return &server{
		eventsReader:    events.NewReader(ch),
		insightsService: insightsService,
		profilesRead:    profilesRead,
	}
}

func (s *server) GetActivityFeed(
	ctx context.Context,
	req *connect.Request[activityv1.GetActivityFeedRequest],
) (*connect.Response[activityv1.GetActivityFeedResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
	}

	params := events.ActivityFeedParams{
		ProjectID:       principal.Project.ID,
		DistinctID:      req.Msg.GetDistinctId(),
		SessionID:       req.Msg.GetSessionId(),
		TimeRange:       req.Msg.GetTimeRange(),
		PropertyFilters: req.Msg.GetPropertyFilters(),
		EventFilters:    req.Msg.GetEvents(),
		PageSize:        req.Msg.GetPageSize(),
	}

	if req.Msg.GetPageToken() != "" {
		cursor, err := events.DecodeEventCursor(req.Msg.GetPageToken())
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid page token"))
		}
		params.PageToken = cursor
	}

	evts, nextCursor, err := s.eventsReader.GetActivityFeed(ctx, params)
	if err != nil {
		if errors.Is(err, events.ErrInvalidFilter) {
			slog.WarnContext(ctx, "invalid filter in activity feed request",
				slogx.Error(err),
				slog.String("projectID", principal.Project.ID))
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid filter parameters"))
		}
		slog.ErrorContext(ctx, "failed to get activity feed",
			slogx.Error(err),
			slog.String("projectID", principal.Project.ID),
			slog.String("distinctID", req.Msg.GetDistinctId()),
			slog.String("sessionID", req.Msg.GetSessionId()),
			slog.Int("filterCount", len(req.Msg.GetPropertyFilters())),
			slog.Int("eventFilterCount", len(req.Msg.GetEvents())))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	protoEvents, err := eventsToProto(ctx, evts, principal.Project.ID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to convert events in GetActivityFeed",
			slogx.Error(err),
			slog.String("projectID", principal.Project.ID),
			slog.String("distinctID", req.Msg.GetDistinctId()))
		return nil, err
	}

	resp := &activityv1.GetActivityFeedResponse{
		Events: protoEvents,
	}
	if nextCursor != nil {
		token, err := nextCursor.Encode()
		if err != nil {
			slog.ErrorContext(ctx, "failed to encode pagination cursor", slogx.Error(err))
			telemetry.RecordError(ctx, err)
			return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
		}
		resp.NextPageToken = token
	}

	return connect.NewResponse(resp), nil
}

func (s *server) GetEventExplorer(
	ctx context.Context,
	req *connect.Request[activityv1.GetEventExplorerRequest],
) (*connect.Response[activityv1.GetEventExplorerResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
	}

	params := events.EventExplorerParams{
		ProjectID:       principal.Project.ID,
		DistinctID:      req.Msg.GetDistinctId(),
		SessionID:       req.Msg.GetSessionId(),
		TimeRange:       req.Msg.GetTimeRange(),
		PropertyFilters: req.Msg.GetPropertyFilters(),
		EventFilters:    req.Msg.GetEvents(),
		PageSize:        req.Msg.GetPageSize(),
	}

	if req.Msg.GetPageToken() != "" {
		cursor, err := events.DecodeEventCursor(req.Msg.GetPageToken())
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid page token"))
		}
		params.PageToken = cursor
	}

	evts, nextCursor, err := s.eventsReader.GetEventExplorer(ctx, params)
	if err != nil {
		if errors.Is(err, events.ErrInvalidFilter) {
			slog.WarnContext(ctx, "invalid filter in event explorer request",
				slogx.Error(err),
				slog.String("projectID", principal.Project.ID))
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid filter parameters"))
		}
		slog.ErrorContext(ctx, "failed to get event explorer",
			slogx.Error(err),
			slog.String("projectID", principal.Project.ID),
			slog.String("distinctID", req.Msg.GetDistinctId()),
			slog.String("sessionID", req.Msg.GetSessionId()),
			slog.Int("filterCount", len(req.Msg.GetPropertyFilters())),
			slog.Int("eventFilterCount", len(req.Msg.GetEvents())))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	protoEvents, err := eventsToProto(ctx, evts, principal.Project.ID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to convert events in GetEventExplorer",
			slogx.Error(err),
			slog.String("projectID", principal.Project.ID),
			slog.String("distinctID", req.Msg.GetDistinctId()))
		return nil, err
	}

	resp := &activityv1.GetEventExplorerResponse{
		Events: protoEvents,
	}
	if nextCursor != nil {
		token, err := nextCursor.Encode()
		if err != nil {
			slog.ErrorContext(ctx, "failed to encode pagination cursor", slogx.Error(err))
			telemetry.RecordError(ctx, err)
			return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
		}
		resp.NextPageToken = token
	}

	return connect.NewResponse(resp), nil
}

// eventsToProto converts internal Event structs to proto ActivityEvent messages.
func eventsToProto(ctx context.Context, evts []events.Event, projectID string) ([]*activityv1.ActivityEvent, error) {
	protoEvents := make([]*activityv1.ActivityEvent, len(evts))
	for i, e := range evts {
		autoProps, err := mapToStruct(e.AutoProperties)
		if err != nil {
			slog.ErrorContext(ctx, "failed to convert auto_properties",
				slogx.Error(err),
				slog.String("eventID", e.EventID),
				slog.String("projectID", projectID))
			telemetry.RecordError(ctx, err)
			return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
		}
		customProps, err := mapToStruct(e.CustomProperties)
		if err != nil {
			slog.ErrorContext(ctx, "failed to convert custom_properties",
				slogx.Error(err),
				slog.String("eventID", e.EventID),
				slog.String("projectID", projectID))
			telemetry.RecordError(ctx, err)
			return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
		}
		protoEvents[i] = &activityv1.ActivityEvent{
			EventId:          e.EventID,
			Kind:             e.Kind,
			DistinctId:       e.DistinctID,
			OccurTime:        timestamppb.New(e.OccurTime),
			SessionId:        e.SessionID,
			AutoProperties:   autoProps,
			CustomProperties: customProps,
		}
	}
	return protoEvents, nil
}

func mapToStruct(m map[string]string) (*structpb.Struct, error) {
	fields := make(map[string]any, len(m))
	for k, v := range m {
		fields[k] = v
	}
	return structpb.NewStruct(fields)
}

func (s *server) GetActivityHeatmap(
	ctx context.Context,
	req *connect.Request[activityv1.GetActivityHeatmapRequest],
) (*connect.Response[activityv1.GetActivityHeatmapResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
	}

	tr := req.Msg.GetTimeRange()
	if tr == nil {
		now := time.Now().UTC()
		tr = &commonv1.TimeRange{
			From: timestamppb.New(now.AddDate(0, 0, -events.DefaultHeatmapDays)),
			To:   timestamppb.New(now),
		}
	}

	days, err := s.eventsReader.GetActivityHeatmap(ctx, events.ActivityHeatmapParams{
		ProjectID:  principal.Project.ID,
		DistinctID: req.Msg.GetDistinctId(),
		TimeRange:  tr,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to get activity heatmap",
			slogx.Error(err),
			slog.String("projectID", principal.Project.ID),
			slog.String("distinctID", req.Msg.GetDistinctId()),
			slog.Time("from", tr.GetFrom().AsTime()),
			slog.Time("to", tr.GetTo().AsTime()))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	proto := make([]*activityv1.HeatmapDay, len(days))
	for i, d := range days {
		proto[i] = &activityv1.HeatmapDay{Date: d.Date, Count: d.Count}
	}

	return connect.NewResponse(&activityv1.GetActivityHeatmapResponse{Days: proto}), nil
}

func (s *server) GetProfileStats(
	ctx context.Context,
	req *connect.Request[activityv1.GetProfileStatsRequest],
) (*connect.Response[activityv1.GetProfileStatsResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
	}

	stats, heatmap, err := s.eventsReader.GetProfileStats(ctx, principal.Project.ID, req.Msg.GetDistinctId())
	if err != nil {
		slog.ErrorContext(ctx, "failed to get profile stats",
			slogx.Error(err),
			slog.String("projectID", principal.Project.ID),
			slog.String("distinctID", req.Msg.GetDistinctId()))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	resp := &activityv1.GetProfileStatsResponse{}

	if stats != nil {
		resp.Stats = &activityv1.ProfileStats{
			FirstSeen:      timestamppb.New(stats.FirstSeen),
			LastSeen:       timestamppb.New(stats.LastSeen),
			TotalEvents:    stats.TotalEvents,
			Browser:        stats.Browser,
			BrowserVersion: stats.BrowserVersion,
			Os:             stats.OS,
			OsVersion:      stats.OSVersion,
			Device:         stats.Device,
			Country:        stats.Country,
			City:           stats.City,
			Ip:             stats.IP,
		}
	}

	resp.Heatmap = make([]*activityv1.HeatmapDay, len(heatmap))
	for i, d := range heatmap {
		resp.Heatmap[i] = &activityv1.HeatmapDay{Date: d.Date, Count: d.Count}
	}

	profile, err := s.profilesRead.GetProfileByIDAndProjectID(ctx, dbread.GetProfileByIDAndProjectIDParams{
		ID:        req.Msg.GetDistinctId(),
		ProjectID: principal.Project.ID,
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		slog.ErrorContext(ctx, "failed to get profile properties",
			slogx.Error(err),
			slog.String("projectID", principal.Project.ID),
			slog.String("distinctID", req.Msg.GetDistinctId()))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	if err == nil {
		props, err := structpb.NewStruct(profile.Properties)
		if err != nil {
			slog.ErrorContext(ctx, "failed to convert profile properties to struct",
				slogx.Error(err),
				slog.String("projectID", principal.Project.ID),
				slog.String("distinctID", req.Msg.GetDistinctId()))
			telemetry.RecordError(ctx, err)
			return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
		}
		resp.Properties = props
	}

	return connect.NewResponse(resp), nil
}

func (s *server) GetFilterSchema(
	ctx context.Context,
	req *connect.Request[activityv1.GetFilterSchemaRequest],
) (*connect.Response[activityv1.GetFilterSchemaResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
	}

	projectID := principal.Project.ID

	schema, err := s.insightsService.GetFilterSchema(ctx, projectID, req.Msg.GetEventKind())
	if err != nil {
		slog.ErrorContext(ctx, "failed to get filter schema", slogx.Error(err),
			slog.String("projectID", projectID))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	return connect.NewResponse(&activityv1.GetFilterSchemaResponse{
		Events:              schema.GetEvents(),
		AutoPropertyKeys:    schema.GetAutoPropertyKeys(),
		CustomPropertyKeys:  schema.GetCustomPropertyKeys(),
		ProfilePropertyKeys: schema.GetProfilePropertyKeys(),
	}), nil
}

func (s *server) GetPropertyValues(
	ctx context.Context,
	req *connect.Request[activityv1.GetPropertyValuesRequest],
) (*connect.Response[activityv1.GetPropertyValuesResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
	}

	projectID := principal.Project.ID

	values, err := s.insightsService.GetPropertyValues(ctx, projectID, req.Msg.GetPropertyKey(), req.Msg.GetEventKind(), req.Msg.GetSource())
	if err != nil {
		slog.ErrorContext(ctx, "failed to get property values", slogx.Error(err),
			slog.String("projectID", projectID),
			slog.String("propertyKey", req.Msg.GetPropertyKey()))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	return connect.NewResponse(&activityv1.GetPropertyValuesResponse{Values: values}), nil
}
