package profiles

import (
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Worker struct {
	PgW   *pgxpool.Pool
	Read  *dbread.Queries
	Write *dbwrite.Queries
	Ch    driver.Conn
}

func NewWorker(pgRO, pgW *pgxpool.Pool, ch driver.Conn) *Worker {
	return &Worker{
		PgW:   pgW,
		Read:  dbread.New(pgRO),
		Write: dbwrite.New(pgW),
		Ch:    ch,
	}
}
