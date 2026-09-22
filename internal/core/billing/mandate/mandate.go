// Package mandate owns the payment mandate lifecycle: checkout, the webhook inbox,
// reconcile, and every write to billing_subscriptions. It reads the org's
// entitlement row through the entitlement package, which owns that table.
package mandate

import (
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pug-sh/pug/internal/core/billing"
	"github.com/pug-sh/pug/internal/core/billing/entitlement"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
)

// Service is the mandate lifecycle. It holds the entitlement service rather than a
// pool of its own for the org's row: one writer per table, and the entitlement
// package is that writer.
type Service struct {
	read *dbread.Queries
	pgW  *pgxpool.Pool
	// payments is nil on a deployment with no provider credentials, which is a
	// supported mode: only the buy button is missing.
	payments *billing.Payments
	// ent is also where the billing switch is read from; mandate keeps no copy.
	ent *entitlement.Service
}

// NewService builds the lifecycle over ent, which must not be nil: the webhook
// never reads through it, so a nil one would mount, take deliveries, and panic
// only in a paid confirm or on the first reconciled row. payments may be nil.
func NewService(pgRO, pgW *pgxpool.Pool, payments *billing.Payments, ent *entitlement.Service) *Service {
	if ent == nil {
		panic("mandate: entitlement service is nil")
	}
	return &Service{
		read:     dbread.New(pgRO),
		pgW:      pgW,
		payments: payments,
		ent:      ent,
	}
}

func (s *Service) write() *dbwrite.Queries { return dbwrite.New(s.pgW) }
