package billing

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/slogx"
	"github.com/rs/xid"
)

// HandleDelivery stores one verified delivery and applies it, returning only once
// the row is durable. Everything unapplicable is stored, marked processed and NOT
// retried -- eight retries fix none of it. A body pug cannot DECODE is retried.
func (s *Service) HandleDelivery(ctx context.Context, provider PaymentProvider, d Delivery) error {
	stored, err := s.write().InsertBillingWebhookDelivery(ctx, dbwrite.InsertBillingWebhookDeliveryParams{
		EventType: d.EventType,
		Payload:   storablePayload(d.RawPayload),
		Provider:  provider.Name(),
		WebhookID: d.WebhookID,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to store a billing webhook delivery", slogx.Error(err),
			slog.String("provider", provider.Name()), slog.String("webhook_id", d.WebhookID))
		telemetry.RecordError(ctx, err)
		return err
	}
	// A retry of a delivery pug has already settled. A row still unprocessed is an
	// attempt that died mid-apply, and only the provider's retry finishes it.
	if stored.ProcessedAt.Valid {
		return nil
	}

	event, err := provider.Normalize(d)
	if err != nil {
		// Retried, not rejected: a payload shape changed under us, and a redeploy inside
		// the retry window fixes it. Marking it processed would lose the replay too.
		slog.ErrorContext(ctx, "failed to normalize a billing webhook delivery", slogx.Error(err),
			slog.String("provider", provider.Name()), slog.String("webhook_id", d.WebhookID),
			slog.String("event_type", d.EventType))
		telemetry.RecordError(ctx, err)
		return err
	}
	// Payments, refunds, disputes and every type added after this was written.
	if event.IsZero() {
		return s.finishDelivery(ctx, provider, d, "")
	}
	return s.applySubscriptionEvent(ctx, provider, d, event)
}

func (s *Service) applySubscriptionEvent(
	ctx context.Context, provider PaymentProvider, d Delivery, event SubscriptionEvent,
) error {
	// Not constraint mirroring like the two below: the insert writes the Currency
	// constant, so an unguarded foreign-currency event would be STORED as USD.
	if cur := normalizeCurrency(event.Currency); cur != Currency {
		return s.rejectDelivery(ctx, provider, d, "currency",
			errors.New("subscription is billed in "+cur+", not "+Currency))
	}
	if event.Status == "" {
		return s.rejectDelivery(ctx, provider, d, "status",
			errors.New("subscription carries no status"))
	}
	// Column checks mirrored here: unguarded they fail the insert, which retries
	// eight times and then leaves the delivery stored but never processed.
	if event.ProviderCustomerID == "" {
		return s.rejectDelivery(ctx, provider, d, "customer",
			errors.New("subscription names no customer"))
	}
	if event.PriceCents < 0 {
		return s.rejectDelivery(ctx, provider, d, "price",
			errors.New("subscription carries a negative price"))
	}

	orgID, err := s.attributeDelivery(ctx, provider, event)
	if err != nil {
		// A read that failed is retryable; accepting it would lose the delivery for
		// good, because the provider only retries on a non-2xx.
		if !errors.Is(err, ErrOrgNotFound) && !errors.Is(err, ErrCustomerNotUnique) {
			slog.ErrorContext(ctx, "failed to attribute a subscription delivery", slogx.Error(err),
				slog.String("provider_sub_id", event.ProviderSubID))
			telemetry.RecordError(ctx, err)
			return err
		}
		return s.rejectDelivery(ctx, provider, d, "attribution", err)
	}

	// The org's own row, for the negotiated-deal product. Read after attribution
	// because it is the attributed org's row that a custom product must match.
	rec, err := s.StoredRecord(ctx, orgID)
	if err != nil {
		return err
	}
	planSlug, err := s.planForProduct(event.ProductID, rec)
	if err != nil {
		// A deploy is missing a product key, or an operator created a product without
		// pasting its id. Neither is fixed by retrying, and reconcile reports it.
		return s.rejectDelivery(ctx, provider, d, "product", err)
	}

	applied, err := s.write().ApplyBillingSubscription(ctx, dbwrite.ApplyBillingSubscriptionParams{
		Currency:           Currency,
		CurrentPeriodEnd:   postgres.NewOptionalTimestamptz(event.CurrentPeriodEnd),
		CurrentPeriodStart: postgres.NewOptionalTimestamptz(event.CurrentPeriodStart),
		ID:                 xid.New().String(),
		OrgID:              orgID,
		PlanSlug:           planSlug,
		PriceCents:         event.PriceCents,
		Provider:           provider.Name(),
		ProviderCustomerID: event.ProviderCustomerID,
		ProviderStatus:     event.ProviderStatus,
		ProviderSubID:      event.ProviderSubID,
		ProviderUpdatedAt:  postgres.NewTimestamptz(d.DeliveredAt),
		Status:             string(event.Status),
	})
	if err != nil {
		// The partial unique index refusing a second live subscription. Retried: this is
		// a cutover whose old cancellation has not landed, and the retry then succeeds.
		if isUniqueViolation(err) {
			slog.ErrorContext(ctx, "org already holds a live subscription", slogx.Error(err),
				slog.String("org_id", orgID), slog.String("provider_sub_id", event.ProviderSubID))
			telemetry.RecordError(ctx, err)
			return err
		}
		slog.ErrorContext(ctx, "failed to apply a subscription delivery", slogx.Error(err),
			slog.String("org_id", orgID), slog.String("provider_sub_id", event.ProviderSubID))
		telemetry.RecordError(ctx, err)
		return err
	}
	if applied == 0 {
		// The CAS rejected an out-of-order delivery: a newer one already landed.
		// Recorded on the row, or the inbox cannot tell applied from skipped.
		slog.InfoContext(ctx, "skipped a stale subscription delivery",
			slog.String("org_id", orgID), slog.String("provider_sub_id", event.ProviderSubID))
		return s.finishDelivery(ctx, provider, d, "stale: a newer delivery already applied")
	}
	return s.finishDelivery(ctx, provider, d, "")
}

