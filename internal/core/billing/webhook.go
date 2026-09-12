package billing

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgerrcode"
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
// retried — the provider retries and fixes none of it. A body it cannot DECODE is retried.
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
		return s.undecodable(ctx, provider, d, err)
	}
	if !event.IsZero() {
		return s.applySubscriptionEvent(ctx, provider, d, event)
	}
	payment, err := provider.NormalizePayment(d)
	if err != nil {
		return s.undecodable(ctx, provider, d, err)
	}
	if payment.IsZero() {
		return s.finishDelivery(ctx, provider, d, "")
	}
	return s.applyPaymentEvent(ctx, provider, d, payment)
}

// undecodable is retried, not rejected: a payload shape changed under us, and a
// redeploy inside the retry window fixes it. Marking it processed loses the replay.
func (s *Service) undecodable(ctx context.Context, provider PaymentProvider, d Delivery, err error) error {
	slog.ErrorContext(ctx, "failed to normalize a billing webhook delivery", slogx.Error(err),
		slog.String("provider", provider.Name()), slog.String("webhook_id", d.WebhookID),
		slog.String("event_type", d.EventType))
	telemetry.RecordError(ctx, err)
	return err
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
	if event.ProviderCustomerID == "" {
		return s.rejectDelivery(ctx, provider, d, "customer",
			errors.New("subscription names no customer"))
	}
	if event.PriceCents < 0 {
		return s.rejectDelivery(ctx, provider, d, "price",
			errors.New("subscription carries a negative price"))
	}
	// A recurring subscription would be charged by the provider on its schedule AND
	// by pug's invoices.
	if !event.OnDemand {
		return s.rejectDelivery(ctx, provider, d, "on_demand",
			errors.New("subscription is not on-demand"))
	}

	orgID, err := s.attributeDelivery(ctx, provider, event)
	if err != nil {
		if !errors.Is(err, ErrOrgNotFound) && !errors.Is(err, ErrCustomerNotUnique) {
			slog.ErrorContext(ctx, "failed to attribute a subscription delivery", slogx.Error(err),
				slog.String("provider_sub_id", event.ProviderSubID))
			telemetry.RecordError(ctx, err)
			return err
		}
		return s.rejectDelivery(ctx, provider, d, "attribution", err)
	}

	applied, err := s.applySubscription(ctx, provider, orgID, event, d.DeliveredAt)
	if err != nil {
		// Retried, ErrTwoLiveSubscriptions included: that one is a cutover whose old
		// cancellation has not landed, and the retry then succeeds.
		return err
	}
	if applied == 0 {
		slog.InfoContext(ctx, "skipped a stale subscription delivery",
			slog.String("org_id", orgID), slog.String("provider_sub_id", event.ProviderSubID))
	}
	if event.PaymentMethodUpdated {
		if _, err := s.reopenDunning(ctx, orgID, "webhook:"+d.WebhookID, d.DeliveredAt); err != nil {
			return err
		}
	}
	return s.finishDelivery(ctx, provider, d, "")
}

