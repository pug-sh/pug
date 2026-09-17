package billing

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/slogx"
)

// MaxChargeAttempts is the charges an invoice gets before a soft decline is final. A
// new payment method buys one more.
const MaxChargeAttempts = 4

// codeUnsettled is a charge no read could find a payment for on its last attempt.
const codeUnsettled = "unsettled"

// retryAfter is the provider's recommended schedule: three days after the first
// attempt, then seven after each later one.
func retryAfter(attempts int) time.Duration {
	if attempts <= 1 {
		return 3 * 24 * time.Hour
	}
	return 7 * 24 * time.Hour
}

// hardDecline is a code the provider says never to retry: it only damages
// authorization rates.
func hardDecline(code string) bool {
	switch code {
	case "STOLEN_CARD", "LOST_CARD", "PICKUP_CARD", "DO_NOT_HONOR", "FRAUDULENT", "AUTHENTICATION_FAILURE":
		return true
	}
	return false
}

// decline records the card refusing a charge: retried on the schedule, or
// uncollectible once the code is hard or the attempts are spent, which is final.
func (s *Service) decline(
	ctx context.Context, orgID, invoiceID string, from InvoiceStatus, p Payment, now time.Time, actor string,
) (moved, final bool, err error) {
	moved, err = s.moveInvoice(ctx, orgID, invoiceID, actor, strings.TrimSpace("declined "+p.ErrorCode),
		func(w *dbwrite.Queries) (dbwrite.BillingInvoice, error) {
			cur, err := w.GetBillingInvoiceAttempts(ctx, invoiceID)
			if err != nil {
				return dbwrite.BillingInvoice{}, err
			}
			attempts := int(cur.Attempts)
			if from == InvoiceCharging {
				attempts++
			}
			final = hardDecline(p.ErrorCode) || attempts >= MaxChargeAttempts
			to, next := InvoiceFailed, postgres.NewTimestamptz(now.Add(retryAfter(attempts)))
			if final {
				to, next = InvoiceUncollectible, pgtype.Timestamptz{}
			}
			return w.DeclineBillingInvoice(ctx, dbwrite.DeclineBillingInvoiceParams{
				FailedAt:          postgres.NewTimestamptz(now),
				FromStatus:        string(from),
				ID:                invoiceID,
				LastErrorCode:     p.ErrorCode,
				LastErrorMessage:  p.ErrorMessage,
				NextAttemptAt:     next,
				ProviderPaymentID: postgres.NewOptionalText(p.PaymentID),
				Status:            string(to),
			})
		})
	return moved, final, err
}

// reopenDunning gives every failed and uncollectible invoice another charge now:
// the org has a new payment method.
func (s *Service) reopenDunning(ctx context.Context, orgID, actor string, now time.Time) error {
	ids, err := s.write().ListDunningBillingInvoices(ctx, orgID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to list the invoices a new card reopens", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return err
	}
	for _, id := range ids {
		if _, err := s.moveInvoice(ctx, orgID, id, actor, "payment method updated",
			func(w *dbwrite.Queries) (dbwrite.BillingInvoice, error) {
				return w.ReopenDunningBillingInvoice(ctx, dbwrite.ReopenDunningBillingInvoiceParams{
					ID: id, NextAttemptAt: postgres.NewTimestamptz(now),
				})
			}); err != nil {
			return err
		}
	}
	return nil
}
