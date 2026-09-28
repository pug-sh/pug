package deletion

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/slogx"
)

var ErrProjectInactive = errors.New("project is pending deletion")
var ErrOrganizationInactive = errors.New("organization is pending deletion")
var ErrInsufficientPool = errors.New("deletion gate callbacks require at least two PostgreSQL connections")

// Gate holds a PostgreSQL session advisory lock for the lifetime of a write. A
// deletion request takes the matching exclusive transaction lock before marking
// the target inactive, so a delayed writer that passed its state check cannot
// write after the transition. Session locks keep the fence without leaving a
// database transaction open while an RPC or ClickHouse write runs.
type Gate struct {
	pg            *pgxpool.Pool
	callbackSlots chan struct{}
}

func NewGate(pg *pgxpool.Pool) *Gate {
	capacity := max(0, int(pg.Config().MaxConns)-1)
	return &Gate{pg: pg, callbackSlots: make(chan struct{}, capacity)}
}

func (g *Gate) WithActiveProject(ctx context.Context, projectID string, write func(context.Context) error) error {
	if err := g.acquireCallbackSlot(ctx); err != nil {
		return err
	}
	defer g.releaseCallbackSlot()
	return g.WithActiveProjectExternal(ctx, projectID, write)
}

// WithActiveProjectExternal fences a write that does not acquire another
// connection from this Gate's PostgreSQL pool, such as a ClickHouse write.
func (g *Gate) WithActiveProjectExternal(ctx context.Context, projectID string, write func(context.Context) error) error {
	return g.WithActiveProjectConnection(ctx, projectID, func(ctx context.Context, _ *pgxpool.Conn) error {
		return write(ctx)
	})
}

// WithActiveProjectConnection gives PostgreSQL writers the same connection that
// owns the session lock. This avoids nested pool acquisition while the fence is
// held, which can deadlock a saturated worker pool.
func (g *Gate) WithActiveProjectConnection(ctx context.Context, projectID string, write func(context.Context, *pgxpool.Conn) error) error {
	conn, err := g.pg.Acquire(ctx)
	if err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, `select pg_advisory_lock_shared(hashtext('pug-project-deletion'), hashtext($1::text))`, projectID); err != nil {
		conn.Release()
		return err
	}
	defer releaseSharedLock(ctx, conn, "pug-project-deletion", projectID)
	var active bool
	err = conn.QueryRow(ctx, `select p.deletion_state='active' and o.deletion_state='active'
		from projects p join orgs o on o.id=p.org_id where p.id=$1`, projectID).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrProjectInactive
	}
	if err != nil {
		return err
	}
	if !active {
		return ErrProjectInactive
	}
	return write(ctx, conn)
}

func lockProjectExclusive(ctx context.Context, tx pgx.Tx, projectID string) error {
	_, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtext('pug-project-deletion'), hashtext($1::text))`, projectID)
	return err
}

// WithActiveOrganization fences org-scoped requests, including project
// creation and member changes, against the organization deletion transition.
func (g *Gate) WithActiveOrganization(ctx context.Context, orgID string, action func(context.Context) error) error {
	if err := g.acquireCallbackSlot(ctx); err != nil {
		return err
	}
	defer g.releaseCallbackSlot()
	conn, err := g.pg.Acquire(ctx)
	if err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, `select pg_advisory_lock_shared(hashtext('pug-org-deletion'),hashtext($1::text))`, orgID); err != nil {
		conn.Release()
		return err
	}
	defer releaseSharedLock(ctx, conn, "pug-org-deletion", orgID)
	var active bool
	if err := conn.QueryRow(ctx, `select deletion_state='active' from orgs where id=$1`, orgID).Scan(&active); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrOrganizationInactive
		}
		return err
	}
	if !active {
		return ErrOrganizationInactive
	}
	return action(ctx)
}

func (g *Gate) acquireCallbackSlot(ctx context.Context) error {
	if cap(g.callbackSlots) == 0 {
		return ErrInsufficientPool
	}
	select {
	case g.callbackSlots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *Gate) releaseCallbackSlot() { <-g.callbackSlots }

// ConcurrencyLimit caps worker concurrency at the number of connections that
// can own simultaneous gates. A nil gate is the test and legacy path, where no
// PostgreSQL connection is held.
func (g *Gate) ConcurrencyLimit(configured int) int {
	if g == nil || configured < 1 {
		return configured
	}
	maxConns := int(g.pg.Config().MaxConns)
	if maxConns < 1 {
		return 1
	}
	return min(configured, maxConns)
}

func releaseSharedLock(requestCtx context.Context, conn *pgxpool.Conn, namespace, id string) {
	unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(requestCtx), 5*time.Second)
	defer cancel()
	if _, err := conn.Exec(unlockCtx, `select pg_advisory_unlock_shared(hashtext($1::text),hashtext($2::text))`, namespace, id); err != nil {
		slog.ErrorContext(unlockCtx, "failed to release deletion gate; closing connection", slogx.Error(err))
		telemetry.RecordError(unlockCtx, err)
		pgConn := conn.Hijack()
		if closeErr := pgConn.Close(unlockCtx); closeErr != nil {
			slog.ErrorContext(unlockCtx, "failed to close connection after deletion gate unlock failure", slogx.Error(closeErr))
			telemetry.RecordError(unlockCtx, closeErr)
		}
		return
	}
	conn.Release()
}
