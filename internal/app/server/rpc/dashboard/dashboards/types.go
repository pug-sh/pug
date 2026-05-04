package dashboards

import (
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	coreprojects "github.com/pug-sh/pug/internal/core/projects"
	dashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/dashboards/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
)

func roDashboardToRPC(dashboard coreprojects.DashboardWithInsights) (*dashboardsv1.Dashboard, error) {
	insights := make([]*dashboardsv1.DashboardInsight, 0, len(dashboard.Insights))
	for _, insight := range dashboard.Insights {
		msg, err := roInsightToRPC(insight)
		if err != nil {
			return nil, err
		}
		insights = append(insights, msg)
	}
	return &dashboardsv1.Dashboard{
		Id:          proto.String(dashboard.Dashboard.ID),
		ProjectId:   proto.String(dashboard.Dashboard.ProjectID),
		DisplayName: proto.String(dashboard.Dashboard.DisplayName),
		Description: proto.String(dashboard.Dashboard.Description),
		CreateTime:  toTimestamp(dashboard.Dashboard.CreateTime.Time),
		UpdateTime:  toTimestamp(dashboard.Dashboard.UpdateTime.Time),
		Insights:    insights,
	}, nil
}

func wDashboardToRPC(dashboard dbwrite.Dashboard, insights []*dashboardsv1.DashboardInsight) *dashboardsv1.Dashboard {
	return &dashboardsv1.Dashboard{
		Id:          proto.String(dashboard.ID),
		ProjectId:   proto.String(dashboard.ProjectID),
		DisplayName: proto.String(dashboard.DisplayName),
		Description: proto.String(dashboard.Description),
		CreateTime:  toTimestamp(dashboard.CreateTime.Time),
		UpdateTime:  toTimestamp(dashboard.UpdateTime.Time),
		Insights:    insights,
	}
}

func roInsightToRPC(insight dbread.DashboardInsight) (*dashboardsv1.DashboardInsight, error) {
	query, err := coreprojects.MapToQueryMessage(insight.InsightQuery)
	if err != nil {
		return nil, err
	}
	layouts, err := coreprojects.MapToLayouts(insight.Layouts)
	if err != nil {
		return nil, err
	}
	return &dashboardsv1.DashboardInsight{
		Id:          proto.String(insight.ID),
		DashboardId: proto.String(insight.DashboardID),
		DisplayName: proto.String(insight.DisplayName),
		Description: proto.String(insight.Description),
		Query:       query,
		Layouts:     layouts,
		CreateTime:  toTimestamp(insight.CreateTime.Time),
		UpdateTime:  toTimestamp(insight.UpdateTime.Time),
	}, nil
}

func wInsightToRPC(insight dbwrite.DashboardInsight) (*dashboardsv1.DashboardInsight, error) {
	query, err := coreprojects.MapToQueryMessage(insight.InsightQuery)
	if err != nil {
		return nil, err
	}
	layouts, err := coreprojects.MapToLayouts(insight.Layouts)
	if err != nil {
		return nil, err
	}
	return &dashboardsv1.DashboardInsight{
		Id:          proto.String(insight.ID),
		DashboardId: proto.String(insight.DashboardID),
		DisplayName: proto.String(insight.DisplayName),
		Description: proto.String(insight.Description),
		Query:       query,
		Layouts:     layouts,
		CreateTime:  toTimestamp(insight.CreateTime.Time),
		UpdateTime:  toTimestamp(insight.UpdateTime.Time),
	}, nil
}

func toTimestamp(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}
