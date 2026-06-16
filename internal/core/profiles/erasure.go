package profiles

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	natsdeps "github.com/pug-sh/pug/internal/deps/nats"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	workercompliancev1 "github.com/pug-sh/pug/internal/gen/proto/workers/compliance/v1"
	workerprofilesv1 "github.com/pug-sh/pug/internal/gen/proto/workers/profiles/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/slogx"
	"github.com/rs/xid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// GDPR/DPDP data-subject erasure (roadmap §4.1). The flow is split: a thin
// synchronous prelude (record the request + soft-delete to hide immediately +
// enqueue) and a heavy asynchronous worker (resolve the identity fan-out, hard
// delete events + derived rollups + the profile). See
// docs/compliance/4.1-erasure-scope.md.

var (
	ErrDeletionRequestNotFound = errors.New("profiles: deletion request not found")
	ErrErasureUnavailable      = errors.New("profiles: erasure dependencies are unavailable")
)

// Compliance request lifecycle. Mirrors the compliance_requests.status CHECK constraint.
const (
	ComplianceStatusPending    = "pending"
	ComplianceStatusProcessing = "processing"
	ComplianceStatusCompleted  = "completed"
	ComplianceStatusFailed     = "failed"
)

// Compliance request kinds. Mirrors the compliance_requests.kind CHECK constraint.
const (
	ComplianceKindErase  = "erase"
	ComplianceKindExport = "export"
)

