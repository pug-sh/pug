// Package retention deletes each org's event history past its retention length,
// a whole month at a time (docs/architecture/data-retention.md). It is the only
// writer of retention_state.
package retention

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pug-sh/pug/internal/core/billing/entitlement"
	chdb "github.com/pug-sh/pug/internal/deps/clickhouse"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/slogx"
)

var (
	ErrNoClickHouse   = errors.New("retention: no ClickHouse connection to delete through")
	ErrActorRequired  = errors.New("retention: an actor is required")
	ErrOrgPays        = errors.New("retention: the org has a live subscription, so its length stays")
	ErrNothingWaiting = errors.New("retention: nothing waits for an expire on this org; one that just stopped paying waits after the next daily pass")
	ErrDeleteFailing  = errors.New("retention: a delete is failing")
)

// Config is the switch on the deletes. Off, a pass still moves every org's state
// on, and logs what it would delete.
type Config struct {
	Enabled bool `env:"PUG_RETENTION_ENABLED,default=false"`
}

type Service struct {
	cfg   Config
	ch    *chdb.Conn
	ents  *entitlement.Service
	read  *dbread.Queries
	write *dbwrite.Queries
}

// NewService reads through the writer pool, so a pass moves on the latest state.
// ch is nil for `pug retention expire`; a pass needs it even switched off.
func NewService(pgW *pgxpool.Pool, ch *chdb.Conn, ents *entitlement.Service, cfg Config) *Service {
	return &Service{cfg: cfg, ch: ch, ents: ents, read: dbread.New(pgW), write: dbwrite.New(pgW)}
}

func failed(ctx context.Context, op string, err error) error {
	err = fmt.Errorf("retention: %s: %w", op, err)
	slog.ErrorContext(ctx, "retention step failed", slogx.Error(err))
	telemetry.RecordError(ctx, err)
	return err
}

// daysAttr logs zero as none: a 0 in a log line reads as keeping nothing.
func daysAttr(key string, days int64) slog.Attr {
	if days == 0 {
		return slog.String(key, "none")
	}
	return slog.Int64(key, days)
}
