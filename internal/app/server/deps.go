package server

import (
	"context"
	"strings"

	"github.com/fivebitsio/cotton/internal/deps/nats"
	"github.com/fivebitsio/cotton/internal/deps/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sethvargo/go-envconfig"
)

type deps struct {
	campaignsProducer  jetstream.JetStream
	corsOrigins        []string
	deliveriesProducer jetstream.JetStream
	eventsProducer     jetstream.JetStream
	jwtKey             []byte
	nats               *nats.NATSClient
	pgRo               *pgxpool.Pool
	pgW                *pgxpool.Pool
	port               string
}

func (d *deps) close(ctx context.Context) {
	d.pgRo.Close()
	d.pgW.Close()
	if d.nats != nil {
		d.nats.Close()
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

	return &deps{
		campaignsProducer:  natsClient.GetJetStream(),
		corsOrigins:        strings.Split(serverCfg.CORSOrigins, ","),
		deliveriesProducer: natsClient.GetJetStream(),
		eventsProducer:     natsClient.GetJetStream(),
		jwtKey:             []byte(serverCfg.JWTKey),
		nats:               natsClient,
		pgRo:               pgRo,
		pgW:                pgW,
		port:               serverCfg.Port,
	}, nil
}
