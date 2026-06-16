package postgres

import (
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func NewText(s string) pgtype.Text {
	return pgtype.Text{String: s, Valid: true}
}

func NewOptionalText(s string) pgtype.Text {
	return pgtype.Text{String: s, Valid: s != ""}
}

// NewNullableText maps a presence-tracked optional string to pgtype.Text for
// partial-update queries. A nil pointer becomes SQL NULL (Valid:false) so a
// coalesce(param, column) preserves the existing column value; a non-nil pointer —
// EVEN an empty string — becomes a present value (Valid:true) that overwrites it.
// That is what lets a caller distinguish "field omitted" from "field set to empty".
func NewNullableText(s *string) pgtype.Text {
	if s == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *s, Valid: true}
}

func NewTimestamptz(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

func NewOptionalTimestamptz(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: !t.IsZero()}
}

// TimestampToTimestamptz converts a proto timestamp to pgtype, returning Valid: false for nil.
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
