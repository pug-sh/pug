package projects

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/slogx"
	"github.com/rs/xid"
)

var ErrApiKeyNotFound = errors.New("api key not found")

// ApiKeyWithSecret pairs a stored key row with the raw key it was minted from.
// The raw key is the caller's only chance to see a private key: nothing but its
// digest is stored. Mirrors orgs.InviteDispatch's RawToken.
type ApiKeyWithSecret struct {
	Key    dbwrite.ApiKey
	RawKey string
}

// CreateApiKeyInTx mints a key of the given kind for a project and stores it.
// Exposed like CreateProjectInTx so a caller with an open transaction (the
// seeder pairing a project with its private key) can run it under their own tx;
// the handle may be tx-bound or pool-bound.
func CreateApiKeyInTx(ctx context.Context, w *dbwrite.Queries, projectID string, kind Kind, displayName string) (ApiKeyWithSecret, error) {
	raw, token, err := mintKey(kind)
	if err != nil {
		slog.ErrorContext(ctx, "failed to generate api key", slogx.Error(err),
			slog.String("project_id", projectID), slog.String("kind", string(kind)))
		telemetry.RecordError(ctx, err)
		return ApiKeyWithSecret{}, err
	}

	key, err := w.CreateApiKey(ctx, dbwrite.CreateApiKeyParams{
		DisplayName: displayName,
		ID:          xid.New().String(),
		Kind:        string(kind),
		Masked:      maskKeyDisplay(raw),
		ProjectID:   projectID,
		Token:       token,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to create api key", slogx.Error(err),
			slog.String("project_id", projectID), slog.String("kind", string(kind)))
		telemetry.RecordError(ctx, err)
		return ApiKeyWithSecret{}, err
	}

	return ApiKeyWithSecret{Key: key, RawKey: raw}, nil
}

func (s *Service) CreateApiKey(ctx context.Context, projectID string, kind Kind, displayName string) (ApiKeyWithSecret, error) {
	return CreateApiKeyInTx(ctx, s.write, projectID, kind, displayName)
}

func (s *Service) ListApiKeys(ctx context.Context, projectID string) ([]dbread.ApiKey, error) {
	return s.read.GetApiKeysByProjectID(ctx, projectID)
}

// DeleteApiKey removes a key from a project and drops the cached project row
// keyed by it, taking effect immediately. The api_keys row is the only place the
// key exists, so this is the whole revocation. A key id from another project is
// not found (the delete is project-scoped in SQL).
func (s *Service) DeleteApiKey(ctx context.Context, projectID, id string) error {
	key, err := s.write.DeleteApiKey(ctx, dbwrite.DeleteApiKeyParams{ID: id, ProjectID: projectID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrApiKeyNotFound
		}
		slog.ErrorContext(ctx, "failed to delete api key", slogx.Error(err),
			slog.String("project_id", projectID), slog.String("api_key_id", id))
		telemetry.RecordError(ctx, err)
		return err
	}

	s.invalidateTokens(ctx, projectID, key.Token)
	return nil
}

// apiKeyTokens lists the tokens of a project's keys — every value its cached row
// is reachable by.
//
// The error is returned rather than folded into a nil slice: an empty list is a
// legitimate state (a project whose every key has been revoked), so no caller can
// tell a failed listing from "no keys" by the slice alone. Each decides what a
// failure costs — DeleteProject cannot proceed without these, invalidateProject
// can. Logged + recorded here, at the layer that detects it.
func (s *Service) apiKeyTokens(ctx context.Context, projectID string) ([]string, error) {
	tokens, err := s.read.GetApiKeyTokensByProjectID(ctx, projectID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to list api key tokens for cache invalidation", slogx.Error(err),
			slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
		return nil, err
	}
	return tokens, nil
}

// invalidateCacheTimeout bounds the detached invalidation below: a Redis
// round-trip per token, plus the token listing on the invalidateProject path.
// Generous for that — the budget exists so a wedged Redis cannot pin the
// goroutine forever, and the alternative to finishing is a revoked key that keeps
// authenticating.
const invalidateCacheTimeout = 5 * time.Second

// detachedInvalidateCtx returns the context post-commit cache invalidation runs
// on. The write it follows has already committed, so the invalidation must not
// inherit the caller's cancellation: an admin who revokes a key and closes the
// tab would otherwise leave it cached until apiKeyCacheTTL while the RPC reported
// success — and their retry would report "not found", which reads as "already
// revoked". Mirrors the WithoutCancel behind deps/nats.worker's DLQ publish, for
// the same reason.
func detachedInvalidateCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), invalidateCacheTimeout)
}

func (s *Service) invalidateTokens(ctx context.Context, projectID string, tokens ...string) {
	if s.repo == nil {
		slog.WarnContext(ctx, "cache repo not set; skipping api key cache invalidation")
		return
	}
	ctx, cancel := detachedInvalidateCtx(ctx)
	defer cancel()
	s.repo.InvalidateProjectKeys(ctx, projectID, tokens...)
}