// attributeDelivery places a delivery on an org: the ref from a checkout pug
// started, then the provider customer, and that one only while it names a single
// org. metadata.org_id is never enough on its own: static payment links let the
// buyer set it.
func (s *Service) attributeDelivery(ctx context.Context, provider PaymentProvider, event SubscriptionEvent) (string, error) {
	// Every read here goes through the WRITE pool: a lagging replica would report "no
	// such org" for an org that just checked out, rejecting the delivery permanently.
	w := s.write()
	if event.CheckoutRef != "" {
		session, err := w.GetBillingCheckoutSession(ctx, dbwrite.GetBillingCheckoutSessionParams{
			Provider: provider.Name(),
			Ref:      event.CheckoutRef,
		})
		if err == nil {
			return session.OrgID, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
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

// applyPaymentEvent settles the invoice a payment was made for, by the invoice
// id pug put in the charge's metadata. A payment carrying none is not pug's.
func (s *Service) applyPaymentEvent(
	ctx context.Context, provider PaymentProvider, d Delivery, event PaymentEvent,
) error {
	actor := "webhook:" + d.WebhookID
	if event.Refund {
		inv, err := s.invoiceByPayment(ctx, provider.Name(), event.Payment.PaymentID)
		if err != nil {
			if errors.Is(err, ErrInvoiceNotFound) {
				return s.finishDelivery(ctx, provider, d, "")
			}
			return err
		}
		_, err = s.transition(ctx, inv.ID, actor, "refunded at the provider", func(w *dbwrite.Queries) (dbwrite.BillingInvoice, error) {
			return w.MarkBillingInvoiceRefunded(ctx, inv.ID)
		})
		if err != nil && !errors.Is(err, ErrInvoiceTransition) {
			return err
		}
		return s.finishDelivery(ctx, provider, d, "")
	}
	if event.Payment.InvoiceID == "" {
		return s.finishDelivery(ctx, provider, d, "")
	}
	inv, err := s.GetInvoice(ctx, event.Payment.InvoiceID)
	if err != nil {
		if errors.Is(err, ErrInvoiceNotFound) {
			return s.rejectDelivery(ctx, provider, d, "invoice",
				errors.New("payment names an invoice pug does not have"))
		}
		return err
	}
	if inv.Provider != provider.Name() {
		return s.rejectDelivery(ctx, provider, d, "invoice",
			errors.New("payment names an invoice charged through another provider"))
	}
	if err := s.applyPaymentOutcome(ctx, inv, event.Payment, d.DeliveredAt, actor, nil); err != nil {
		return err
	}
	return s.finishDelivery(ctx, provider, d, "")
}

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
		err := errors.New("billing: the delivery row vanished before it could be marked processed")
		slog.ErrorContext(ctx, "failed to mark a billing webhook delivery processed", slogx.Error(err),
			slog.String("provider", provider.Name()), slog.String("webhook_id", d.WebhookID))
		telemetry.RecordError(ctx, err)
		return err
	}
	return nil
}

// rejectDelivery records why a delivery was not applied and accepts it anyway.
func (s *Service) rejectDelivery(
	ctx context.Context, provider PaymentProvider, d Delivery, reason string, cause error,
) error {
	slog.ErrorContext(ctx, "not applying a billing webhook delivery", slogx.Error(cause),
		slog.String("provider", provider.Name()), slog.String("webhook_id", d.WebhookID),
		slog.String("event_type", d.EventType), slog.String("reason", reason))
	telemetry.RecordError(ctx, cause)
	return s.finishDelivery(ctx, provider, d, reason+": "+cause.Error())
}

// nulEscape is the one JSON escape json.Valid accepts and jsonb still rejects.
var nulEscape = []byte{'\\', 'u', '0', '0', '0', '0'}

// storablePayload keeps a body Postgres cannot parse storable: payload is jsonb,
// so non-JSON bytes would fail the insert and lose the delivery.
func storablePayload(raw []byte) []byte {
	if json.Valid(raw) && !bytes.Contains(raw, nulEscape) {
		return raw
	}
	return []byte(`{"raw_base64":"` + base64.StdEncoding.EncodeToString(raw) + `"}`)
}

const subscriptionsOneLiveIndex = "billing_subscriptions_one_live_idx"

func isTwoLiveViolation(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == pgerrcode.UniqueViolation && pgErr.ConstraintName == subscriptionsOneLiveIndex
}

// PruneDeliveries drops deliveries and spent checkout refs older than the
// retention window. Payloads carry personal data pug does not otherwise store.
func (s *Service) PruneDeliveries(ctx context.Context, olderThan time.Time) (int64, error) {
	n, err := s.write().PruneBillingWebhookDeliveries(ctx, postgres.NewTimestamptz(olderThan))
	if err != nil {
		slog.ErrorContext(ctx, "failed to prune billing webhook deliveries", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return 0, err
	}
	if _, err := s.write().PruneBillingCheckoutSessions(ctx, postgres.NewTimestamptz(olderThan)); err != nil {
		slog.ErrorContext(ctx, "failed to prune billing checkout sessions", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return 0, err
	}
	return n, nil
}

// ErrTwoLiveSubscriptions is the partial unique index refusing a second live
// subscription.
var ErrTwoLiveSubscriptions = errors.New("billing: this org already has a live subscription")

// ErrSubscriptionUnapplicable is a provider state no writer can store: an unsold
// currency, no status, no customer, a negative price, or not on-demand.
var ErrSubscriptionUnapplicable = errors.New("billing: subscription cannot be applied")

// applySubscription is the one writer behind all three paths — webhook,
// reconcile and confirm — so they cannot disagree about what "newer" means. It
// runs under the entitlement lock, which serializes it with the operator's
// writes. 0 applied is the CAS refusing an older read.
func (s *Service) applySubscription(
	ctx context.Context, provider PaymentProvider, orgID string, event SubscriptionEvent, at time.Time,
) (int64, error) {
	if normalizeCurrency(event.Currency) != Currency || event.Status == "" ||
		event.ProviderCustomerID == "" || event.PriceCents < 0 || !event.OnDemand {
		slog.ErrorContext(ctx, "subscription cannot be applied", slogx.Error(ErrSubscriptionUnapplicable),
			slog.String("org_id", orgID), slog.String("provider_sub_id", event.ProviderSubID),
			slog.String("currency", normalizeCurrency(event.Currency)),
			slog.String("status", string(event.Status)), slog.Bool("on_demand", event.OnDemand))
		telemetry.RecordError(ctx, ErrSubscriptionUnapplicable)
		return 0, ErrSubscriptionUnapplicable
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	w := dbwrite.New(tx)
	if err := w.LockBillingEntitlementOrg(ctx, orgID); err != nil {
		slog.ErrorContext(ctx, "failed to lock the org for a subscription write", slogx.Error(err),
			slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return 0, err
	}
	planSlug, err := s.pinnedSlug(ctx, w, provider, event)
	if err != nil {
		return 0, err
	}
	applied, err := w.ApplyBillingSubscription(ctx, dbwrite.ApplyBillingSubscriptionParams{
		CancelAtPeriodEnd:  event.CancelAtPeriodEnd,
		Currency:           Currency,
		CurrentPeriodEnd:   postgres.NewOptionalTimestamptz(event.CurrentPeriodEnd),
		CurrentPeriodStart: postgres.NewOptionalTimestamptz(event.CurrentPeriodStart),
		EndedAt:            postgres.NewOptionalTimestamptz(event.EndedAt),
		ID:                 xid.New().String(),
		OnDemand:           event.OnDemand,
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
		if isTwoLiveViolation(err) {
			slog.ErrorContext(ctx, "org already holds a live subscription", slogx.Error(err),
				slog.String("org_id", orgID), slog.String("provider_sub_id", event.ProviderSubID))
			telemetry.RecordError(ctx, err)
			return 0, ErrTwoLiveSubscriptions
		}
		slog.ErrorContext(ctx, "failed to apply a subscription", slogx.Error(err),
			slog.String("org_id", orgID), slog.String("provider_sub_id", event.ProviderSubID))
		telemetry.RecordError(ctx, err)
		return 0, err
	}
	if err := s.commit(ctx, tx, orgID); err != nil {
		return 0, err
	}
	return applied, nil
}

// pinnedSlug is the card the mandate holds: the one pug wrote when it opened the
// checkout, else what the row already says, else the card current now. Pinned at
// activation, because the mandate is when the customer agreed to a price.
func (s *Service) pinnedSlug(ctx context.Context, w *dbwrite.Queries, provider PaymentProvider, event SubscriptionEvent) (string, error) {
	if event.CheckoutRef != "" {
		session, err := w.GetBillingCheckoutSession(ctx, dbwrite.GetBillingCheckoutSessionParams{
			Provider: provider.Name(),
			Ref:      event.CheckoutRef,
		})
		if err == nil {
			return session.PlanSlug, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.ErrorContext(ctx, "failed to read the checkout session for a subscription write", slogx.Error(err),
				slog.String("provider_sub_id", event.ProviderSubID))
			telemetry.RecordError(ctx, err)
			return "", err
		}
	}
	stored, err := w.GetBillingSubscriptionPlanSlug(ctx, dbwrite.GetBillingSubscriptionPlanSlugParams{
		Provider:      provider.Name(),
		ProviderSubID: event.ProviderSubID,
	})
	if err == nil {
		return stored, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		slog.ErrorContext(ctx, "failed to read the stored plan slug for a subscription write", slogx.Error(err),
			slog.String("provider_sub_id", event.ProviderSubID))
		telemetry.RecordError(ctx, err)
		return "", err
	}
	return CurrentCard().Slug, nil
}
