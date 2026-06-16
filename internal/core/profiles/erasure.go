package profiles

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
	natsdeps "github.com/pug-sh/pug/internal/deps/nats"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	workercompliancev1 "github.com/pug-sh/pug/internal/gen/proto/workers/compliance/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/slogx"
	"github.com/rs/xid"
	"google.golang.org/protobuf/proto"
)

// GDPR/DPDP data-subject erasure (roadmap §4.1). The flow is split: a thin
// synchronous prelude (record the request + soft-delete the PostgreSQL profile +
// enqueue) and a heavy asynchronous worker (resolve the identity fan-out, hard
// delete events + derived rollups + the profile from every store). The worker's
// ClickHouse profile delete is the authoritative read-hide; the prelude does not
// publish a separate ClickHouse tombstone (it would race the worker's delete and
// could resurrect a hidden row). See docs/compliance/4.1-erasure-scope.md.

var (
	ErrDeletionRequestNotFound = errors.New("profiles: deletion request not found")
	// ErrNoErasableIdentifiers is returned when a request resolves to neither an
	// external_id nor a profile — it can never identify data to erase, so it must
	// fail rather than complete as a no-op.
	ErrNoErasableIdentifiers = errors.New("profiles: erasure request resolved no identifiers")
)

// ComplianceStatus is the lifecycle of a compliance request. Mirrors the
// compliance_requests.status CHECK constraint and the proto
// ComplianceRequestStatus enum. The DB column is plain text, so values cross the
// sqlc boundary as strings; this type keeps the service surface honest.
type ComplianceStatus string

const (
	ComplianceStatusPending    ComplianceStatus = "pending"
	ComplianceStatusProcessing ComplianceStatus = "processing"
	ComplianceStatusCompleted  ComplianceStatus = "completed"
	ComplianceStatusFailed     ComplianceStatus = "failed"
)

// ComplianceKind discriminates the unified DSAR ledger. Mirrors the
// compliance_requests.kind CHECK constraint.
type ComplianceKind string

const (
	ComplianceKindErase  ComplianceKind = "erase"
	ComplianceKindExport ComplianceKind = "export"
)

