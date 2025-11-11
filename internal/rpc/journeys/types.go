// Package journeys provides the gRPC/Connect handlers for the journeys service.
package journeys

import (
	journeysv1 "github.com/fivebitsio/cotton/internal/gen/proto/journeys/v1"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/fivebitsio/cotton/pkg/postgres"
)

func roToRPCMsg(j dbread.Journey) *journeysv1.Journey {
	description, _ := postgres.TextToString(j.Description)
	return &journeysv1.Journey{
		Id:          j.ID,
		ProjectId:   j.ProjectID,
		Name:        j.Name,
		Description: description,
		State:       ParseStateFromDB(j.State),
		EntryType:   ParseEntryTypeFromDB(j.EntryType),
		StartTime:   postgres.TimestamptzToTimestamp(j.StartTime),
		EndTime:     postgres.TimestamptzToTimestamp(j.EndTime),
		CreateTime:  postgres.TimestamptzToTimestamp(j.CreateTime),
		UpdateTime:  postgres.TimestamptzToTimestamp(j.UpdateTime),
	}
}

func wToRPCMsg(j dbwrite.Journey) *journeysv1.Journey {
	description, _ := postgres.TextToString(j.Description)
	return &journeysv1.Journey{
		Id:          j.ID,
		ProjectId:   j.ProjectID,
		Name:        j.Name,
		Description: description,
		State:       ParseStateFromDB(j.State),
		EntryType:   ParseEntryTypeFromDB(j.EntryType),
		StartTime:   postgres.TimestamptzToTimestamp(j.StartTime),
		EndTime:     postgres.TimestamptzToTimestamp(j.EndTime),
		CreateTime:  postgres.TimestamptzToTimestamp(j.CreateTime),
		UpdateTime:  postgres.TimestamptzToTimestamp(j.UpdateTime),
	}
}

func ParseStateFromDB(state string) journeysv1.State {
	switch state {
	case "active":
		return journeysv1.State_STATE_ACTIVE
	case "draft":
		return journeysv1.State_STATE_DRAFT
	case "paused":
		return journeysv1.State_STATE_PAUSED
	case "archived":
		return journeysv1.State_STATE_ARCHIVED
	default:
		return journeysv1.State_STATE_UNSPECIFIED
	}
}

func ParseEntryTypeFromDB(entryType string) journeysv1.EntryType {
	switch entryType {
	case "segment":
		return journeysv1.EntryType_ENTRY_TYPE_SEGMENT
	case "event":
		return journeysv1.EntryType_ENTRY_TYPE_EVENT
	default:
		return journeysv1.EntryType_ENTRY_TYPE_UNSPECIFIED
	}
}
