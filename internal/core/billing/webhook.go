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
	// and then leaves the delivery stored but never processed.
	if event.ProviderCustomerID == "" {
		return s.rejectDelivery(ctx, provider, d, "customer",
			errors.New("subscription names no customer"))
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

	applied, err := s.applySubscription(ctx, provider, orgID, event, d.DeliveredAt)
	if err != nil {
		// A deploy is missing a product key, or an operator created a product without
		// pasting its id. Neither is fixed by retrying, and reconcile reports it.
		if errors.Is(err, ErrNotPurchasable) {
			return s.rejectDelivery(ctx, provider, d, "product", err)
		}
		// Everything else is retried, ErrTwoLiveSubscriptions included: that one is a
		// cutover whose old cancellation has not landed, and the retry then succeeds.
		return err
	}
	if applied == 0 {
		// The CAS rejected an out-of-order delivery: a newer one already landed. The
		// row is finished with NO error -- the provider guarantees no ordering, so
		// this is ordinary, and reconcile reports a non-empty error as a lost payment.
		slog.InfoContext(ctx, "skipped a stale subscription delivery",
			slog.String("org_id", orgID), slog.String("provider_sub_id", event.ProviderSubID))
	}
	return s.finishDelivery(ctx, provider, d, "")
}

// attributeDelivery places a delivery on an org: the ref from a checkout pug
// started, then the provider customer, and that one only while it names a single
// org. A delivery resolving to no org, or to two, is never applied to a guess.
//
// metadata.org_id is never enough on its own, and there is no longer a staged
// product to pair it with: static payment links let the buyer set metadata_* from
// the URL, so an org id in a payload names an org rather than proving one.
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
		// A ref nobody minted is worth nothing, not a rejection: a payment link has none.
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

// finishDelivery marks the row processed — for an applied delivery and every
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

// The one-live index is the only unique constraint an apply can trip: the primary
// key gets a fresh xid, and (provider, provider_sub_id) is the conflict target. A
// bare 23505 catch would report a future index as a second live subscription, and
// that one is retried to the DLQ rather than rejected.
const subscriptionsOneLiveIndex = "billing_subscriptions_one_live_idx"

func isTwoLiveViolation(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == pgerrcode.UniqueViolation && pgErr.ConstraintName == subscriptionsOneLiveIndex
}

// PruneDeliveries drops deliveries and spent checkout refs older than the
// retention window. For deliveries that is the whole
// row, dedup key included, so a re-send after this is treated as new. They carry
// personal data pug does not otherwise store; only replay needs it. One that
// never processed is dated from its arrival, or a body pug cannot decode would
// keep its payload forever.
func (s *Service) PruneDeliveries(ctx context.Context, olderThan time.Time) (int64, error) {
	n, err := s.write().PruneBillingWebhookDeliveries(ctx, postgres.NewTimestamptz(olderThan))
	if err != nil {
		slog.ErrorContext(ctx, "failed to prune billing webhook deliveries", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return 0, err
	}
	// Same window: past it a ref has either produced its subscription row, which
	// carries attribution from then on, or belongs to an abandoned checkout.
	if _, err := s.write().PruneBillingCheckoutSessions(ctx, postgres.NewTimestamptz(olderThan)); err != nil {
		slog.ErrorContext(ctx, "failed to prune billing checkout sessions", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return 0, err
	}
	return n, nil
}

// ErrTwoLiveSubscriptions is the partial unique index refusing a second live
// subscription. Returned, not swallowed: the caller cannot tell it from the CAS's
// own skip, and on the confirm path that is a buyer who paid and holds nothing.
var ErrTwoLiveSubscriptions = errors.New("billing: this org already has a live subscription")

// ErrSubscriptionUnapplicable is a provider state no writer can store: an unsold
// currency, no status, or no customer. Returned rather than
// reported as a skip, or a pass counts neither an apply nor a finding.
var ErrSubscriptionUnapplicable = errors.New("billing: subscription cannot be applied")

// applySubscription is the one writer behind all three paths — webhook,
// reconcile and confirm — so they cannot disagree about what "newer" means. 0
// applied is the CAS refusing an older read. It takes no entitlement lock: a
// deal's terms live on the org's row and the mandate no longer resolves anything
// from it, so there is nothing for a concurrent `billing clear` to strand.
func (s *Service) applySubscription(
	ctx context.Context, provider PaymentProvider, orgID string, event SubscriptionEvent, at time.Time,
) (int64, error) {
	// Mirrors the column checks: unguarded they fail the insert. Logged here because
	// the confirm path reaches it with a buyer already charged.
	if normalizeCurrency(event.Currency) != Currency || event.Status == "" ||
		event.ProviderCustomerID == "" {
		slog.ErrorContext(ctx, "subscription cannot be applied", slogx.Error(ErrSubscriptionUnapplicable),
			slog.String("org_id", orgID), slog.String("provider_sub_id", event.ProviderSubID),
			slog.String("currency", normalizeCurrency(event.Currency)),
			slog.String("status", string(event.Status)))
		telemetry.RecordError(ctx, ErrSubscriptionUnapplicable)
		return 0, ErrSubscriptionUnapplicable
	}
	planSlug, err := s.pinnedCard(ctx, provider, event)
	if err != nil {
		return 0, err
	}
	applied, err := s.write().ApplyBillingSubscription(ctx, dbwrite.ApplyBillingSubscriptionParams{
		Currency:           Currency,
		CurrentPeriodEnd:   postgres.NewOptionalTimestamptz(event.CurrentPeriodEnd),
		CurrentPeriodStart: postgres.NewOptionalTimestamptz(event.CurrentPeriodStart),
		ID:                 xid.New().String(),
		OrgID:              orgID,
		PlanSlug:           planSlug,
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
	return applied, nil
}

// pinnedCard is the card this mandate is priced on: the one its checkout pinned,
// else the one already stored, else the current card. A delivery's PRODUCT
// decides nothing — every org authorizes against the same one — and a renewal or
// cancellation carries no ref, so re-resolving would move a grandfathered price.
func (s *Service) pinnedCard(ctx context.Context, provider PaymentProvider, event SubscriptionEvent) (string, error) {
	w := s.write()
	if event.CheckoutRef != "" {
		session, err := w.GetBillingCheckoutSession(ctx, dbwrite.GetBillingCheckoutSessionParams{
			Provider: provider.Name(),
			Ref:      event.CheckoutRef,
		})
		if err == nil {
			return session.PlanSlug, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.ErrorContext(ctx, "failed to read the checkout session for a subscription", slogx.Error(err),
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
		slog.ErrorContext(ctx, "failed to read the stored plan slug for a subscription", slogx.Error(err),
			slog.String("provider_sub_id", event.ProviderSubID))
		telemetry.RecordError(ctx, err)
		return "", err
	}
	// A payment link pug never opened a checkout for: the buyer saw the current
	// card, because that is the only one on sale.
	return CurrentCard().Slug, nil
}