// RequestErasureByID enqueues erasure of the data subject identified by profile
// id (the dashboard "delete this profile" path). The profile must exist; a
// missing or already-deleted profile returns ErrProfileNotFound.
func (s *Service) RequestErasureByID(ctx context.Context, projectID, profileID, requestedBy string) (string, ComplianceStatus, error) {
	// Idempotency / re-drive: reuse an existing non-completed erase request for
	// this profile rather than creating a duplicate ledger row. This must run
	// before the profile lookup below — the prelude soft-deletes the profile, so
	// a retry after the first request would otherwise 404 here and lose the only
	// path to re-drive a failed erasure.
	if requestID, status, ok, err := s.reopenErasure(ctx, projectID, profileID, ""); err != nil {
		return requestID, status, err
	} else if ok {
		return requestID, status, nil
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
func (s *Service) RequestErasureByExternalID(ctx context.Context, projectID, externalID, requestedBy string) (string, ComplianceStatus, error) {
	// Idempotency / re-drive: reuse an existing non-completed erase request for
	// this external_id rather than creating a duplicate ledger row.
	if requestID, status, ok, err := s.reopenErasure(ctx, projectID, "", externalID); err != nil {
		return requestID, status, err
	} else if ok {
		return requestID, status, nil
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
// delete the PostgreSQL profile and deactivate its devices (when one exists),
// then publish the erase message for the worker, which hard-deletes every store
// including the ClickHouse profile. profileID is "" when erasing by external_id
// with no resolved profile.
func (s *Service) requestErasure(ctx context.Context, projectID, externalID, profileID, requestedBy string) (string, ComplianceStatus, error) {
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
		Kind:        string(ComplianceKindErase),
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

	if err := s.publishErase(ctx, projectID, requestID, profileID, externalID); err != nil {
		// The request row is durably committed; only the enqueue failed (already
		// logged + recorded in publishErase). Surface the error so the caller can
		// retry — the record of intent is not lost, and a retry reuses this pending
		// row via reopenErasure rather than creating a duplicate.
		return requestID, ComplianceStatusPending, err
	}

	slog.InfoContext(ctx, "data subject erasure requested",
		slog.String("project_id", projectID), slog.String("request_id", requestID),
		slog.Bool("has_profile", profileID != ""))
	return requestID, ComplianceStatusPending, nil
}

// reopenErasure looks for an existing non-completed erase request for the same
// data subject and, if found, re-drives it instead of letting the caller create
// a duplicate ledger row. A 'failed' request is revived to 'pending'; any match
// (including a 'pending' row whose original enqueue was lost) is re-published.
// The worker resolves identity from the request row's frozen profile_id, so a
// re-drive stays correct even though the prelude has already soft-deleted the
// profile. Returns ok=false when no reopenable request exists, so the caller
// proceeds to create a fresh one. profileID is "" on the external_id path and
// externalID is "" on the by-id path; at least one is always set.
func (s *Service) reopenErasure(ctx context.Context, projectID, profileID, externalID string) (string, ComplianceStatus, bool, error) {
	existing, err := s.read.GetReopenableComplianceRequest(ctx, dbread.GetReopenableComplianceRequestParams{
		ProjectID:  projectID,
		Kind:       string(ComplianceKindErase),
		ProfileID:  profileID,
		ExternalID: externalID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "", false, nil
		}
		slog.ErrorContext(ctx, "failed looking up reopenable erasure request", slogx.Error(err),
			slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
		return "", "", false, err
	}

	status := ComplianceStatus(existing.Status)
	if status == ComplianceStatusFailed {
		if _, err := s.write.ReopenComplianceRequest(ctx, dbwrite.ReopenComplianceRequestParams{
			ID:        existing.ID,
			ProjectID: projectID,
		}); err != nil {
			slog.ErrorContext(ctx, "failed reviving failed erasure request", slogx.Error(err),
				slog.String("request_id", existing.ID), slog.String("project_id", projectID))
			telemetry.RecordError(ctx, err)
			return "", "", false, err
		}
		status = ComplianceStatusPending
	}

	exProfileID := ""
	if existing.ProfileID.Valid {
		exProfileID = existing.ProfileID.String
	}
	exExternalID := ""
	if existing.ExternalID.Valid {
		exExternalID = existing.ExternalID.String
	}
	if err := s.publishErase(ctx, projectID, existing.ID, exProfileID, exExternalID); err != nil {
		// The row is intact; only the re-enqueue failed (logged + recorded in
		// publishErase). Surface the error so the caller can retry — a retry reuses
		// this same row via reopenErasure.
		return existing.ID, status, true, err
	}

	slog.InfoContext(ctx, "re-driving existing data subject erasure request",
		slog.String("project_id", projectID), slog.String("request_id", existing.ID),
		slog.String("prior_status", existing.Status))
	return existing.ID, status, true, nil
}

// ExecuteErasure performs the irreversible hard erasure for a deletion request.
// It is idempotent and retry-safe: the resolved identifiers are frozen on the
// first pass, so a redelivery after events are deleted still cleans every store.
func (s *Service) ExecuteErasure(ctx context.Context, projectID, requestID string) error {
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

	if ComplianceStatus(req.Status) == ComplianceStatusCompleted {
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

// MarkErasureFailed records a permanent erasure failure on the audit row so the
// DSAR ledger reflects reality instead of a request stuck at 'processing'. The
// worker calls this just before a dead-lettered message is terminated. It is
// best-effort: if the ledger write itself fails (e.g. the same outage that
// failed the erasure), the row stays 'processing' until a re-request re-drives
// it. The frozen identifiers remain on the row, so a later re-drive (via
// RequestErasure*) cleans every store correctly.
func (s *Service) MarkErasureFailed(ctx context.Context, projectID, requestID string, cause error) error {
	reason := ""
	if cause != nil {
		reason = truncateError(cause.Error())
	}
	if _, err := s.write.MarkComplianceRequestFailed(ctx, dbwrite.MarkComplianceRequestFailedParams{
		ID:        requestID,
		ProjectID: projectID,
		Error:     postgres.NewOptionalText(reason),
	}); err != nil {
		slog.ErrorContext(ctx, "failed marking erasure request failed", slogx.Error(err),
			slog.String("request_id", requestID), slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
		return err
	}
	slog.WarnContext(ctx, "data subject erasure marked failed",
		slog.String("request_id", requestID), slog.String("project_id", projectID),
		slog.String("status", string(ComplianceStatusFailed)), slog.String("cause", reason))
	return nil
}

// GetDeletionRequest returns the audit row for an erasure request so a
// controller can prove fulfilment.
func (s *Service) GetDeletionRequest(ctx context.Context, projectID, requestID string) (dbread.ComplianceRequest, error) {
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

	if len(distinctIDs) == 0 {
		// Neither an external_id nor a resolvable profile — there is nothing to key
		// the erasure on. The RPCs always set at least one identifier, so this is a
		// corrupt request. Fail it (the worker marks the row failed) rather than
		// freezing an empty set and marking it 'completed' — a completed erasure
		// that deleted nothing would silently misreport DSAR fulfilment.
		slog.ErrorContext(ctx, "erasure request resolved no identifiers; refusing to complete",
			slog.String("request_id", req.ID), slog.String("project_id", req.ProjectID))
		telemetry.RecordError(ctx, ErrNoErasableIdentifiers)
		return nil, nil, ErrNoErasableIdentifiers
	}

	sessionIDs, err := s.resolveSessionIDs(ctx, req.ProjectID, distinctIDs)
	if err != nil {
		return nil, nil, err
	}
	eventsDeleted, err := s.countEvents(ctx, req.ProjectID, distinctIDs)
	if err != nil {
		return nil, nil, err
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
	aliasIDs, err := s.selectStrings(ctx,
		"SELECT DISTINCT alias_id FROM profile_aliases FINAL WHERE project_id = ? AND profile_id = ?",
		projectID, profileID,
	)
	if err != nil {
		return nil, fmt.Errorf("resolve alias ids: %w", err)
	}
	return aliasIDs, nil
}

func (s *Service) resolveSessionIDs(ctx context.Context, projectID string, distinctIDs []string) ([]string, error) {
	inClause, inArgs := chInClause(distinctIDs)
	args := append([]any{projectID}, inArgs...)
	sessionIDs, err := s.selectStrings(ctx,
		"SELECT DISTINCT toString(session_id) FROM events WHERE project_id = ? AND distinct_id IN "+inClause,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("resolve session ids: %w", err)
	}
	return sessionIDs, nil
}

// selectStrings runs a single-column ClickHouse query and collects the column
// into a string slice. clickhouse-go's conn.Select maps each row into a struct,
// so a scalar column must be read by iterating rows explicitly.
func (s *Service) selectStrings(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := s.ch.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := rows.Close(); cerr != nil {
			slog.ErrorContext(ctx, "failed to close ClickHouse rows", slogx.Error(cerr))
			telemetry.RecordError(ctx, cerr)
		}
	}()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
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

// truncateError bounds the length of a cause string persisted to the audit
// row's error column, so a pathological driver error can't bloat the ledger.
func truncateError(s string) string {
	const maxErrorLen = 1024
	if len(s) <= maxErrorLen {
		return s
	}
	return s[:maxErrorLen]
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
