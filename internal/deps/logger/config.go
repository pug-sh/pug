package logger

import (
	"log/slog"
)

type loglevel string

// Constants for log levels
const (
	levelDebug    loglevel = "DEBUG"
	levelInfo     loglevel = "INFO"
	levelWarning  loglevel = "WARNING"
	levelError    loglevel = "ERROR"
	levelCritical loglevel = "CRITICAL"
)

type Config struct {
	Level loglevel `env:"LOG_LEVEL"`
	Mode  string   `env:"LOG_MODE"`
}

func (config Config) toSlogLevel() slog.Level {
	switch config.Level {
	case levelDebug:
		return slog.LevelDebug
	case levelInfo:
		return slog.LevelInfo
	case levelWarning:
		return slog.LevelWarn
	case levelError:
		return slog.LevelError
	case levelCritical:
		return slog.LevelError + 1
	default:
		return slog.LevelDebug
	}
}
