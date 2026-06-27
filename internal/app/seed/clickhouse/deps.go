package seed

import (
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jackc/pgx/v5/pgxpool"
)

type deps struct {
	pg *pgxpool.Pool
	ch driver.Conn
}
