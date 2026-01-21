package campaigns

import (
	campaignsv1 "github.com/fivebitsio/cotton/internal/gen/proto/campaigns/v1"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/fivebitsio/cotton/pkg/postgres"
)

func wToRPCMsg(campaign dbwrite.Campaign) *campaignsv1.Campaign {
	return &campaignsv1.Campaign{
		CreateTime:       postgres.TimestamptzToTimestamp(campaign.CreateTime),
		EndTime:          postgres.TimestamptzToTimestamp(campaign.EndTime),
		Id:               campaign.ID,
		Name:             campaign.Name,
		NotificationData: campaign.NotificationData,
		ProjectId:        campaign.ProjectID,
		ScheduledTime:    postgres.TimestamptzToTimestamp(campaign.ScheduledTime),
		StartTime:        postgres.TimestamptzToTimestamp(campaign.StartTime),
		Status:           campaign.Status,
		UpdateTime:       postgres.TimestamptzToTimestamp(campaign.UpdateTime),
	}
}

func roToRPCMsg(campaign dbread.Campaign) *campaignsv1.Campaign {
	return &campaignsv1.Campaign{
		CreateTime:       postgres.TimestamptzToTimestamp(campaign.CreateTime),
		EndTime:          postgres.TimestamptzToTimestamp(campaign.EndTime),
		Id:               campaign.ID,
		Name:             campaign.Name,
		NotificationData: campaign.NotificationData,
		ProjectId:        campaign.ProjectID,
		ScheduledTime:    postgres.TimestamptzToTimestamp(campaign.ScheduledTime),
		StartTime:        postgres.TimestamptzToTimestamp(campaign.StartTime),
		Status:           campaign.Status,
		UpdateTime:       postgres.TimestamptzToTimestamp(campaign.UpdateTime),
	}
}
