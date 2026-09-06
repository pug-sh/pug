package billing

import (
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

// HandleDelivery stores one verified delivery and applies it, returning only
// once the row is durable -- the caller answers 2xx on a nil error, and the
// provider's retry is what a non-nil error asks for.
//
// The disposition of everything unapplicable is the same and is deliberate:
// stored, marked processed, logged, and NOT retried. A currency pug cannot
// render, a product it cannot place, a delivery it cannot attribute and an event
// type it does not handle are all things eight retries cannot fix, and retrying
// would only delay the alert.
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
	// A retry of a delivery that already applied. The one case this must NOT
	// short-circuit is a row still unprocessed: that is an attempt that died
	// mid-apply, and the provider's retry is the only thing that finishes it.
	if stored.ProcessedAt.Valid {
		return nil
	}

	event, err := provider.Normalize(d)
	if err != nil {
		return s.rejectDelivery(ctx, provider, d, "normalize", err)
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
	if cur := normalizeCurrency(event.Currency); cur != Currency {
		return s.rejectDelivery(ctx, provider, d, "currency",
			errors.New("subscription is billed in "+cur+", not "+Currency))
	}
	if event.Status == "" {
		return s.rejectDelivery(ctx, provider, d, "status",
			errors.New("subscription carries no status"))
	}

	orgID, err := s.attributeDelivery(ctx, provider, event)
	if err != nil {
		// A read that failed is retryable; accepting it would lose the delivery for
		// good, because the provider only retries on a non-2xx.
		if !errors.Is(err, ErrOrgNotFound) {
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
		// The partial unique index refusing a second live subscription for one org.
		// A real inconsistency that needs a person, and eight retries will not make
		// the other row go away.
		if isUniqueViolation(err) {
			return s.rejectDelivery(ctx, provider, d, "two_live_subscriptions", err)
		}
		slog.ErrorContext(ctx, "failed to apply a subscription delivery", slogx.Error(err),
			slog.String("org_id", orgID), slog.String("provider_sub_id", event.ProviderSubID))
		telemetry.RecordError(ctx, err)
		return err
	}
	if applied == 0 {
		// The CAS rejected an out-of-order delivery. Deliveries are unordered and each
		// carries the latest object, so a newer one has already landed.
		slog.DebugContext(ctx, "skipped a stale subscription delivery",
			slog.String("org_id", orgID), slog.String("provider_sub_id", event.ProviderSubID))
	}
	return s.finishDelivery(ctx, provider, d, "")
}

// attributeDelivery places a delivery on an org: metadata.org_id first, since
// pug sets it on every checkout it starts, then the provider customer. A
// delivery that resolves to no org is never applied to a guess.
func (s *Service) attributeDelivery(ctx context.Context, provider PaymentProvider, event SubscriptionEvent) (string, error) {
	// Both reads go through the WRITE pool. Against a real replica a lagging read
	// would report "no such org" for an org that just checked out, and the delivery
	// would be rejected as unattributable -- permanently, since the rejection marks
	// it processed.
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
		row, err := w.GetBillingSubscriptionByProviderCustomerID(ctx,
			dbwrite.GetBillingSubscriptionByProviderCustomerIDParams{
				Provider:           provider.Name(),
				ProviderCustomerID: event.ProviderCustomerID,
			})
		if err == nil {
			return row.OrgID, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", err
		}
	}
	return "", ErrOrgNotFound
}

// finishDelivery marks the row processed. Called for an applied delivery and for
// every unapplicable one, so a stored row is never left looking like an attempt
// that died mid-apply.
func (s *Service) finishDelivery(ctx context.Context, provider PaymentProvider, d Delivery, reason string) error {
	err := s.write().MarkBillingWebhookDeliveryProcessed(ctx, dbwrite.MarkBillingWebhookDeliveryProcessedParams{
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

// storablePayload keeps a body Postgres cannot parse storable. payload is jsonb,
// so raw bytes that are not JSON would fail the insert and lose the delivery
// entirely -- which is the one thing the inbox exists to prevent.
func storablePayload(raw []byte) []byte {
	if json.Valid(raw) {
		return raw
	}
	wrapped, err := json.Marshal(map[string]string{"raw_base64": base64.StdEncoding.EncodeToString(raw)})
	if err != nil {
		return []byte(`{"raw_base64":""}`)
	}
	return wrapped
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// PruneDeliveries drops delivery payloads older than the retention window. They
// carry the customer's name, email and billing address -- personal data pug does
// not otherwise store -- and replay is the only thing that needs the bytes.
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
// subscription for one org. Returned rather than swallowed because the caller
// cannot tell it from the CAS's own skip, and on the confirm path that
// difference is a buyer who paid and holds nothing.
var ErrTwoLiveSubscriptions = errors.New("billing: this org already has a live subscription")

// applyReconciledSubscription writes a provider read through the same CAS the
// webhook uses, so the two cannot disagree about what "newer" means. Reports
// whether the write landed; false is the CAS refusing a read older than a
// delivery that arrived while the pass was running.
func (s *Service) applyReconciledSubscription(
	ctx context.Context, provider PaymentProvider, orgID string,
	event SubscriptionEvent, rec Record, at time.Time,
) (bool, error) {
	if normalizeCurrency(event.Currency) != Currency || event.Status == "" {
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
