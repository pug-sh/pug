package server

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"connectrpc.com/otelconnect"
	chdb "github.com/fivebitsio/cotton/internal/deps/clickhouse"
	"github.com/fivebitsio/cotton/internal/deps/nats"
	"github.com/fivebitsio/cotton/internal/deps/postgres"
	"github.com/fivebitsio/cotton/internal/deps/redis"
	"github.com/fivebitsio/cotton/internal/deps/telemetry"
	"github.com/fivebitsio/cotton/internal/slogx"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sethvargo/go-envconfig"
)

type deps struct {
	ch              *chdb.Conn
	closeOtel       func(context.Context) error
	corsOrigins     []string
	jwtKey          []byte
	nats            *nats.NATSClient
	otelInterceptor *otelconnect.Interceptor
	pgRo            *pgxpool.Pool
	pgW             *pgxpool.Pool
	redis           *redis.Client
	port            string
}

// close shuts down all deps. OTel must shut down last — it owns the slog backend,
// so earlier components' shutdown logs are still captured. A fresh timeout context
// is used internally so cleanup isn't aborted by a cancelled signal context.
func (d *deps) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	d.pgRo.Close()
	d.pgW.Close()
	if d.nats != nil {
		d.nats.Close()
	}
	if d.redis != nil {
		d.redis.Close(ctx)
	}
	if d.ch != nil {
		if err := d.ch.Close(); err != nil {
			slog.ErrorContext(ctx, "failed to close clickhouse", slogx.Error(err))
		}
	}
	if d.closeOtel != nil {
		if err := d.closeOtel(ctx); err != nil {
			slog.ErrorContext(ctx, "failed to shutdown telemetry", slogx.Error(err))
		}
	}
}

func newDeps(ctx context.Context) (*deps, error) {
	var closers []func()
	success := false
	defer func() {
		if !success {
			for i := len(closers) - 1; i >= 0; i-- {
				closers[i]()
			}
		}
	}()

	otelInterceptor, closeOtel, err := telemetry.NewOtelInterceptor(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed to initialize telemetry", slogx.Error(err))
		return nil, err
	}
	closers = append(closers, func() {
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := closeOtel(rollbackCtx); err != nil {
			slog.ErrorContext(rollbackCtx, "failed to close otel during rollback", slogx.Error(err))
		}
	})

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
	closers = append(closers, pgRo.Close)

	pgW, err := postgres.NewWriterPool(ctx, &pgCfg)
	if err != nil {
		return nil, err
	}
	closers = append(closers, pgW.Close)

	natsClient, err := nats.New(ctx)
	if err != nil {
		return nil, err
	}
	closers = append(closers, natsClient.Close)

	var redisCfg redis.Config
	if err := envconfig.Process(ctx, &redisCfg); err != nil {
		return nil, err
	}

	redisClient, err := redis.NewFromConfig(ctx, &redisCfg)
	if err != nil {
		return nil, err
	}
	closers = append(closers, func() {
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		redisClient.Close(rollbackCtx)
	})

	var chCfg chdb.Config
	if err := envconfig.Process(ctx, &chCfg); err != nil {
		return nil, err
	}

	chConn, err := chdb.NewReaderPool(ctx, &chCfg)
	if err != nil {
		return nil, err
	}
	closers = append(closers, func() {
		if err := chConn.Close(); err != nil {
			slog.ErrorContext(ctx, "failed to close clickhouse during rollback", slogx.Error(err))
		}
	})

	success = true
	return &deps{
		ch:              chConn,
		closeOtel:       closeOtel,
		corsOrigins:     strings.Split(serverCfg.CORSOrigins, ","),
		jwtKey:          []byte(serverCfg.JWTKey),
		nats:            natsClient,
		otelInterceptor: otelInterceptor,
		pgRo:            pgRo,
		pgW:             pgW,
		redis:           redisClient,
		port:            serverCfg.Port,
	}, nil
}
