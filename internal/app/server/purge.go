package server

import (
	"context"
	"errors"
	"log/slog"

	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/slogx"
)

// warnStalledDeletions reports project deletions stuck for over a day. The
// purge job's own alerts cannot fire when nothing runs it.
func warnStalledDeletions(ctx context.Context, hasStalled func(context.Context) (bool, error)) {
	found, err := hasStalled(ctx)
	if err == nil && found {
		err = errors.New("project deletions have been stuck for over a day; check that pug cron purge is scheduled and succeeding")
	}
	if err != nil {
		slog.ErrorContext(ctx, "deleted projects are not being erased", slogx.Error(err))
		telemetry.RecordError(ctx, err)
	}
}
