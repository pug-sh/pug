package postgres

import (
	"github.com/jackc/pgx/v5/pgtype"
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
