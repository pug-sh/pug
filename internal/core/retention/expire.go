package retention

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pug-sh/pug/internal/core/billing/entitlement"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
)

// Expired is what an expire started: the org keeps Days until AppliesAt, then
// PendingDays, unless it pays again first.
type Expired struct {
	Days        int64
	PendingDays int64
	AppliesAt   time.Time
}

// Expire starts the 30-day wait on a drop that waits for an operator: an org that
// stopped paying keeps its paid length until then. It refuses an org that still
// pays, or has nothing waiting.
func (s *Service) Expire(ctx context.Context, orgID, actor string, now time.Time) (Expired, error) {
	if strings.TrimSpace(actor) == "" {
		return Expired{}, ErrActorRequired
	}
	ent, err := s.ents.GetEntitlement(ctx, orgID, now)
	if err != nil {
		return Expired{}, err
	}
	if ent.Status == entitlement.StatusActive {
		return Expired{}, ErrOrgPays
	}
	row, err := s.write.ExpireRetentionState(ctx, dbwrite.ExpireRetentionStateParams{
		ExpiredBy:    postgres.NewText(actor),
		OrgID:        orgID,
		PendingSince: postgres.NewTimestamptz(now),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Expired{}, ErrNothingWaiting
	}
	if err != nil {
		return Expired{}, failed(ctx, "expire org "+orgID, err)
	}
	got := Expired{Days: row.Days.Int64, PendingDays: row.PendingDays.Int64, AppliesAt: now.Add(wait)}
	slog.InfoContext(ctx, "retention expired", slog.String("org_id", orgID), slog.String("actor", actor),
		daysAttr("days", got.Days), daysAttr("pending_days", got.PendingDays), slog.Time("applies_at", got.AppliesAt))
	return got, nil
}
