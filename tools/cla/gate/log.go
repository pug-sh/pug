package main

import (
	"log/slog"
	"os"
)

// errAttr mirrors internal/slogx.Error. This module is deliberately stdlib-only
// (see go.mod), so the repo's helper cannot be imported.
func errAttr(err error) slog.Attr { return slog.Any("error", err) }

// setupLogging sends diagnostics to stderr, leaving stdout for the contributor
// report and the workflow commands GitHub parses line by line. Re-running a job
// with debug logging sets RUNNER_DEBUG.
func setupLogging() {
	level := slog.LevelInfo
	if os.Getenv("RUNNER_DEBUG") == "1" {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
}
