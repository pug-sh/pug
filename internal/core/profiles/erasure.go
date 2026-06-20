package profiles

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
	// ErrComplianceRequestVanished is returned when a guarded ledger write (freeze or
	// completion) matches 0 rows — the row was concurrently removed or its (id,
	// project_id, kind) no longer matches. The freeze guard prevents a destructive
	// delete with no surviving audit row; the completion guard prevents reporting a
	// phantom 'completed' that never landed in the ledger.
	ErrComplianceRequestVanished = errors.New("profiles: compliance request row vanished before write")
	// ErrInvalidComplianceStatus is returned by ParseComplianceStatus when a DB value
	// is not a recognized lifecycle status, so a corrupt column surfaces loudly
	// instead of flowing through as an unchecked cast. (There is no kind parse: the
	// erase read boundary enforces kind = 'erase' in SQL via GetEraseRequestByID,
	// which is stronger than a Go-side check.)
	ErrInvalidComplianceStatus = errors.New("profiles: invalid compliance status")
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

// ParseComplianceStatus validates a raw DB status string at the single read
// boundary, so the rest of the service works with a known-good value rather than
// an unchecked ComplianceStatus(...) cast that would silently propagate a corrupt
// column. The status CHECK constraint makes a bad value unreachable in practice;
// this is the defensive complement.
func ParseComplianceStatus(s string) (ComplianceStatus, error) {
	switch ComplianceStatus(s) {
	case ComplianceStatusPending, ComplianceStatusProcessing, ComplianceStatusCompleted, ComplianceStatusFailed:
		return ComplianceStatus(s), nil
	default:
		return "", fmt.Errorf("%w: %q", ErrInvalidComplianceStatus, s)
	}
}

// isTerminal reports whether the request has reached its final state. Only
// 'completed' is terminal: a 'failed' request can still be revived (reopened) to
// 'pending' by a re-request.
func (s ComplianceStatus) isTerminal() bool { return s == ComplianceStatusCompleted }

// canReopen reports whether a re-driven request in this state needs its status
// reset to 'pending'. Only 'failed' is revived; 'pending'/'processing' are already
// open and are simply re-published.
func (s ComplianceStatus) canReopen() bool { return s == ComplianceStatusFailed }

// erasureScope is the frozen identifier fan-out for one erasure: the distinct_id
// set (events + per-distinct_id rollups) and the session_id set (session rollup),
// bundled so the two transposable []string slices can't be swapped at a call site.
type erasureScope struct {
	distinctIDs []string
	sessionIDs  []string
}

// Postgres open-status unique indexes (migration 015) that dedup concurrent
// first-time requests for the same subject.
const (
	complianceOpenProfileUnique  = "compliance_requests_open_profile_uniq"
	complianceOpenExternalUnique = "compliance_requests_open_external_uniq"
)

