package postgres

import (
	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TextToString converts a pgtype.Text to a Go string
func TextToString(text pgtype.Text) (string, bool) {
	if !text.Valid {
		return "", false
	}
	return text.String, true
}

// StringToText converts a Go string to a pgtype.Text
func StringToText(s string) pgtype.Text {
	return pgtype.Text{String: s, Valid: true}
}

// NullString creates a pgtype.Text that can be null
func NullString(s string) pgtype.Text {
	return pgtype.Text{String: s, Valid: s != ""}
}

// TimestampToTimestamptz converts a protobuf timestamp to a pgtype.Timestamptz
func TimestampToTimestamptz(ts *timestamppb.Timestamp) pgtype.Timestamptz {
	if ts == nil {
		return pgtype.Timestamptz{Valid: false}
	}
	return pgtype.Timestamptz{Time: ts.AsTime(), Valid: true}
}

// TimestamptzToTimestamp converts a pgtype.Timestamptz to a protobuf timestamp
func TimestamptzToTimestamp(tz pgtype.Timestamptz) *timestamppb.Timestamp {
	if !tz.Valid {
		return nil
	}
	return timestamppb.New(tz.Time)
}
