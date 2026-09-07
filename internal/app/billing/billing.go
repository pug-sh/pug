// Package billing is the operator CLI behind `pug billing`: the only writer of
// billing_entitlements. Postgres only -- no provider and no network -- and ungated
// by PUG_BILLING_ENABLED, which decides only how the resolved half is reported.
package billing

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
	corebilling "github.com/pug-sh/pug/internal/core/billing"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/sethvargo/go-envconfig"
)

// deps is one command's worth of wiring: the billing service, and a plain org read
// for the display name that tells an operator they have the right org.
type deps struct {
	read *dbread.Queries
	svc  *corebilling.Service
}

// Show prints the resolved entitlement and the stored row beneath it: a lapsed
// deal's quota is invisible in the resolved answer and still carries onto a set.
func Show(ctx context.Context, out io.Writer, orgID string, history bool) error {
	return withDeps(ctx, func(ctx context.Context, d deps) error {
		rec, err := d.svc.StoredRecord(ctx, orgID)
		if err != nil {
			return err
		}
		var entries []corebilling.HistoryEntry
		if history {
			if entries, err = d.svc.History(ctx, orgID); err != nil {
				return err
			}
		}
		return d.report(ctx, out, orgID, rec, entries)
	})
}

// Set grants a plan, merging change over whatever is stored.
func Set(ctx context.Context, out io.Writer, orgID, actor string, change corebilling.Change) error {
	return withDeps(ctx, func(ctx context.Context, d deps) error {
		rec, err := d.svc.SetPlan(ctx, orgID, actor, change)
		if err != nil {
			return err
		}
		return d.report(ctx, out, orgID, rec, nil)
	})
}

// ExtendTrial moves the org's trial end to days from now.
func ExtendTrial(ctx context.Context, out io.Writer, orgID, actor string, days int) error {
	return withDeps(ctx, func(ctx context.Context, d deps) error {
		rec, err := d.svc.ExtendTrial(ctx, orgID, actor, days, time.Now())
		if err != nil {
			return err
		}
		return d.report(ctx, out, orgID, rec, nil)
	})
}

// Clear deletes the row, returning the org to the derived trial-then-free floors.
func Clear(ctx context.Context, out io.Writer, orgID, actor string) error {
	return withDeps(ctx, func(ctx context.Context, d deps) error {
		if err := d.svc.Clear(ctx, orgID, actor); err != nil {
			return err
		}
		// The empty record rather than a re-read: the delete is what just committed,
		// so this is the authoritative answer even where the reader is a replica.
		return d.report(ctx, out, orgID, corebilling.Record{}, nil)
	})
}

// report renders one org's state. rec is passed in rather than re-read so a
// mutation reports the row its own transaction wrote.
func (d deps) report(ctx context.Context, out io.Writer, orgID string, rec corebilling.Record, history []corebilling.HistoryEntry) error {
	org, err := d.read.GetOrgByID(ctx, orgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return corebilling.ErrOrgNotFound
		}
		return err
	}
	ent, err := d.svc.GetEntitlement(ctx, orgID, time.Now())
	if err != nil {
		return err
	}
	return writeReport(out, org, ent, rec, history)
}

func withDeps(ctx context.Context, fn func(context.Context, deps) error) error {
	var billingCfg corebilling.Config
	if err := envconfig.Process(ctx, &billingCfg); err != nil {
		return fmt.Errorf("billing config: %w", err)
	}

	var pgCfg postgres.Config
	if err := envconfig.Process(ctx, &pgCfg); err != nil {
		return fmt.Errorf("postgres config: %w", err)
	}
	pgRO, err := postgres.NewReaderPool(ctx, &pgCfg)
	if err != nil {
		return fmt.Errorf("postgres reader pool: %w", err)
	}
	defer pgRO.Close()

	pgW, err := postgres.NewWriterPool(ctx, &pgCfg)
	if err != nil {
		return fmt.Errorf("postgres writer pool: %w", err)
	}
	defer pgW.Close()

	// No payments: this CLI never talks to a provider, and a nil Payments is the
	// same supported shape a deployment without credentials runs in.
	svc, err := corebilling.NewService(pgRO, pgW, billingCfg.Enabled, nil)
	if err != nil {
		return fmt.Errorf("billing service: %w", err)
	}
	return fn(ctx, deps{read: dbread.New(pgRO), svc: svc})
}
