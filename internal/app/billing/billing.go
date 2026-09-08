// Package billing is the operator CLI behind `pug billing`: the only writer of
// billing_entitlements. Postgres only — no provider and no network — and ungated
// by PUG_BILLING_ENABLED, which decides only how the resolved half is reported.
package billing

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	corebilling "github.com/pug-sh/pug/internal/core/billing"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/sethvargo/go-envconfig"
)

// CLI is the billing commands and the pools they run against.
type CLI struct {
	svc  *corebilling.Service
	pgRO *pgxpool.Pool
	pgW  *pgxpool.Pool
}

// New builds its own pools rather than taking them: this binary runs one command
// and exits, so there is nothing else to share them with.
func New(ctx context.Context) (*CLI, error) {
	var billingCfg corebilling.Config
	if err := envconfig.Process(ctx, &billingCfg); err != nil {
		return nil, fmt.Errorf("billing config: %w", err)
	}

	var pgCfg postgres.Config
	if err := envconfig.Process(ctx, &pgCfg); err != nil {
		return nil, fmt.Errorf("postgres config: %w", err)
	}
	pgRO, err := postgres.NewReaderPool(ctx, &pgCfg)
	if err != nil {
		return nil, fmt.Errorf("postgres reader pool: %w", err)
	}

	pgW, err := postgres.NewWriterPool(ctx, &pgCfg)
	if err != nil {
		pgRO.Close()
		return nil, fmt.Errorf("postgres writer pool: %w", err)
	}

	// No payments: this CLI never talks to a provider, and a nil Payments is the
	// same supported shape a deployment without credentials runs in.
	svc, err := corebilling.NewService(pgRO, pgW, billingCfg.Enabled, nil)
	if err != nil {
		pgRO.Close()
		pgW.Close()
		return nil, fmt.Errorf("billing service: %w", err)
	}
	return &CLI{svc: svc, pgRO: pgRO, pgW: pgW}, nil
}

func (c *CLI) Close() {
	c.pgRO.Close()
	c.pgW.Close()
}

// Show prints the resolved entitlement and the stored row beneath it: a lapsed
// deal's quota is invisible in the resolved answer and still carries onto a set.
func (c *CLI) Show(ctx context.Context, out io.Writer, orgID string, history bool) error {
	rec, err := c.svc.StoredRecord(ctx, orgID)
	if err != nil {
		return err
	}
	var entries []corebilling.HistoryEntry
	if history {
		if entries, err = c.svc.History(ctx, orgID); err != nil {
			return err
		}
	}
	return c.report(ctx, out, orgID, rec, entries)
}

// Set grants a plan, merging change over whatever is stored.
func (c *CLI) Set(ctx context.Context, out io.Writer, orgID, actor string, change corebilling.Change) error {
	rec, err := c.svc.SetPlan(ctx, orgID, actor, change)
	if err != nil {
		return err
	}
	return c.report(ctx, out, orgID, rec, nil)
}

// ExtendTrial moves the org's trial end to days from now.
func (c *CLI) ExtendTrial(ctx context.Context, out io.Writer, orgID, actor string, days int) error {
	rec, err := c.svc.ExtendTrial(ctx, orgID, actor, days, time.Now())
	if err != nil {
		return err
	}
	return c.report(ctx, out, orgID, rec, nil)
}

// Clear deletes the row, returning the org to the derived trial-then-free floors.
func (c *CLI) Clear(ctx context.Context, out io.Writer, orgID, actor string) error {
	if err := c.svc.Clear(ctx, orgID, actor); err != nil {
		return err
	}
	// The empty record rather than a re-read: the delete is what just committed,
	// so this is the authoritative answer even where the reader is a replica.
	return c.report(ctx, out, orgID, corebilling.Record{}, nil)
}

// report renders one org's state. rec is passed in rather than re-read so a
// mutation reports the row its own transaction wrote.
func (c *CLI) report(ctx context.Context, out io.Writer, orgID string, rec corebilling.Record, history []corebilling.HistoryEntry) error {
	// The display name is what tells an operator they have the right org; the
	// service resolves entitlement and knows nothing about it.
	read := dbread.New(c.pgRO)
	org, err := read.GetOrgByID(ctx, orgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return corebilling.ErrOrgNotFound
		}
		return err
	}
	ent, err := c.svc.GetEntitlement(ctx, orgID, time.Now())
	if err != nil {
		return err
	}
	// Not through the entitlement: that one nils a non-live row and resolves
	// nothing while billing is off, which is when `show` is most worth running.
	subs, err := read.ListBillingSubscriptionsByOrg(ctx, orgID)
	if err != nil {
		return err
	}
	return writeReport(out, org, ent, rec, subs, history)
}
