package postgres

import (
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func NewText(s string) pgtype.Text {
	return pgtype.Text{String: s, Valid: true}
}

func NewTimestampTZ(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: !t.IsZero()}
}

// TimestamptzToTimestamp converts a pgtype.Timestamptz to a protobuf timestamp
func TimestamptzToTimestamp(tz pgtype.Timestamptz) *timestamppb.Timestamp {
	if !tz.Valid {
		return nil
	}
	return timestamppb.New(tz.Time)
}
