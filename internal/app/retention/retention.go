// Package retention is the operator CLI behind `pug retention`. Postgres only.
package retention

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	corebilling "github.com/pug-sh/pug/internal/core/billing"
	"github.com/pug-sh/pug/internal/core/billing/entitlement"
	coreretention "github.com/pug-sh/pug/internal/core/retention"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/sethvargo/go-envconfig"
)

type CLI struct {
	svc *coreretention.Service
	pgW *pgxpool.Pool
}

// New builds its own pool: this binary runs one command and exits. It reads the
// billing switch, as the job does, to tell whether the org still pays.
func New(ctx context.Context) (*CLI, error) {
	var billingCfg corebilling.Config
	if err := envconfig.Process(ctx, &billingCfg); err != nil {
		return nil, fmt.Errorf("billing config: %w", err)
	}
	var pgCfg postgres.Config
	if err := envconfig.Process(ctx, &pgCfg); err != nil {
		return nil, fmt.Errorf("postgres config: %w", err)
	}
	pgW, err := postgres.NewWriterPool(ctx, &pgCfg)
	if err != nil {
		return nil, fmt.Errorf("postgres writer pool: %w", err)
	}
	ents, err := entitlement.NewService(pgW, pgW, billingCfg)
	if err != nil {
		pgW.Close()
		return nil, fmt.Errorf("entitlement service: %w", err)
	}
	return &CLI{svc: coreretention.NewService(pgW, nil, ents, coreretention.Config{}), pgW: pgW}, nil
}

func (c *CLI) Close() { c.pgW.Close() }

// Expire starts the wait before an org that stopped paying drops to free's length.
func (c *CLI) Expire(ctx context.Context, out io.Writer, orgID, actor string) error {
	got, err := c.svc.Expire(ctx, orgID, actor, time.Now().UTC())
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "%s: keeps %s until %s, then %s, unless it pays again first\n",
		orgID, length(got.Days), got.AppliesAt.Format(time.DateOnly), length(got.PendingDays))
	return err
}

func length(days int64) string {
	if days == 0 {
		return "everything"
	}
	return fmt.Sprintf("%d days", days)
}
