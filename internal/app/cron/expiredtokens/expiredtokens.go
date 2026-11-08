// Package expiredtokens removes expired authentication tokens and dead MCP grants
// in one pass. Used and revoked tokens are retained until expires_at so
// refresh-token replay detection and invite resend accounting keep their full
// retention windows; a dead grant's tokens go with it.
package expiredtokens

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/pug-sh/pug/internal/app/cron"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/slogx"
	"github.com/sethvargo/go-envconfig"
	"go.opentelemetry.io/otel"
)

const passTimeout = 5 * time.Minute

// Run deletes expired tokens and dead MCP grants once and returns. Lock contention is a successful
// no-op; any setup or deletion failure is returned to the CronJob.
func Run(ctx context.Context) error {
	closeOtel, err := telemetry.SetupSDK(ctx)
	if err != nil {
		return err
	}
	defer telemetry.ShutdownOnExit(ctx, closeOtel)

	ctx, span := otel.Tracer("cron/expired-tokens").Start(ctx, "expired_tokens.pass")
	defer span.End()

	ctx, cancel := context.WithTimeout(ctx, passTimeout)
	defer cancel()

	var pgCfg postgres.Config
	if err := envconfig.Process(ctx, &pgCfg); err != nil {
		return setupFailed(ctx, "postgres config", err)
	}
	pgW, err := postgres.NewWriterPool(ctx, &pgCfg)
	if err != nil {
		return setupFailed(ctx, "postgres writer pool", err)
	}
	defer pgW.Close()

	err = cron.WithLock(ctx, pgW, cron.JobExpiredTokens, func(ctx context.Context) error {
		w := dbwrite.New(pgW)
		n, err := w.DeleteExpiredRefreshTokens(ctx)
		if err != nil {
			return fmt.Errorf("delete expired refresh tokens: %w", err)
		}
		slog.InfoContext(ctx, "deleted expired refresh tokens", slog.Int64("refresh_tokens_deleted", n))
		n, err = w.DeleteDeadMCPGrants(ctx)
		if err != nil {
			return fmt.Errorf("delete dead mcp grants: %w", err)
		}
		slog.InfoContext(ctx, "deleted dead mcp grants", slog.Int64("mcp_grants_deleted", n))
		n, err = w.DeleteExpiredEmailActionTokens(ctx)
		if err != nil {
			return fmt.Errorf("delete expired email action tokens: %w", err)
		}
		slog.InfoContext(ctx, "deleted expired email action tokens", slog.Int64("email_action_tokens_deleted", n))
		return nil
	})
	if errors.Is(err, cron.ErrLockHeld) {
		slog.InfoContext(ctx, "another pass holds the expired-token cleanup lock; nothing to do")
		return nil
	}
	if err != nil {
		slog.ErrorContext(ctx, "expired-token cleanup failed", slogx.Error(err))
		telemetry.RecordErrorOnSpan(span, err)
		return err
	}
	return nil
}

func setupFailed(ctx context.Context, step string, err error) error {
	err = fmt.Errorf("expired-token cleanup setup: %s: %w", step, err)
	slog.ErrorContext(ctx, "expired-token cleanup setup failed", slogx.Error(err), slog.String("step", step))
	telemetry.RecordError(ctx, err)
	return err
}
