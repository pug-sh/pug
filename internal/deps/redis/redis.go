package redis

import (
	"context"
	"log/slog"

	"github.com/redis/go-redis/v9"
)

type Client struct {
	Redis *redis.Client
}

func NewFromConfig(ctx context.Context, cfg *Config) (*Client, error) {
	opts, err := redis.ParseURL(cfg.URL)
	if err != nil {
		slog.ErrorContext(ctx, "unable to parse redis URL", slog.Any("error", err))
		return nil, err
	}

	client := redis.NewClient(opts)

	if err := client.Ping(ctx).Err(); err != nil {
		slog.ErrorContext(ctx, "unable to connect to redis", slog.Any("error", err))
		return nil, err
	}

	return &Client{Redis: client}, nil
}

func (c *Client) Close(ctx context.Context) {
	slog.InfoContext(ctx, "Closing redis connection.")
	if err := c.Redis.Close(); err != nil {
		slog.ErrorContext(ctx, "error closing redis connection", slog.Any("error", err))
	}
}
