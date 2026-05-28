package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	ch "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"
	redislog "github.com/redis/go-redis/v9/logging"
)

const infraProbeTimeout = 3 * time.Second

type infraHealth struct {
	label string
	url   string
	err   error
}

func probeInfrastructure(ctx context.Context) []infraHealth {
	targets := []struct {
		label  string
		envKey string
		probe  func(context.Context, string) error
	}{
		{"PostgreSQL", "DATABASE_URL", probePostgres},
		{"ClickHouse", "CLICKHOUSE_URL", probeClickHouse},
		{"NATS", "NATS_URL", probeNATS},
		{"Redis", "REDIS_URL", probeRedis},
	}

	probeCtx, cancel := context.WithTimeout(ctx, infraProbeTimeout)
	defer cancel()

	results := make([]infraHealth, len(targets))
	var wg sync.WaitGroup

	for i, target := range targets {
		url := os.Getenv(target.envKey)
		if url == "" {
			results[i].label = target.label
			continue
		}

		wg.Add(1)
		go func(i int, label, url string, probe func(context.Context, string) error) {
			defer wg.Done()
			results[i] = infraHealth{
				label: label,
				url:   url,
				err:   probe(probeCtx, url),
			}
		}(i, target.label, url, target.probe)
	}

	wg.Wait()

	ordered := make([]infraHealth, 0, len(targets))
	for _, r := range results {
		if r.url == "" {
			continue
		}
		ordered = append(ordered, r)
	}
	return ordered
}

func printInfrastructure(ctx context.Context) {
	fmt.Println(bold + "Infrastructure:" + reset)
	for _, h := range probeInfrastructure(ctx) {
		fmt.Println("  "+green+h.label+":"+reset, redactURL(h.url), formatInfraHealth(h.err))
	}
	fmt.Println()
}

func formatInfraHealth(err error) string {
	if err == nil {
		return green + "connected" + reset
	}
	return red + "unreachable (" + shortProbeError(err) + ")" + reset
}

func shortProbeError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	msg := err.Error()
	if idx := strings.IndexByte(msg, '\n'); idx >= 0 {
		msg = msg[:idx]
	}
	msg = strings.TrimRight(msg, ": ")
	if len(msg) > 80 {
		return msg[:77] + "..."
	}
	return msg
}

func probePostgres(ctx context.Context, dbURL string) error {
	cfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	return pool.Ping(ctx)
}

func probeClickHouse(ctx context.Context, chURL string) error {
	opts, err := ch.ParseDSN(chURL)
	if err != nil {
		return err
	}
	conn, err := ch.Open(opts)
	if err != nil {
		return err
	}
	defer func() {
		_ = conn.Close()
	}()
	return conn.Ping(ctx)
}

func probeNATS(_ context.Context, natsURL string) error {
	opts := []nats.Option{
		nats.Name("pug-dev-probe"),
		nats.Timeout(infraProbeTimeout),
	}
	if jwt := os.Getenv("NATS_JWT"); jwt != "" {
		if seed := os.Getenv("NATS_SEED"); seed != "" {
			opts = append(opts, nats.UserJWTAndSeed(jwt, seed))
		}
	} else if creds := os.Getenv("NATS_CREDS_FILE"); creds != "" {
		opts = append(opts, nats.UserCredentials(creds))
	}

	conn, err := nats.Connect(natsURL, opts...)
	if err != nil {
		return err
	}
	conn.Close()
	return nil
}

func probeRedis(ctx context.Context, redisURL string) error {
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return err
	}
	opts.DialerRetries = 1
	opts.DialTimeout = time.Second

	redislog.Disable()
	defer redislog.Enable()

	client := redis.NewClient(opts)
	defer func() {
		_ = client.Close()
	}()
	return client.Ping(ctx).Err()
}
