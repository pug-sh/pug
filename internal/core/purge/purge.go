// Package purge erases what a deleted project stored.
package purge

import (
	"github.com/jackc/pgx/v5/pgxpool"
	coreusage "github.com/pug-sh/pug/internal/core/usage"
	chdb "github.com/pug-sh/pug/internal/deps/clickhouse"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
)

type Service struct {
	ch    *chdb.Conn
	pgW   *pgxpool.Pool
	read  *dbread.Queries
	write *dbwrite.Queries
	usage *coreusage.Service
}

// NewService reads through the writer pool too, so each step sees the last one's
// writes.
func NewService(pgW *pgxpool.Pool, ch *chdb.Conn) *Service {
	return &Service{
		ch:    ch,
		pgW:   pgW,
		read:  dbread.New(pgW),
		write: dbwrite.New(pgW),
		usage: coreusage.NewService(pgW, pgW).WithClickHouse(ch),
	}
}