// attributeDelivery places a delivery on an org: metadata.org_id first, then the
// provider customer, and that one only while it names a single org. A delivery
// resolving to no org, or to two, is never applied to a guess.
func (s *Service) attributeDelivery(ctx context.Context, provider PaymentProvider, event SubscriptionEvent) (string, error) {
	// Both reads go through the WRITE pool: a lagging replica would report "no such
	// org" for an org that just checked out, rejecting the delivery permanently.
	w := s.write()
	// Checked against orgs, not against the subscription table: a first delivery
	// beats the row into existence, and the org is what the id has to name.
	if event.OrgID != "" {
		if _, err := w.GetOrgByID(ctx, event.OrgID); err == nil {
			return event.OrgID, nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return "", err
		}
	}
	if event.ProviderCustomerID != "" {
		orgs, err := w.ListBillingSubscriptionOrgsByProviderCustomerID(ctx,
			dbwrite.ListBillingSubscriptionOrgsByProviderCustomerIDParams{
				Provider:           provider.Name(),
				ProviderCustomerID: event.ProviderCustomerID,
			})
		if err != nil {
			return "", err
		}
		if len(orgs) == 1 {
			return orgs[0], nil
		}
		if len(orgs) > 1 {
			return "", ErrCustomerNotUnique
		}
	}
	return "", ErrOrgNotFound
}

// finishDelivery marks the row processed -- for an applied delivery and every
// unapplicable one, so no row is left looking like an attempt that died.
func (s *Service) finishDelivery(ctx context.Context, provider PaymentProvider, d Delivery, reason string) error {
	n, err := s.write().MarkBillingWebhookDeliveryProcessed(ctx, dbwrite.MarkBillingWebhookDeliveryProcessedParams{
		Error:     reason,
		Provider:  provider.Name(),
		WebhookID: d.WebhookID,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to mark a billing webhook delivery processed", slogx.Error(err),
			slog.String("provider", provider.Name()), slog.String("webhook_id", d.WebhookID))
		telemetry.RecordError(ctx, err)
		return err
	}
	if n == 0 {
		// The insert at the top guarantees the row, so none here means it was deleted
		// mid-flight; reporting success would leave it to be re-applied on the retry.
		err := errors.New("billing: the delivery row vanished before it could be marked processed")
		slog.ErrorContext(ctx, "failed to mark a billing webhook delivery processed", slogx.Error(err),
			slog.String("provider", provider.Name()), slog.String("webhook_id", d.WebhookID))
		telemetry.RecordError(ctx, err)
		return err
	}
	return nil
}

