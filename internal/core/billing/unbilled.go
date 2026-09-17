package billing

import (
	"context"
	"log/slog"
	"time"

	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/slogx"
)

// UnbilledUsage counts the orgs the close leaves out whose latest closed period went
// over the current card's allowance, and logs what that usage would have cost on it.
func (s *Service) UnbilledUsage(ctx context.Context, now time.Time, grace time.Duration) (int, error) {
	if !s.billingEnabled {
		return 0, nil
	}
	card := CurrentCard()
	counts, err := s.read.ListUnbilledUsage(ctx, dbread.ListUnbilledUsageParams{
		ClosedBefore: postgres.NewTimestamptz(now.Add(-grace)),
		FreeEvents:   card.FreeEvents,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to list the unbilled usage", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return 0, err
	}
	if len(counts) == 0 {
		return 0, nil
	}
	var cents int64
	for _, events := range counts {
		cents += Price(card, events).TotalCents
	}
	slog.InfoContext(ctx, "usage over the free allowance that nothing bills",
		slog.Int("orgs", len(counts)), slog.Int64("cents", cents), slog.String("card", card.Slug))
	return len(counts), nil
}
