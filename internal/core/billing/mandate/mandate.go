// Package mandate is what docs/architecture/payments.md calls the payments side: the
// lifecycle of a mandate, a buyer's standing authority for the provider to charge
// them, which pug mirrors as a subscription. It runs checkout, the portal, the
// webhook inbox and reconcile, and is the only writer of billing_subscriptions, the
// delivery inbox and the checkout refs, which the table-has-one-writer lint check
// holds it to.
//
// It never writes billing_entitlements, whose one writer is the entitlement
// package, but it reads that row: under the org lock through the entitlement
// service's WithOrgLock, through StoredRecord, and directly for attribution and
// the reconcile walk.
package mandate

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pug-sh/pug/internal/core/billing"
	"github.com/pug-sh/pug/internal/core/billing/entitlement"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
)

// Service is the mandate lifecycle. It holds the entitlement service for the
// billing switch, the org lock and the stored-row reads it shares with the
// dashboard; writes to billing_entitlements stay in that package.
type Service struct {
	read *dbread.Queries
	pgW  *pgxpool.Pool
	// payments is nil on a deployment with no provider credentials, which is a
	// supported mode: only the buy button is missing.
	payments     *billing.Payments
	entitlements *entitlement.Service
}

// NewService builds the lifecycle over entitlements, which must not be nil. A nil
// one fails nothing at wiring otherwise: the webhook would mount and store
// deliveries, then panic applying the first subscription, and the dashboard's
// first billing call would panic too. payments may be nil.
func NewService(pgRO, pgW *pgxpool.Pool, payments *billing.Payments, entitlements *entitlement.Service) *Service {
	if entitlements == nil {
		panic("mandate: entitlement service is nil")
	}
	return &Service{
		read:         dbread.New(pgRO),
		pgW:          pgW,
		payments:     payments,
		entitlements: entitlements,
	}
}

// Entitlements is the entitlement service this lifecycle was built over. A caller
// that needs both takes it from here rather than beside it: the billing switch
// lives there, and two services wired apart would report two switches.
func (s *Service) Entitlements() *entitlement.Service { return s.entitlements }

// Provider is the provider this lifecycle was built with, nil when there is none.
// The webhook route mounts it from here, so the route that verifies a delivery and
// the service that stores and maps it cannot name two providers.
func (s *Service) Provider() billing.PaymentProvider {
	if !s.payments.Configured() {
		return nil
	}
	return s.payments.Provider
}

// write is the pool-backed writer: for single-statement writes, and for reads that
// must see a write that just committed (attribution, checkout refs).
func (s *Service) write() *dbwrite.Queries { return dbwrite.New(s.pgW) }
