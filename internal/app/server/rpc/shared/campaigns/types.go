package campaigns

import (
	"encoding/json"

	"google.golang.org/protobuf/proto"

	"github.com/pug-sh/pug/internal/deps/postgres"
	campaignsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/campaigns/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
)

// wToRPCMsg and roToRPCMsg must be kept in sync — they convert
// the write and read models to the same proto message.
func wToRPCMsg(c dbwrite.Campaign) (*campaignsv1.Campaign, error) {
	notificationData, err := json.Marshal(c.NotificationData)
	if err != nil {
		return nil, err
	}
	return &campaignsv1.Campaign{
		CreateTime:       postgres.TimestamptzToTimestamp(c.CreateTime),
		EndTime:          postgres.TimestamptzToTimestamp(c.EndTime),
		Id:               proto.String(c.ID),
		Name:             proto.String(c.Name),
		NotificationData: notificationData,
		ProjectId:        proto.String(c.ProjectID),
		ScheduledTime:    postgres.TimestamptzToTimestamp(c.ScheduledTime),
		StartTime:        postgres.TimestamptzToTimestamp(c.StartTime),
		Status:           proto.String(c.Status),
		UpdateTime:       postgres.TimestamptzToTimestamp(c.UpdateTime),
	}, nil
}

func roToRPCMsg(c dbread.Campaign) (*campaignsv1.Campaign, error) {
	notificationData, err := json.Marshal(c.NotificationData)
	if err != nil {
		return nil, err
	}
	return &campaignsv1.Campaign{
		CreateTime:       postgres.TimestamptzToTimestamp(c.CreateTime),
		EndTime:          postgres.TimestamptzToTimestamp(c.EndTime),
		Id:               proto.String(c.ID),
		Name:             proto.String(c.Name),
		NotificationData: notificationData,
		ProjectId:        proto.String(c.ProjectID),
		ScheduledTime:    postgres.TimestamptzToTimestamp(c.ScheduledTime),
		StartTime:        postgres.TimestamptzToTimestamp(c.StartTime),
		Status:           proto.String(c.Status),
		UpdateTime:       postgres.TimestamptzToTimestamp(c.UpdateTime),
	}, nil
}