// RequestErasureByID enqueues erasure of the data subject identified by profile
// id (the dashboard "delete this profile" path). The profile must exist; a
// missing or already-deleted profile returns ErrProfileNotFound.
func (s *Service) RequestErasureByID(ctx context.Context, projectID, profileID, requestedBy string) (string, string, error) {
	if s == nil || s.pgW == nil || s.write == nil || s.read == nil || s.producer == nil {
		return "", "", ErrErasureUnavailable
	}

	profile, err := s.read.GetProfileByIDAndProjectID(ctx, dbread.GetProfileByIDAndProjectIDParams{
		ID:        profileID,
		ProjectID: projectID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "", ErrProfileNotFound
		}
		slog.ErrorContext(ctx, "failed resolving profile for erasure", slogx.Error(err),
			slog.String("profile_id", profileID), slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
		return "", "", err
	}
	return s.requestErasure(ctx, projectID, profile.ExternalID.String, profile.ID, requestedBy)
}

// RequestErasureByExternalID enqueues erasure of the data subject identified by
// external_id (the controller-facing handle). It proceeds even when no profile
// row resolves — events can be keyed directly by external_id, and those must
// still be erased.
func (s *Service) RequestErasureByExternalID(ctx context.Context, projectID, externalID, requestedBy string) (string, string, error) {
	if s == nil || s.pgW == nil || s.write == nil || s.read == nil || s.producer == nil {
		return "", "", ErrErasureUnavailable
	}

	profileID := ""
	profile, err := s.read.GetProfileByProjectAndExternalID(ctx, dbread.GetProfileByProjectAndExternalIDParams{
		ProjectID:  projectID,
		ExternalID: externalID,
	})
	switch {
	case err == nil:
		profileID = profile.ID
	case errors.Is(err, pgx.ErrNoRows):
		// No profile — events keyed directly by external_id are still erased.
		slog.InfoContext(ctx, "erasure requested for external_id with no profile row",
			slog.String("external_id", externalID), slog.String("project_id", projectID))
	default:
		slog.ErrorContext(ctx, "failed resolving profile by external_id for erasure", slogx.Error(err),
			slog.String("external_id", externalID), slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
		return "", "", err
	}
	return s.requestErasure(ctx, projectID, externalID, profileID, requestedBy)
}

// requestErasure is the shared synchronous prelude: create the audit row, soft
// delete the profile (when one exists) so it disappears from reads immediately,
// then publish the erase message for the worker. profileID is "" when erasing by
// external_id with no resolved profile.
func (s *Service) requestErasure(ctx context.Context, projectID, externalID, profileID, requestedBy string) (string, string, error) {
	requestID := xid.New().String()

	tx, err := s.pgW.Begin(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed starting erasure request transaction", slogx.Error(err),
			slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
		return "", "", err
	}
	defer func() {
		if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			slog.ErrorContext(ctx, "failed rolling back erasure request transaction", slogx.Error(rbErr),
				slog.String("project_id", projectID), slog.String("request_id", requestID))
			telemetry.RecordError(ctx, rbErr)
		}
	}()

	qtx := s.write.WithTx(tx)

	if _, err := qtx.CreateComplianceRequest(ctx, dbwrite.CreateComplianceRequestParams{
		ID:          requestID,
		ProjectID:   projectID,
		Kind:        ComplianceKindErase,
		ProfileID:   postgres.NewOptionalText(profileID),
		ExternalID:  postgres.NewOptionalText(externalID),
		RequestedBy: postgres.NewOptionalText(requestedBy),
	}); err != nil {
		slog.ErrorContext(ctx, "failed creating deletion request", slogx.Error(err),
			slog.String("project_id", projectID), slog.String("request_id", requestID))
		telemetry.RecordError(ctx, err)
		return "", "", err
	}

	if profileID != "" {
		if _, err := qtx.SoftDeleteProfileByIDAndProjectID(ctx, dbwrite.SoftDeleteProfileByIDAndProjectIDParams{
			ID:        profileID,
			ProjectID: projectID,
		}); err != nil {
			slog.ErrorContext(ctx, "failed soft-deleting profile for erasure", slogx.Error(err),
				slog.String("profile_id", profileID), slog.String("project_id", projectID))
			telemetry.RecordError(ctx, err)
			return "", "", err
		}
		if _, err := qtx.DeactivateDevicesByProfileID(ctx, dbwrite.DeactivateDevicesByProfileIDParams{
			ProfileID: postgres.NewText(profileID),
			ProjectID: projectID,
		}); err != nil {
			slog.ErrorContext(ctx, "failed deactivating devices for erasure", slogx.Error(err),
				slog.String("profile_id", profileID), slog.String("project_id", projectID))
			telemetry.RecordError(ctx, err)
			return "", "", err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		slog.ErrorContext(ctx, "failed committing erasure request transaction", slogx.Error(err),
			slog.String("project_id", projectID), slog.String("request_id", requestID))
		telemetry.RecordError(ctx, err)
		return "", "", err
	}

	// Immediate ClickHouse read-hide for the profile (the worker hard-deletes it
	// later). Best-effort — the erase worker is the source of truth.
	if profileID != "" {
		s.publishProfileSoftDelete(ctx, projectID, profileID)
	}

	if err := s.publishErase(ctx, projectID, requestID, profileID, externalID); err != nil {
		// The request row is durably committed; only the enqueue failed. Surface
		// the error so the caller can retry — the record of intent is not lost.
		return requestID, ComplianceStatusPending, err
	}

	slog.InfoContext(ctx, "data subject erasure requested",
		slog.String("project_id", projectID), slog.String("request_id", requestID),
		slog.Bool("has_profile", profileID != ""))
	return requestID, ComplianceStatusPending, nil
}

// ExecuteErasure performs the irreversible hard erasure for a deletion request.
// It is idempotent and retry-safe: the resolved identifiers are frozen on the
// first pass, so a redelivery after events are deleted still cleans every store.
func (s *Service) ExecuteErasure(ctx context.Context, projectID, requestID string) error {
	if s == nil || s.ch == nil || s.pgW == nil || s.write == nil || s.read == nil {
		return ErrErasureUnavailable
	}

	req, err := s.read.GetComplianceRequestByID(ctx, dbread.GetComplianceRequestByIDParams{
		ID:        requestID,
		ProjectID: projectID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrDeletionRequestNotFound
		}
		slog.ErrorContext(ctx, "failed loading deletion request", slogx.Error(err),
			slog.String("request_id", requestID), slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
		return err
	}

	if req.Status == ComplianceStatusCompleted {
		slog.InfoContext(ctx, "deletion request already completed, skipping",
			slog.String("request_id", requestID), slog.String("project_id", projectID))
		return nil
	}

	distinctIDs := req.DistinctIds
	sessionIDs := req.SessionIds
	if len(distinctIDs) == 0 {
		distinctIDs, sessionIDs, err = s.freezeIdentifiers(ctx, &req)
		if err != nil {
			return err
		}
	}

	profileID := ""
	if req.ProfileID.Valid {
		profileID = req.ProfileID.String
	}

	// PostgreSQL hard deletes — devices BEFORE the profile (the profile_id FK is
	// ON DELETE SET NULL; deleting the profile first would orphan device rows
	// holding the push token + endpoint).
	if profileID != "" {
		if err := s.hardDeletePostgres(ctx, projectID, profileID); err != nil {
			return err
		}
	}

	if err := s.eraseClickHouse(ctx, projectID, profileID, distinctIDs, sessionIDs); err != nil {
		return err
	}

	if _, err := s.write.MarkComplianceRequestCompleted(ctx, dbwrite.MarkComplianceRequestCompletedParams{
		ID:        requestID,
		ProjectID: projectID,
	}); err != nil {
		slog.ErrorContext(ctx, "failed marking deletion request completed", slogx.Error(err),
			slog.String("request_id", requestID), slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
		return err
	}

	slog.InfoContext(ctx, "data subject erasure completed",
		slog.String("request_id", requestID), slog.String("project_id", projectID),
		slog.Int("distinct_ids", len(distinctIDs)), slog.Int("session_ids", len(sessionIDs)))
	return nil
}

// GetDeletionRequest returns the audit row for an erasure request so a
// controller can prove fulfilment.
func (s *Service) GetDeletionRequest(ctx context.Context, projectID, requestID string) (dbread.ComplianceRequest, error) {
	if s == nil || s.read == nil {
		return dbread.ComplianceRequest{}, ErrErasureUnavailable
	}
	req, err := s.read.GetComplianceRequestByID(ctx, dbread.GetComplianceRequestByIDParams{
		ID:        requestID,
		ProjectID: projectID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return dbread.ComplianceRequest{}, ErrDeletionRequestNotFound
		}
		slog.ErrorContext(ctx, "failed loading deletion request", slogx.Error(err),
			slog.String("request_id", requestID), slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
		return dbread.ComplianceRequest{}, err
	}
	return req, nil
}

// freezeIdentifiers resolves the full distinct_id fan-out and the session_ids
// (read from events BEFORE they are deleted) and persists them onto the request
// row so retries reuse the frozen set. Any resolution failure aborts without
// persisting, so a retry re-resolves cleanly.
func (s *Service) freezeIdentifiers(ctx context.Context, req *dbread.ComplianceRequest) ([]string, []string, error) {
	distinctIDs, err := s.resolveDistinctIDs(ctx, req)
	if err != nil {
		return nil, nil, err
	}

	var sessionIDs []string
	var eventsDeleted uint64
	if len(distinctIDs) > 0 {
		if sessionIDs, err = s.resolveSessionIDs(ctx, req.ProjectID, distinctIDs); err != nil {
			return nil, nil, err
		}
		if eventsDeleted, err = s.countEvents(ctx, req.ProjectID, distinctIDs); err != nil {
			return nil, nil, err
		}
	} else {
		slog.WarnContext(ctx, "erasure request has no resolvable identifiers",
			slog.String("request_id", req.ID), slog.String("project_id", req.ProjectID))
	}

	if _, err := s.write.FreezeComplianceRequestIdentifiers(ctx, dbwrite.FreezeComplianceRequestIdentifiersParams{
		ID:             req.ID,
		ProjectID:      req.ProjectID,
		DistinctIds:    distinctIDs,
		SessionIds:     sessionIDs,
		EventsAffected: int64(eventsDeleted),
	}); err != nil {
		slog.ErrorContext(ctx, "failed freezing erasure identifiers", slogx.Error(err),
			slog.String("request_id", req.ID), slog.String("project_id", req.ProjectID))
		telemetry.RecordError(ctx, err)
		return nil, nil, err
	}
	return distinctIDs, sessionIDs, nil
}

// resolveDistinctIDs builds the complete set of events.distinct_id values for the
// subject: the external_id (always, when present) and — when a profile exists —
// the profile id and every alias_id. For anonymous profiles the id IS the
// distinct_id; post-identify the external_id and the anon alias_ids are.
func (s *Service) resolveDistinctIDs(ctx context.Context, req *dbread.ComplianceRequest) ([]string, error) {
	seen := make(map[string]struct{})
	var ids []string
	add := func(v string) {
		if v == "" {
			return
		}
		if _, ok := seen[v]; ok {
			return
		}
		seen[v] = struct{}{}
		ids = append(ids, v)
	}

	if req.ExternalID.Valid {
		add(req.ExternalID.String)
	}
	if req.ProfileID.Valid && req.ProfileID.String != "" {
		add(req.ProfileID.String)
		aliasIDs, err := s.resolveAliasIDs(ctx, req.ProjectID, req.ProfileID.String)
		if err != nil {
			return nil, err
		}
		for _, a := range aliasIDs {
			add(a)
		}
	}
	return ids, nil
}

func (s *Service) resolveAliasIDs(ctx context.Context, projectID, profileID string) ([]string, error) {
	var aliasIDs []string
	if err := s.ch.Select(ctx, &aliasIDs,
		"SELECT DISTINCT alias_id FROM profile_aliases FINAL WHERE project_id = ? AND profile_id = ?",
		projectID, profileID,
	); err != nil {
		return nil, fmt.Errorf("resolve alias ids: %w", err)
	}
	return aliasIDs, nil
}

func (s *Service) resolveSessionIDs(ctx context.Context, projectID string, distinctIDs []string) ([]string, error) {
	inClause, inArgs := chInClause(distinctIDs)
	args := append([]any{projectID}, inArgs...)
	var sessionIDs []string
	if err := s.ch.Select(ctx, &sessionIDs,
		"SELECT DISTINCT toString(session_id) FROM events WHERE project_id = ? AND distinct_id IN "+inClause,
		args...,
	); err != nil {
		return nil, fmt.Errorf("resolve session ids: %w", err)
	}
	return sessionIDs, nil
}

func (s *Service) countEvents(ctx context.Context, projectID string, distinctIDs []string) (uint64, error) {
	inClause, inArgs := chInClause(distinctIDs)
	args := append([]any{projectID}, inArgs...)
	var count uint64
	if err := s.ch.QueryRow(ctx,
		"SELECT count() FROM events WHERE project_id = ? AND distinct_id IN "+inClause,
		args...,
	).Scan(&count); err != nil {
		return 0, fmt.Errorf("count events: %w", err)
	}
	return count, nil
}

// hardDeletePostgres removes the profile's PostgreSQL rows in one transaction,
// devices first (see DeleteDevicesByProfileID for why the order matters).
func (s *Service) hardDeletePostgres(ctx context.Context, projectID, profileID string) error {
	tx, err := s.pgW.Begin(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed starting erasure pg transaction", slogx.Error(err),
			slog.String("profile_id", profileID), slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
		return err
	}
	defer func() {
		if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			slog.ErrorContext(ctx, "failed rolling back erasure pg transaction", slogx.Error(rbErr),
				slog.String("profile_id", profileID), slog.String("project_id", projectID))
			telemetry.RecordError(ctx, rbErr)
		}
	}()

	qtx := s.write.WithTx(tx)
	if _, err := qtx.DeleteDevicesByProfileID(ctx, dbwrite.DeleteDevicesByProfileIDParams{
		ProfileID: postgres.NewText(profileID),
		ProjectID: projectID,
	}); err != nil {
		slog.ErrorContext(ctx, "failed hard-deleting profile devices", slogx.Error(err),
			slog.String("profile_id", profileID), slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
		return err
	}
	if _, err := qtx.HardDeleteProfileByIDAndProjectID(ctx, dbwrite.HardDeleteProfileByIDAndProjectIDParams{
		ID:        profileID,
		ProjectID: projectID,
	}); err != nil {
		slog.ErrorContext(ctx, "failed hard-deleting profile", slogx.Error(err),
			slog.String("profile_id", profileID), slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		slog.ErrorContext(ctx, "failed committing erasure pg transaction", slogx.Error(err),
			slog.String("profile_id", profileID), slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
		return err
	}
	return nil
}

// eraseClickHouse hard-deletes every ClickHouse store that holds the subject's
// data. dashboard_event_rollup_daily is intentionally NOT touched — it has no
// per-person key and retains only an anonymous aggregate (decision "a"). The
// per-distinct_id and per-session rollups are insert-triggered MVs, so deleting
// from events does not propagate — they must be deleted explicitly here.
func (s *Service) eraseClickHouse(ctx context.Context, projectID, profileID string, distinctIDs, sessionIDs []string) error {
	if len(sessionIDs) > 0 {
		inClause, inArgs := chInClause(sessionIDs)
		// session_id is a UUID column; compare as string to avoid a UUID/String
		// supertype error on the IN literals.
		if err := s.execMutation(ctx,
			"ALTER TABLE dashboard_session_rollup DELETE WHERE project_id = ? AND toString(session_id) IN "+inClause,
			append([]any{projectID}, inArgs...)...,
		); err != nil {
			return fmt.Errorf("erase session rollup: %w", err)
		}
	}

	if len(distinctIDs) > 0 {
		inClause, inArgs := chInClause(distinctIDs)
		base := append([]any{projectID}, inArgs...)
		if err := s.execMutation(ctx,
			"ALTER TABLE events DELETE WHERE project_id = ? AND distinct_id IN "+inClause,
			base...,
		); err != nil {
			return fmt.Errorf("erase events: %w", err)
		}
		if err := s.execMutation(ctx,
			"ALTER TABLE distinct_id_activity_states DELETE WHERE project_id = ? AND distinct_id IN "+inClause,
			base...,
		); err != nil {
			return fmt.Errorf("erase activity states: %w", err)
		}
	}

	if profileID != "" {
		if err := s.execMutation(ctx,
			"ALTER TABLE profile_aliases DELETE WHERE project_id = ? AND profile_id = ?",
			projectID, profileID,
		); err != nil {
			return fmt.Errorf("erase profile aliases: %w", err)
		}
		if err := s.execMutation(ctx,
			"ALTER TABLE profiles DELETE WHERE project_id = ? AND id = ?",
			projectID, profileID,
		); err != nil {
			return fmt.Errorf("erase profile: %w", err)
		}
	}
	return nil
}

// execMutation runs a ClickHouse ALTER ... DELETE with mutations_sync = 1 so the
// call blocks until the rows are physically removed — required to prove erasure
// (a lightweight delete would defer physical removal to a later merge).
func (s *Service) execMutation(ctx context.Context, query string, args ...any) error {
	return s.ch.Exec(ctx, query+" SETTINGS mutations_sync = 1", args...)
}

func (s *Service) publishProfileSoftDelete(ctx context.Context, projectID, profileID string) {
	msg := &workerprofilesv1.ProfileUpsertMessage{
		ProfileId:  proto.String(profileID),
		ProjectId:  proto.String(projectID),
		IsDeleted:  proto.Bool(true),
		UpdateTime: timestamppb.New(time.Now()),
	}
	data, err := proto.Marshal(msg)
	if err != nil {
		slog.ErrorContext(ctx, "failed marshalling profile soft-delete upsert", slogx.Error(err),
			slog.String("profile_id", profileID), slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
		return
	}
	if err := s.producer.Publish(ctx, natsdeps.ProfileUpsertSubject, data); err != nil {
		slog.ErrorContext(ctx, "failed publishing profile soft-delete upsert", slogx.Error(err),
			slog.String("profile_id", profileID), slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
	}
}

func (s *Service) publishErase(ctx context.Context, projectID, requestID, profileID, externalID string) error {
	msg := &workercompliancev1.EraseMessage{
		ProjectId:  proto.String(projectID),
		RequestId:  proto.String(requestID),
		ProfileId:  proto.String(profileID),
		ExternalId: proto.String(externalID),
	}
	data, err := proto.Marshal(msg)
	if err != nil {
		slog.ErrorContext(ctx, "failed marshalling profile erase message", slogx.Error(err),
			slog.String("request_id", requestID), slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
		return err
	}
	if err := s.producer.Publish(ctx, natsdeps.ComplianceEraseSubject, data); err != nil {
		slog.ErrorContext(ctx, "failed publishing profile erase message", slogx.Error(err),
			slog.String("request_id", requestID), slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
		return err
	}
	return nil
}

// chInClause builds a "(?, ?, ?)" placeholder group for a ClickHouse IN list and
// returns it with the values boxed as []any for positional binding.
func chInClause(values []string) (string, []any) {
	placeholders := make([]string, len(values))
	args := make([]any, len(values))
	for i, v := range values {
		placeholders[i] = "?"
		args[i] = v
	}
	return "(" + strings.Join(placeholders, ", ") + ")", args
}