// isComplianceOpenRequestConflict reports whether err is the unique-violation
// raised when a concurrent first-time request for the same subject already holds
// the open (pending/processing) slot. The caller re-drives the winner instead of
// surfacing the error.
func isComplianceOpenRequestConflict(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == pgerrcode.UniqueViolation &&
		(pgErr.ConstraintName == complianceOpenProfileUnique ||
			pgErr.ConstraintName == complianceOpenExternalUnique)
}

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
// delete the PostgreSQL profile and deactivate its devices (when a profile
// resolved), then publish the erase message for the worker, which hard-deletes
// every store including the ClickHouse profile. profileID is "" when erasing by
// external_id with no resolved profile.
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
		// A concurrent first-time request for the same subject won the open-status
		// unique index (migration 015). This is the concurrent twin of the sequential
		// reopen gate at the top — abandon this tx and re-drive the winner instead of
		// surfacing a duplicate-key error. Not logged as an error: it's an expected,
		// handled race, not a fault.
		if isComplianceOpenRequestConflict(err) {
			if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
				slog.ErrorContext(ctx, "failed rolling back after compliance insert conflict", slogx.Error(rbErr),
					slog.String("project_id", projectID), slog.String("request_id", requestID))
				telemetry.RecordError(ctx, rbErr)
			}
			if rid, status, ok, rerr := s.reopenErasure(ctx, projectID, profileID, externalID); rerr != nil {
				return rid, status, rerr
			} else if ok {
				return rid, status, nil
			}
			// Conflict but nothing reopenable (the winner completed between our failed
			// insert and the reopen lookup): fall through to report the original error.
		}
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

	status, err := ParseComplianceStatus(existing.Status)
	if err != nil {
		slog.ErrorContext(ctx, "reopenable erasure request has invalid status", slogx.Error(err),
			slog.String("request_id", existing.ID), slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
		return "", "", false, err
	}
	if status.canReopen() {
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
	req, err := s.read.GetEraseRequestByID(ctx, dbread.GetEraseRequestByIDParams{
		ID:        requestID,
		ProjectID: projectID,
	})
	if err != nil {
		// Not-found also covers a non-erase (e.g. export) row carrying this id: the
		// query is scoped to kind = 'erase', so the erase path can never hard-delete
		// against an export request.
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrDeletionRequestNotFound
		}
		slog.ErrorContext(ctx, "failed loading deletion request", slogx.Error(err),
			slog.String("request_id", requestID), slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
		return err
	}

	status, err := ParseComplianceStatus(req.Status)
	if err != nil {
		slog.ErrorContext(ctx, "deletion request has invalid status", slogx.Error(err),
			slog.String("request_id", requestID), slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
		return err
	}
	if status.isTerminal() {
		slog.InfoContext(ctx, "deletion request already completed, skipping",
			slog.String("request_id", requestID), slog.String("project_id", projectID))
		return nil
	}

	scope := erasureScope{distinctIDs: req.DistinctIds, sessionIDs: req.SessionIds}
	if len(scope.distinctIDs) == 0 {
		scope, err = s.freezeIdentifiers(ctx, &req)
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

	if err := s.eraseClickHouse(ctx, projectID, profileID, scope); err != nil {
		return err
	}

	// Guard the proof-of-fulfilment write. If it matched 0 rows the row was
	// concurrently removed (the (id, project_id, kind='erase') no longer matches),
	// so the ledger holds no 'completed' record. Fail loudly so the message
	// Naks/retries rather than logging a phantom completion the audit trail can't
	// back. The common path loaded the row at the top, so this is a high-consequence
	// edge, not an everyday case.
	rows, err := s.write.MarkComplianceRequestCompleted(ctx, dbwrite.MarkComplianceRequestCompletedParams{
		ID:        requestID,
		ProjectID: projectID,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed marking deletion request completed", slogx.Error(err),
			slog.String("request_id", requestID), slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
		return err
	}
	if rows == 0 {
		slog.ErrorContext(ctx, "deletion request completion matched no rows; refusing to report phantom completion",
			slog.String("request_id", requestID), slog.String("project_id", projectID))
		telemetry.RecordError(ctx, ErrComplianceRequestVanished)
		return ErrComplianceRequestVanished
	}

	slog.InfoContext(ctx, "data subject erasure completed",
		slog.String("request_id", requestID), slog.String("project_id", projectID),
		slog.Int("distinct_ids", len(scope.distinctIDs)), slog.Int("session_ids", len(scope.sessionIDs)))
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
	// Scoped to kind = 'erase' (GetEraseRequestByID): an export row carrying this id
	// reads as not-found, so the erasure-status RPC can never surface an export row.
	req, err := s.read.GetEraseRequestByID(ctx, dbread.GetEraseRequestByIDParams{
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

// ListStuckComplianceRequests returns open (pending|processing) ledger rows older
// than `olderThan`, capped at `limit`. It is an operability / SLA backstop: a row
// whose enqueue was lost (publish failure) or aged out of the compliance stream
// (max_age 720h) sits open with nothing to re-drive it, so the subject looks
// erased (profile hidden) while PII is physically intact. Surface these to
// alerting; an operator re-drives erasure rows via a fresh RequestErasure* call
// (frozen identifiers keep the re-drive correct). See
// docs/compliance/4.1-erasure-scope.md.
func (s *Service) ListStuckComplianceRequests(ctx context.Context, olderThan time.Time, limit int32) ([]dbread.ComplianceRequest, error) {
	rows, err := s.read.ListStuckComplianceRequests(ctx, dbread.ListStuckComplianceRequestsParams{
		OlderThan: postgres.NewTimestamptz(olderThan),
		RowLimit:  limit,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed listing stuck compliance requests", slogx.Error(err),
			slog.Time("older_than", olderThan))
		telemetry.RecordError(ctx, err)
		return nil, err
	}
	return rows, nil
}

// freezeIdentifiers resolves the full distinct_id fan-out and the session_ids
// (read from events BEFORE they are deleted) and persists them onto the request
// row so retries reuse the frozen set. Any resolution failure aborts without
// persisting, so a retry re-resolves cleanly.
func (s *Service) freezeIdentifiers(ctx context.Context, req *dbread.ComplianceRequest) (erasureScope, error) {
	distinctIDs, err := s.resolveDistinctIDs(ctx, req)
	if err != nil {
		return erasureScope{}, err
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
		return erasureScope{}, ErrNoErasableIdentifiers
	}

	sessionIDs, err := s.resolveSessionIDs(ctx, req.ProjectID, distinctIDs)
	if err != nil {
		return erasureScope{}, err
	}
	eventsIdentified, err := s.countEvents(ctx, req.ProjectID, distinctIDs)
	if err != nil {
		return erasureScope{}, err
	}

	// Guard the freeze with its rows-affected count: this UPDATE advances the row to
	// 'processing' and persists the frozen identifier set *before* any destructive
	// delete. If it matched 0 rows the row is gone, so aborting here is what keeps a
	// hard delete from running with no surviving audit row (the complement of the
	// no-identifiers guard above).
	frozen, err := s.write.FreezeComplianceRequestIdentifiers(ctx, dbwrite.FreezeComplianceRequestIdentifiersParams{
		ID:             req.ID,
		ProjectID:      req.ProjectID,
		DistinctIds:    distinctIDs,
		SessionIds:     sessionIDs,
		EventsAffected: int64(eventsIdentified),
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed freezing erasure identifiers", slogx.Error(err),
			slog.String("request_id", req.ID), slog.String("project_id", req.ProjectID))
		telemetry.RecordError(ctx, err)
		return erasureScope{}, err
	}
	if frozen == 0 {
		slog.ErrorContext(ctx, "erasure freeze matched no rows; aborting before any delete",
			slog.String("request_id", req.ID), slog.String("project_id", req.ProjectID))
		telemetry.RecordError(ctx, ErrComplianceRequestVanished)
		return erasureScope{}, ErrComplianceRequestVanished
	}
	return erasureScope{distinctIDs: distinctIDs, sessionIDs: sessionIDs}, nil
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
		err = fmt.Errorf("resolve alias ids: %w", err)
		slog.ErrorContext(ctx, "failed resolving alias ids for erasure", slogx.Error(err),
			slog.String("profile_id", profileID), slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
		return nil, err
	}
	return aliasIDs, nil
}

// resolveSessionIDs collects the subject's session_ids, batching the distinct_id
// IN list (S1) so a huge fan-out can't blow ClickHouse max_query_size. SELECT
// DISTINCT only dedups within a batch, so cross-batch duplicates are removed here.
func (s *Service) resolveSessionIDs(ctx context.Context, projectID string, distinctIDs []string) ([]string, error) {
	seen := make(map[string]struct{})
	var sessionIDs []string
	err := forEachBatch(distinctIDs, func(batch []string) error {
		inClause, inArgs := chInClause(batch)
		args := append([]any{projectID}, inArgs...)
		ids, err := s.selectStrings(ctx,
			"SELECT DISTINCT toString(session_id) FROM events WHERE project_id = ? AND distinct_id IN "+inClause,
			args...,
		)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			sessionIDs = append(sessionIDs, id)
		}
		return nil
	})
	if err != nil {
		err = fmt.Errorf("resolve session ids: %w", err)
		slog.ErrorContext(ctx, "failed resolving session ids for erasure", slogx.Error(err),
			slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
		return nil, err
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

// countEvents totals events.count() across the subject's distinct_ids, batching the
// IN list (S1) and summing per batch.
func (s *Service) countEvents(ctx context.Context, projectID string, distinctIDs []string) (uint64, error) {
	var total uint64
	err := forEachBatch(distinctIDs, func(batch []string) error {
		inClause, inArgs := chInClause(batch)
		args := append([]any{projectID}, inArgs...)
		var count uint64
		if err := s.ch.QueryRow(ctx,
			"SELECT count() FROM events WHERE project_id = ? AND distinct_id IN "+inClause,
			args...,
		).Scan(&count); err != nil {
			return err
		}
		total += count
		return nil
	})
	if err != nil {
		err = fmt.Errorf("count events: %w", err)
		slog.ErrorContext(ctx, "failed counting events for erasure", slogx.Error(err),
			slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
		return 0, err
	}
	return total, nil
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
// from events does not propagate — they must be deleted explicitly here. Each
// IN-list mutation is batched (S1) so a huge fan-out can't blow max_query_size,
// and every CH failure is recorded at this detect site (I1).
func (s *Service) eraseClickHouse(ctx context.Context, projectID, profileID string, scope erasureScope) error {
	// session_id is a UUID column; compare as string to avoid a UUID/String
	// supertype error on the IN literals.
	if err := s.eraseByBatch(ctx, "erase session rollup", projectID, scope.sessionIDs, func(in string) string {
		return "ALTER TABLE dashboard_session_rollup DELETE WHERE project_id = ? AND toString(session_id) IN " + in
	}); err != nil {
		return err
	}
	if err := s.eraseByBatch(ctx, "erase events", projectID, scope.distinctIDs, func(in string) string {
		return "ALTER TABLE events DELETE WHERE project_id = ? AND distinct_id IN " + in
	}); err != nil {
		return err
	}
	if err := s.eraseByBatch(ctx, "erase activity states", projectID, scope.distinctIDs, func(in string) string {
		return "ALTER TABLE distinct_id_activity_states DELETE WHERE project_id = ? AND distinct_id IN " + in
	}); err != nil {
		return err
	}

	if profileID != "" {
		if err := s.execMutation(ctx,
			"ALTER TABLE profile_aliases DELETE WHERE project_id = ? AND profile_id = ?",
			projectID, profileID,
		); err != nil {
			return s.recordEraseError(ctx, "erase profile aliases", projectID, err)
		}
		if err := s.execMutation(ctx,
			"ALTER TABLE profiles DELETE WHERE project_id = ? AND id = ?",
			projectID, profileID,
		); err != nil {
			return s.recordEraseError(ctx, "erase profile", projectID, err)
		}
	}
	return nil
}

// eraseByBatch runs one DELETE mutation per chBatchSize-bounded chunk of ids, with
// the IN clause spliced in by queryFor. A 0-length id set is a no-op. On failure it
// records once at this detect site and returns the wrapped error. DELETE is
// idempotent across batches, so a mid-batch retry stays correct.
func (s *Service) eraseByBatch(ctx context.Context, op, projectID string, ids []string, queryFor func(inClause string) string) error {
	err := forEachBatch(ids, func(batch []string) error {
		inClause, inArgs := chInClause(batch)
		return s.execMutation(ctx, queryFor(inClause), append([]any{projectID}, inArgs...)...)
	})
	if err != nil {
		return s.recordEraseError(ctx, op, projectID, err)
	}
	return nil
}

// recordEraseError wraps, logs, and records a ClickHouse erase failure at the
// detect site (CLAUDE.md: the detecting layer pairs slog.ErrorContext with
// telemetry.RecordError; ExecuteErasure and the worker only translate). Without
// this the most safety-critical path — a failed erasure mutation — was
// observability-blind.
func (s *Service) recordEraseError(ctx context.Context, op, projectID string, err error) error {
	wrapped := fmt.Errorf("%s: %w", op, err)
	slog.ErrorContext(ctx, "clickhouse erasure mutation failed",
		slog.String("op", op), slog.String("project_id", projectID), slogx.Error(wrapped))
	telemetry.RecordError(ctx, wrapped)
	return wrapped
}

// execMutation runs a ClickHouse heavyweight ALTER ... DELETE with
// mutations_sync = 1 so the call blocks until the rows are physically removed —
// required to prove erasure. (ClickHouse's lightweight DELETE FROM only marks
// rows and defers physical removal to a later merge, so it can't prove erasure;
// mutations_sync does not apply to it.)
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

// chBatchSize bounds the IN-list size per ClickHouse statement (S1). A subject with
// a large alias/session fan-out would otherwise produce one giant (?, ?, …) query
// that can exceed max_query_size / the max-AST-elements limit and fail the mutation
// — retried forever until the DLQ, never completing. A char(20) id binds as ~1
// placeholder + ~20-byte value, so 1000 ids stays well under the 256 KiB default.
const chBatchSize = 1000

// forEachBatch splits values into chunks of at most chBatchSize and invokes fn per
// chunk, stopping at the first error. An empty slice invokes fn zero times.
func forEachBatch(values []string, fn func(batch []string) error) error {
	for start := 0; start < len(values); start += chBatchSize {
		end := start + chBatchSize
		if end > len(values) {
			end = len(values)
		}
		if err := fn(values[start:end]); err != nil {
			return err
		}
	}
	return nil
}
