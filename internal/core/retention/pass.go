package retention

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/pug-sh/pug/internal/core/billing"
	"github.com/pug-sh/pug/internal/core/billing/entitlement"
)

// Pass moves every org's retention state on a run, then deletes the history past
// each org's cut, or with the switch off logs what it would delete. An org that
// fails is left uncut and fails the pass; the next day's run retries it.
func (s *Service) Pass(ctx context.Context, now time.Time) error {
	if s.ch == nil {
		return ErrNoClickHouse
	}
	slog.InfoContext(ctx, "retention pass", slog.Bool("enabled", s.cfg.Enabled))
	orgs, err := s.read.ListOrgIDs(ctx)
	if err != nil {
		return failed(ctx, "list orgs", err)
	}
	rows, err := s.read.ListRetentionStates(ctx)
	if err != nil {
		return failed(ctx, "list retention states", err)
	}
	states := make(map[string]state, len(rows))
	for _, row := range rows {
		states[row.OrgID] = stateFromRow(row)
	}

	var errs []error
	cuts := map[string]time.Time{}
	for _, orgID := range orgs {
		// Else a pass past its deadline reports every org left as a failed read.
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, failed(ctx, "cut short", err))...)
		}
		days, err := s.advance(ctx, orgID, states[orgID], now)
		if err != nil {
			errs = append(errs, fmt.Errorf("org %s: %w", orgID, err))
			continue
		}
		if at, ok := cut(now, days); ok {
			cuts[orgID] = at
		}
	}
	if err := s.deleteBefore(ctx, cuts); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// advance moves one org's state a run on, and returns the length its deletes use.
func (s *Service) advance(ctx context.Context, orgID string, cur state, now time.Time) (int64, error) {
	ent, err := s.ents.GetEntitlement(ctx, orgID, now)
	if err != nil {
		return 0, err
	}
	var days int64
	if ent.RetentionDays != nil {
		days = *ent.RetentionDays
	}
	lapsed := false
	if cur.drops(days) {
		if lapsed, err = s.lapsed(ctx, orgID, ent); err != nil {
			return 0, err
		}
	}
	next := cur.next(days, lapsed, now)
	if next != cur {
		if err := s.write.UpsertRetentionState(ctx, next.params(orgID)); err != nil {
			return 0, failed(ctx, "store the retention state of org "+orgID, err)
		}
	}

	org := slog.String("org_id", orgID)
	if next.days != cur.days {
		slog.InfoContext(ctx, "retention length applies", org, daysAttr("days", next.days), daysAttr("was", cur.days),
			slog.String("expired_by", cur.expiredBy))
	}
	switch {
	case next.pendingDays == 0:
	case next.pendingSince.IsZero():
		slog.InfoContext(ctx, "retention drop waits for an expire", org,
			daysAttr("days", next.days), daysAttr("pending_days", next.pendingDays))
	default:
		slog.InfoContext(ctx, "retention drop waits", org,
			daysAttr("days", next.days), daysAttr("pending_days", next.pendingDays),
			slog.Time("applies_at", next.pendingSince.Add(wait)))
	}
	return next.days, nil
}

// lapsed reports a drop to free's length after every subscription the org had
// has ended. Paid history then goes only once an operator expires it.
func (s *Service) lapsed(ctx context.Context, orgID string, ent entitlement.Entitlement) (bool, error) {
	if !ent.BillingEnabled || ent.Status != entitlement.StatusFree {
		return false, nil
	}
	rec, err := s.ents.StoredRecord(ctx, orgID)
	if err != nil {
		return false, err
	}
	// An operator's own length, not free's.
	if rec.RetentionDaysOverride > 0 {
		return false, nil
	}
	subs, err := s.read.ListBillingSubscriptionsByOrg(ctx, orgID)
	if err != nil {
		return false, failed(ctx, "list the subscriptions of org "+orgID, err)
	}
	// A failed subscription never took a payment.
	for _, sub := range subs {
		if billing.SubStatus(sub.Status) != billing.SubStatusFailed {
			return true, nil
		}
	}
	return false, nil
}
