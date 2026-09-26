// Package domains is the operator CLI behind `pug domains`. It uses only Postgres and
// leaves settings to org admins, except `unenforce`: it turns Require SSO off, the way
// back when SSO breaks and nobody on the domain can sign in.
package domains

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	coreorgs "github.com/pug-sh/pug/internal/core/orgs"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/sethvargo/go-envconfig"
)

type CLI struct {
	svc  *coreorgs.Service
	pgRO *pgxpool.Pool
	pgW  *pgxpool.Pool
}

// New builds its own pools: this binary runs one command and exits.
func New(ctx context.Context) (*CLI, error) {
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
	return &CLI{svc: coreorgs.NewService(pgRO, pgW, nil), pgRO: pgRO, pgW: pgW}, nil
}

func (c *CLI) Close() {
	c.pgRO.Close()
	c.pgW.Close()
}

// Verify marks the domain verified for the org, then shows every claim on it.
func (c *CLI) Verify(ctx context.Context, out io.Writer, orgID, domain string) error {
	d, err := c.svc.VerifyDomainByOperator(ctx, orgID, domain)
	if err != nil {
		return err
	}
	return c.Show(ctx, out, d.Domain)
}

func (c *CLI) Show(ctx context.Context, out io.Writer, domain string) error {
	claims, err := c.svc.DomainClaims(ctx, domain)
	if err != nil {
		return err
	}
	return writeClaims(out, domain, claims)
}

// Release drops the org's claim, then shows what is left on the domain.
func (c *CLI) Release(ctx context.Context, out io.Writer, orgID, domain string) error {
	if err := c.svc.ReleaseDomain(ctx, orgID, domain); err != nil {
		return err
	}
	return c.Show(ctx, out, domain)
}

// Unenforce turns Require SSO off in every org, then shows the claims.
func (c *CLI) Unenforce(ctx context.Context, out io.Writer, domain string) error {
	n, err := c.svc.UnenforceDomain(ctx, domain)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Require SSO turned off in %d org(s).\n", n)
	return c.Show(ctx, out, domain)
}

func writeClaims(out io.Writer, domain string, claims []dbread.ListOrgDomainsByDomainRow) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if len(claims) == 0 {
		fmt.Fprintf(w, "%s\tno org has added this domain\n", domain)
		return w.Flush()
	}
	fmt.Fprintln(w, "ORG\tNAME\tSTATUS\tAUTO-JOIN\tORG CREATION\tSSO SEEN\tREQUIRE SSO")
	for _, c := range claims {
		status := "pending"
		if c.VerifiedAt.Valid {
			status = fmt.Sprintf("verified by %s %s", c.VerificationMethod.String, day(c.VerifiedAt.Time))
		}
		autoJoin := "off"
		if c.AutoJoinRole.Valid {
			autoJoin = c.AutoJoinRole.String
		}
		creation := "allowed"
		if !c.MembersCanCreateOrgs {
			creation = "restricted"
		}
		seen := "never"
		if c.SsoSeenAt.Valid {
			seen = day(c.SsoSeenAt.Time)
		}
		requireSSO := "off"
		if c.RequireSso {
			requireSSO = "on"
		}
		fmt.Fprintf(w, "%s\t%q\t%s\t%s\t%s\t%s\t%s\n", c.OrgID, c.OrgDisplayName, status, autoJoin, creation, seen, requireSSO)
	}
	return w.Flush()
}

func day(t time.Time) string { return t.UTC().Format(time.DateOnly) }