// rejectDelivery records why a delivery was not applied and accepts it anyway.
// The row keeps the payload, so a fixed mapping can replay it.
func (s *Service) rejectDelivery(
	ctx context.Context, provider PaymentProvider, d Delivery, reason string, cause error,
) error {
	slog.ErrorContext(ctx, "not applying a billing webhook delivery", slogx.Error(cause),
		slog.String("provider", provider.Name()), slog.String("webhook_id", d.WebhookID),
		slog.String("event_type", d.EventType), slog.String("reason", reason))
	telemetry.RecordError(ctx, cause)
	return s.finishDelivery(ctx, provider, d, reason+": "+cause.Error())
}

// storablePayload keeps a body Postgres cannot parse storable: payload is jsonb,
// so non-JSON bytes would fail the insert and lose the delivery. A \u0000 escape
// passes json.Valid and jsonb still rejects it, so it is wrapped too.
func storablePayload(raw []byte) []byte {
	if json.Valid(raw) && !bytes.Contains(raw, []byte(`\u0000`)) {
		return raw
	}
	// Built by hand rather than marshalled: base64's alphabet needs no escaping, so
	// there is no error branch that could discard the delivery here.
	return []byte(`{"raw_base64":"` + base64.StdEncoding.EncodeToString(raw) + `"}`)
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// PruneDeliveries drops processed deliveries older than the retention window --
// the whole row, dedup key included, so a re-send after this is treated as new.
// They carry personal data pug does not otherwise store; only replay needs it.
func (s *Service) PruneDeliveries(ctx context.Context, olderThan time.Time) (int64, error) {
	n, err := s.write().PruneBillingWebhookDeliveries(ctx, postgres.NewTimestamptz(olderThan))
	if err != nil {
		slog.ErrorContext(ctx, "failed to prune billing webhook deliveries", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return 0, err
	}
	return n, nil
}

// ErrTwoLiveSubscriptions is the partial unique index refusing a second live
// subscription. Returned, not swallowed: the caller cannot tell it from the CAS's
// own skip, and on the confirm path that is a buyer who paid and holds nothing.
var ErrTwoLiveSubscriptions = errors.New("billing: this org already has a live subscription")

// applyReconciledSubscription writes a provider read through the same CAS the
// webhook uses, so the two cannot disagree about what "newer" means. false is the
// CAS refusing an older read, or a guard below -- neither is actionable.
func (s *Service) applyReconciledSubscription(
	ctx context.Context, provider PaymentProvider, orgID string,
	event SubscriptionEvent, rec Record, at time.Time,
) (bool, error) {
	// The webhook path's guards, repeated because this writer is reachable without
	// them: each would otherwise fail a column check as a raw SQLSTATE.
	if normalizeCurrency(event.Currency) != Currency || event.Status == "" ||
		event.ProviderCustomerID == "" || event.PriceCents < 0 {
		return false, nil
	}
	planSlug, err := s.planForProduct(event.ProductID, rec)
	if err != nil {
		return false, nil
	}
	applied, err := s.write().ApplyBillingSubscription(ctx, dbwrite.ApplyBillingSubscriptionParams{
		Currency:           Currency,
		CurrentPeriodEnd:   postgres.NewOptionalTimestamptz(event.CurrentPeriodEnd),
		CurrentPeriodStart: postgres.NewOptionalTimestamptz(event.CurrentPeriodStart),
		ID:                 xid.New().String(),
		OrgID:              orgID,
		PlanSlug:           planSlug,
		PriceCents:         event.PriceCents,
		Provider:           provider.Name(),
		ProviderCustomerID: event.ProviderCustomerID,
		ProviderStatus:     event.ProviderStatus,
		ProviderSubID:      event.ProviderSubID,
		ProviderUpdatedAt:  postgres.NewTimestamptz(at),
		Status:             string(event.Status),
	})
	if err != nil {
		if isUniqueViolation(err) {
			slog.ErrorContext(ctx, "found two live subscriptions for one org", slogx.Error(err),
				slog.String("org_id", orgID), slog.String("provider_sub_id", event.ProviderSubID))
			telemetry.RecordError(ctx, err)
			return false, ErrTwoLiveSubscriptions
		}
		slog.ErrorContext(ctx, "failed to apply a reconciled subscription", slogx.Error(err),
			slog.String("org_id", orgID), slog.String("provider_sub_id", event.ProviderSubID))
		telemetry.RecordError(ctx, err)
		return false, err
	}
	return applied > 0, nil
}
