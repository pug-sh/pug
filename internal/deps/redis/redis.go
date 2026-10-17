package redis

import (
	"context"
	"log/slog"

	"github.com/pug-sh/pug/internal/slogx"
	"github.com/redis/go-redis/extra/redisotel/v9"
	"github.com/redis/go-redis/v9"
)

type Client struct {
	client *redis.Client
}

func (c *Client) Unwrap() *redis.Client {
	return c.client
}

func NewFromConfig(ctx context.Context, cfg *Config) (*Client, error) {
	opts, err := redis.ParseURL(cfg.URL)
	if err != nil {
		slog.ErrorContext(ctx, "unable to parse redis URL", slogx.Error(err)) // puglint:exempt (no span at startup)
		return nil, err
	}

	client := redis.NewClient(opts)

	// WithDBStatement(false): by default redisotel writes each command's
	// arguments into the span's db.statement, redacting only the Redis
	// credentials it recognizes (AUTH, CONFIG SET requirepass and a few more);
	// any other value is exported, at most hex-encoded. Callers pass secrets as
	// arguments (the cookieless salt, the cached project row with its FCM
	// service-account key), so the statement would ship them to the trace
	// backend. The span name still carries the command.
	if err := redisotel.InstrumentTracing(client, redisotel.WithDBStatement(false)); err != nil {
		slog.ErrorContext(ctx, "error instrumenting Redis tracing", slogx.Error(err)) // puglint:exempt (no span at startup)
		if closeErr := client.Close(); closeErr != nil {
			slog.ErrorContext(ctx, "error closing redis after instrumentation failure", slogx.Error(closeErr)) // puglint:exempt (no span at startup)
		}
		return nil, err
	}
	if err := redisotel.InstrumentMetrics(client); err != nil {
		slog.ErrorContext(ctx, "error instrumenting Redis metrics", slogx.Error(err)) // puglint:exempt (no span at startup)
		if closeErr := client.Close(); closeErr != nil {
			slog.ErrorContext(ctx, "error closing redis after instrumentation failure", slogx.Error(closeErr)) // puglint:exempt (no span at startup)
		}
		return nil, err
	}

	if err := client.Ping(ctx).Err(); err != nil {
		slog.ErrorContext(ctx, "unable to connect to Redis", slogx.Error(err)) // puglint:exempt (no span at startup)
		if closeErr := client.Close(); closeErr != nil {
			slog.ErrorContext(ctx, "error closing redis after ping failure", slogx.Error(closeErr)) // puglint:exempt (no span at startup)
		}
		return nil, err
	}

	return &Client{client: client}, nil
}

func (c *Client) Close(ctx context.Context) {
	slog.InfoContext(ctx, "Closing redis connection.")
	if err := c.client.Close(); err != nil {
		slog.ErrorContext(ctx, "error closing redis connection", slogx.Error(err)) // puglint:exempt (no span at shutdown)
	}
}
