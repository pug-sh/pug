package projects

import (
	"context"
	"encoding/json"
	"errors"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/rs/xid"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	dashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/dashboards/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
)

var (
	ErrDashboardNotFound        = errors.New("dashboard not found")
	ErrDashboardInsightNotFound = errors.New("dashboard insight not found")
)

type DashboardWithInsights struct {
	Dashboard dbread.Dashboard
	Insights  []dbread.DashboardInsight
}

func (s *Service) CreateDashboard(ctx context.Context, projectID, displayName, description string) (dbwrite.Dashboard, error) {
	return s.write.CreateDashboard(ctx, dbwrite.CreateDashboardParams{
		Description: description,
		ID:          xid.New().String(),
		ProjectID:   projectID,
		DisplayName: displayName,
	})
}

func (s *Service) ListDashboards(ctx context.Context, projectID string) ([]DashboardWithInsights, error) {
	dashboards, err := s.read.ListDashboardsByProjectID(ctx, projectID)
	if err != nil {
		return nil, err
	}

	result := make([]DashboardWithInsights, 0, len(dashboards))
	for _, dashboard := range dashboards {
		insights, err := s.read.ListDashboardInsightsByDashboardIDAndProjectID(ctx, dbread.ListDashboardInsightsByDashboardIDAndProjectIDParams{
			DashboardID: dashboard.ID,
			ProjectID:   projectID,
		})
		if err != nil {
			return nil, err
		}
		result = append(result, DashboardWithInsights{
			Dashboard: dashboard,
			Insights:  insights,
		})
	}

	return result, nil
}

func (s *Service) GetDashboard(ctx context.Context, projectID, dashboardID string) (DashboardWithInsights, error) {
	dashboard, err := s.read.GetDashboardByIDAndProjectID(ctx, dbread.GetDashboardByIDAndProjectIDParams{
		ID:        dashboardID,
		ProjectID: projectID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DashboardWithInsights{}, ErrDashboardNotFound
		}
		return DashboardWithInsights{}, err
	}

	insights, err := s.read.ListDashboardInsightsByDashboardIDAndProjectID(ctx, dbread.ListDashboardInsightsByDashboardIDAndProjectIDParams{
		DashboardID: dashboardID,
		ProjectID:   projectID,
	})
	if err != nil {
		return DashboardWithInsights{}, err
	}

	return DashboardWithInsights{
		Dashboard: dashboard,
		Insights:  insights,
	}, nil
}

func (s *Service) UpdateDashboardDisplayName(ctx context.Context, projectID, dashboardID, displayName, description string) (dbwrite.Dashboard, error) {
	dashboard, err := s.write.UpdateDashboardDisplayName(ctx, dbwrite.UpdateDashboardDisplayNameParams{
		Description: description,
		ID:          dashboardID,
		ProjectID:   projectID,
		DisplayName: displayName,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return dbwrite.Dashboard{}, ErrDashboardNotFound
		}
		return dbwrite.Dashboard{}, err
	}
	return dashboard, nil
}

func (s *Service) DeleteDashboard(ctx context.Context, projectID, dashboardID string) error {
	if _, err := s.write.DeleteDashboard(ctx, dbwrite.DeleteDashboardParams{
		ID:        dashboardID,
		ProjectID: projectID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrDashboardNotFound
		}
		return err
	}
	return nil
}

func (s *Service) CreateDashboardInsight(
	ctx context.Context,
	projectID, dashboardID, displayName, description string,
	query *insightsv1.QueryRequest,
	layouts []*dashboardsv1.ResponsiveGridLayout,
) (dbwrite.DashboardInsight, error) {
	queryJSON, err := QueryMessageToMap(query)
	if err != nil {
		return dbwrite.DashboardInsight{}, err
	}
	layoutsMap, err := LayoutsToMap(layouts)
	if err != nil {
		return dbwrite.DashboardInsight{}, err
	}

	insight, err := s.write.CreateDashboardInsight(ctx, dbwrite.CreateDashboardInsightParams{
		Description:  description,
		ID:           xid.New().String(),
		DashboardID:  dashboardID,
		ProjectID:    projectID,
		DisplayName:  displayName,
		InsightQuery: queryJSON,
		Layouts:      layoutsMap,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return dbwrite.DashboardInsight{}, ErrDashboardNotFound
		}
		return dbwrite.DashboardInsight{}, err
	}
	return insight, nil
}

