package slogx

import (
	"log/slog"
)

const (
	errorKey = "error"
)

func Error(err error) slog.Attr {
	return slog.Any(errorKey, err)
}
