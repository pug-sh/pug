package slogx

import (
	"log/slog"
)

// Redacted returns a slog.Attr with "[REDACTED]" if the value is non-nil and non-empty-string,
// or "[EMPTY]" if the value is nil or an empty string.
// Use this for logging keys that reference sensitive data (API keys, credentials, tokens).
func Redacted(key string, value any) slog.Attr {
	if value == nil {
		return slog.String(key, "[EMPTY]")
	}
	if s, ok := value.(string); ok && s == "" {
		return slog.String(key, "[EMPTY]")
	}
	return slog.String(key, "[REDACTED]")
}