func (s *Service) UpsertDashboardInsight(
	ctx context.Context,
	projectID, dashboardID, insightID, displayName, description string,
	query *insightsv1.QueryRequest,
	layouts []*dashboardsv1.ResponsiveGridLayout,
) (dbwrite.DashboardInsight, error) {
	queryJSON, err := QueryMessageToMap(query)
	if err != nil {
		return dbwrite.DashboardInsight{}, err
	}
	layoutsMap, err := LayoutsToMap(layouts)
	if err != nil {
		return dbwrite.DashboardInsight{}, err
	}

	insight, err := s.write.UpdateDashboardInsight(ctx, dbwrite.UpdateDashboardInsightParams{
		Description:  description,
		ID:           insightID,
		DashboardID:  dashboardID,
		ProjectID:    projectID,
		DisplayName:  displayName,
		InsightQuery: queryJSON,
		Layouts:      layoutsMap,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return dbwrite.DashboardInsight{}, ErrDashboardInsightNotFound
		}
		return dbwrite.DashboardInsight{}, err
	}
	return insight, nil
}

func (s *Service) DeleteDashboardInsight(ctx context.Context, projectID, dashboardID, insightID string) error {
	if _, err := s.write.DeleteDashboardInsight(ctx, dbwrite.DeleteDashboardInsightParams{
		ID:          insightID,
		DashboardID: dashboardID,
		ProjectID:   projectID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrDashboardInsightNotFound
		}
		return err
	}
	return nil
}

func QueryMessageToMap(msg *insightsv1.QueryRequest) (map[string]any, error) {
	if msg == nil {
		return map[string]any{}, nil
	}
	data, err := protojson.Marshal(msg)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func MapToQueryMessage(data map[string]any) (*insightsv1.QueryRequest, error) {
	if len(data) == 0 {
		return &insightsv1.QueryRequest{}, nil
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	var out insightsv1.QueryRequest
	if err := protojson.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func LayoutsToMap(layouts []*dashboardsv1.ResponsiveGridLayout) (map[string]any, error) {
	out := make(map[string]any, len(layouts))
	for _, layout := range layouts {
		out[layout.GetBreakpoint()] = map[string]any{
			"x":      layout.GetX(),
			"y":      layout.GetY(),
			"w":      layout.GetW(),
			"h":      layout.GetH(),
			"minW":   layout.GetMinW(),
			"maxW":   layout.GetMaxW(),
			"minH":   layout.GetMinH(),
			"maxH":   layout.GetMaxH(),
			"static": layout.GetStatic(),
		}
	}
	return out, nil
}

func MapToLayouts(data map[string]any) ([]*dashboardsv1.ResponsiveGridLayout, error) {
	if len(data) == 0 {
		return nil, nil
	}

	breakpoints := make([]string, 0, len(data))
	for breakpoint := range data {
		breakpoints = append(breakpoints, breakpoint)
	}
	sort.Strings(breakpoints)

	out := make([]*dashboardsv1.ResponsiveGridLayout, 0, len(data))
	for _, breakpoint := range breakpoints {
		raw, err := json.Marshal(data[breakpoint])
		if err != nil {
			return nil, err
		}
		var item struct {
			X      int32 `json:"x"`
			Y      int32 `json:"y"`
			W      int32 `json:"w"`
			H      int32 `json:"h"`
			MinW   int32 `json:"minW"`
			MaxW   int32 `json:"maxW"`
			MinH   int32 `json:"minH"`
			MaxH   int32 `json:"maxH"`
			Static bool  `json:"static"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, err
		}
		out = append(out, &dashboardsv1.ResponsiveGridLayout{
			Breakpoint: proto.String(breakpoint),
			X:          proto.Int32(item.X),
			Y:          proto.Int32(item.Y),
			W:          proto.Int32(item.W),
			H:          proto.Int32(item.H),
			MinW:       proto.Int32(item.MinW),
			MaxW:       proto.Int32(item.MaxW),
			MinH:       proto.Int32(item.MinH),
			MaxH:       proto.Int32(item.MaxH),
			Static:     proto.Bool(item.Static),
		})
	}

	return out, nil
}
