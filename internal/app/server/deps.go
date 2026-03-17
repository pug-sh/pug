package server

import (
	"context"
	"strings"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	chdb "github.com/fivebitsio/cotton/internal/deps/clickhouse"
	"github.com/fivebitsio/cotton/internal/deps/nats"
	"github.com/fivebitsio/cotton/internal/deps/postgres"
	"github.com/fivebitsio/cotton/internal/deps/redis"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sethvargo/go-envconfig"
)

type deps struct {
	ch          driver.Conn
	corsOrigins []string
	jwtKey      []byte
	nats        *nats.NATSClient
	pgRo        *pgxpool.Pool
	pgW         *pgxpool.Pool
	redis       *redis.Client
	port        string
}

func (d *deps) close(ctx context.Context) {
	d.pgRo.Close()
	d.pgW.Close()
	if d.nats != nil {
		d.nats.Close()
	}
	if d.redis != nil {
		d.redis.Close(ctx)
	}
	if d.ch != nil {
		d.ch.Close()
	}
}

func newDeps(ctx context.Context) (*deps, error) {
	var serverCfg config
	if err := envconfig.Process(ctx, &serverCfg); err != nil {
		return nil, err
	}

	var pgCfg postgres.Config
	if err := envconfig.Process(ctx, &pgCfg); err != nil {
		return nil, err
	}

	pgRo, err := postgres.NewReaderPool(ctx, &pgCfg)
	if err != nil {
		return nil, err
	}

	pgW, err := postgres.NewWriterPool(ctx, &pgCfg)
	if err != nil {
		pgRo.Close()
		return nil, err
	}

	natsClient, err := nats.New(ctx)
	if err != nil {
		pgRo.Close()
		pgW.Close()
		return nil, err
	}

	var redisCfg redis.Config
	if err := envconfig.Process(ctx, &redisCfg); err != nil {
		pgRo.Close()
		pgW.Close()
		natsClient.Close()
		return nil, err
	}

	redisClient, err := redis.NewFromConfig(ctx, &redisCfg)
	if err != nil {
		pgRo.Close()
		pgW.Close()
		natsClient.Close()
		return nil, err
	}

	var chCfg chdb.Config
	if err := envconfig.Process(ctx, &chCfg); err != nil {
		pgRo.Close()
		pgW.Close()
		natsClient.Close()
		redisClient.Close(ctx)
		return nil, err
	}

	chConn, err := chdb.NewReaderPool(ctx, &chCfg)
	if err != nil {
		pgRo.Close()
		pgW.Close()
		natsClient.Close()
		redisClient.Close(ctx)
		return nil, err
	}

	return &deps{
		ch:          chConn,
		corsOrigins: strings.Split(serverCfg.CORSOrigins, ","),
		jwtKey:      []byte(serverCfg.JWTKey),
		nats:        natsClient,
		pgRo:        pgRo,
		pgW:         pgW,
		redis:       redisClient,
		port:        serverCfg.Port,
	}, nil
}
