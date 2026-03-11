package seed

import (
	"context"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	clickhousedeps "github.com/fivebitsio/cotton/internal/deps/clickhouse"
	"github.com/fivebitsio/cotton/internal/deps/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sethvargo/go-envconfig"
)

type deps struct {
	pg *pgxpool.Pool
	ch driver.Conn
}

func (d *deps) close() {
	d.pg.Close()
	_ = d.ch.Close()
}

func newDeps(ctx context.Context) (*deps, error) {
	var pgCfg postgres.Config
	if err := envconfig.Process(ctx, &pgCfg); err != nil {
		return nil, err
	}

	pg, err := postgres.NewReaderPool(ctx, &pgCfg)
	if err != nil {
		return nil, err
	}

	var chCfg clickhousedeps.Config
	if err := envconfig.Process(ctx, &chCfg); err != nil {
		pg.Close()
		return nil, err
	}

	chDB, err := clickhousedeps.NewFromConfig(ctx, &chCfg)
	if err != nil {
		pg.Close()
		return nil, err
	}

	return &deps{pg: pg, ch: chDB.Conn}, nil
}
