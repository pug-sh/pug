package billing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/slogx"
	"github.com/rs/xid"
)

var (
	ErrOrgNotFound = errors.New("billing: org not found")
	// ErrPlanNotFound is a slug the catalog does not have. Distinct from
	// ErrPlanRetired, which is a slug it has but will not hand to a new org.
	ErrPlanNotFound = errors.New("billing: plan not found")
	ErrPlanRetired  = errors.New("billing: plan is retired and cannot be newly assigned")
	// ErrTrialNotSettable guards the one slug that means nothing without a date.
	ErrTrialNotSettable = errors.New("billing: use extend-trial to put an org on the trial plan")
	ErrCustomNeedsQuota = errors.New("billing: the custom plan requires an events override")
	ErrAnchorDayRange   = errors.New("billing: anchor day must be between 1 and 31")
	ErrQuotaNegative    = errors.New("billing: the events override must be positive")
	// ErrTrialNotExtended guards a date that would move the org's trial end
	// backwards, which "extend" must never do.
	ErrTrialNotExtended = errors.New("billing: that trial end is not later than the current one")
	ErrTrialDaysRange   = errors.New("billing: trial extension must be between 1 and 365 days")
	// ErrTrialOnGrantedPlan guards a trial date that would resolve to nothing: a
	// granted plan wins over it, so the write would look like it worked.
	ErrTrialOnGrantedPlan = errors.New("billing: clear the granted plan before extending a trial")
	// ErrNoEntitlement is a clear that found nothing stored. The org is already on
	// the derived floors, but nothing was deleted.
	ErrNoEntitlement = errors.New("billing: no entitlement stored for this org")
)

// Service is the whole package: GetEntitlement for the dashboard, the rest for
// `pug billing`. No RPC mutates an entitlement, so nothing here is behind a
// second type.
type Service struct {
	read  *dbread.Queries
	write *dbwrite.Queries
	pgW   *pgxpool.Pool // every mutation runs in a tx of its own, alongside its history append
	// billingEnabled mirrors PUG_BILLING_ENABLED. Off means a self-hosted install,
	// where every org resolves with no quota at all, so no client can render a
	// limit that does not apply.
	billingEnabled bool
}

// NewService checks the floors at wiring time: a catalog missing one resolves
// every org to a blank plan with no quota, and looks healthy doing it.
func NewService(pgRO *pgxpool.Pool, pgW *pgxpool.Pool, billingEnabled bool) (*Service, error) {
	for _, slug := range []string{SlugFree, SlugTrial} {
		if _, ok := PlanBySlug(slug); !ok {
			return nil, fmt.Errorf("billing: catalog is missing the floor plan %q", slug)
		}
	}
	return &Service{
		read:           dbread.New(pgRO),
		write:          dbwrite.New(pgW),
		pgW:            pgW,
		billingEnabled: billingEnabled,
	}, nil
}

// GetEntitlement resolves what the org may send right now.
func (s *Service) GetEntitlement(ctx context.Context, orgID string, now time.Time) (Entitlement, error) {
	row, err := s.read.GetOrgEntitlement(ctx, orgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Entitlement{}, ErrOrgNotFound
		}
		slog.ErrorContext(ctx, "failed to read the org entitlement", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return Entitlement{}, err
	}
	rec := recordFromRow(row)
	// Detected here rather than in Resolve, which is pure and has no ctx to log
	// against. Only reachable if a slug left the catalog with rows still on it,
	// which only an operator can clear — so this is a warning, not an error a
	// dashboard load should record as an exception on every request.
	if rec.Present {
		if _, known := PlanBySlug(rec.PlanSlug); !known {
			slog.WarnContext(ctx, "entitlement names a plan the catalog does not know",
				slog.String("org_id", orgID), slog.String("plan_slug", rec.PlanSlug))
		}
	}
	return Resolve(row.OrgCreateTime.Time, rec, now, s.billingEnabled), nil
}

// StoredRecord is the row as stored. `pug billing show` prints it beside the
// resolved entitlement, because an override that is not in force today — a
// lapsed deal's quota, say — is invisible in the resolved answer alone.
func (s *Service) StoredRecord(ctx context.Context, orgID string) (Record, error) {
	row, err := s.read.GetOrgEntitlement(ctx, orgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Record{}, ErrOrgNotFound
		}
		slog.ErrorContext(ctx, "failed to read the stored entitlement", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return Record{}, err
	}
	return recordFromRow(row), nil
}

// recordFromRow maps the left-joined read. A NULL plan_slug is the join finding
// no entitlement, which is the ordinary case and not an error.
func recordFromRow(row dbread.GetOrgEntitlementRow) Record {
	if !row.PlanSlug.Valid {
		return Record{}
	}
	rec := Record{
		Present:             true,
		AnchorDay:           postgres.Int2ToInt(row.AnchorDay),
		ContractEndsAt:      row.ContractEndsAt.Time,
		DisplayNameOverride: row.DisplayNameOverride.String,
		Note:                row.Note.String,
		PlanSlug:            row.PlanSlug.String,
		TrialEndsAt:         row.TrialEndsAt.Time,
	}
	if row.IncludedEventsOverride.Valid {
		rec.IncludedEventsOverride = row.IncludedEventsOverride.Int64
	}
	return rec
}

// Change is one operator edit. PlanSlug is required; every other field is nil to
// leave the stored value alone, or a value to write — the zero value being the
// clear.
//
// Leaving values alone is the default because the common re-set is a renewal — a
// new end date on terms that have not changed — and a flag that silently
// reverted a customer's negotiated quota to a catalog number would be the most
// expensive bug this API could have.
type Change struct {
	PlanSlug       string
	IncludedEvents *int64
	DisplayName    *string
	AnchorDay      *int
	ContractEndsAt *time.Time
	Note           *string
}

// orKeep resolves one Change field against the value already stored.
func orKeep[T any](v *T, current T) T {
	if v == nil {
		return current
	}
	return *v
}

// SetPlan grants a plan, merging the change over whatever is stored. Returns the
// row as it now stands.
func (s *Service) SetPlan(ctx context.Context, orgID, actor string, change Change) (Record, error) {
	plan, ok := PlanBySlug(change.PlanSlug)
	if !ok {
		return Record{}, ErrPlanNotFound
	}
	if plan.Slug == SlugTrial {
		return Record{}, ErrTrialNotSettable
	}

	return s.mutate(ctx, orgID, actor, func(cur Record) (Record, error) {
		next := applyChange(cur, change)
		// A retired tier keeps its existing holders but is never handed to somebody
		// new, so this is checked against what the org held BEFORE the change.
		if plan.Retired && cur.PlanSlug != plan.Slug {
			return Record{}, ErrPlanRetired
		}
		if next.PlanSlug == SlugCustom && next.IncludedEventsOverride <= 0 {
			return Record{}, ErrCustomNeedsQuota
		}
		// Mirrors the column's `> 0` check, which would otherwise surface as a raw
		// SQLSTATE logged as a pug fault.
		if next.IncludedEventsOverride < 0 {
			return Record{}, ErrQuotaNegative
		}
		if next.AnchorDay < 0 || next.AnchorDay > 31 {
			return Record{}, ErrAnchorDayRange
		}
		return next, nil
	})
}

// ExtendTrial moves the org's trial end to days from now, which is what puts a
// trial_ends_at on the row at all — every other org's trial is derived from its
// age and stores nothing.
func (s *Service) ExtendTrial(ctx context.Context, orgID, actor string, days int, now time.Time) (Record, error) {
	if days <= 0 || days > MaxTrialDays {
		return Record{}, ErrTrialDaysRange
	}
	// The org's create time, because the trial being extended is usually the
	// derived one and the locked row alone cannot see it.
	orgCreateTime, err := s.orgCreateTime(ctx, orgID)
	if err != nil {
		return Record{}, err
	}
	return s.mutate(ctx, orgID, actor, func(cur Record) (Record, error) {
		// A granted plan resolves ahead of any trial date, so writing one here would
		// store a date that changes nothing and still print as a success. A slug the
		// catalog no longer knows counts as granted: it resolves free without ever
		// consulting a trial date, so the write would be just as empty.
		if cur.Present {
			if plan, ok := PlanBySlug(cur.PlanSlug); !ok || !plan.isFloor() {
				return Record{}, ErrTrialOnGrantedPlan
			}
		}
		next := cur
		next.Present = true
		next.TrialEndsAt = now.AddDate(0, 0, days).UTC().Truncate(time.Second)
		// "Extend" is measured from now, so a small --days on a trial with longer to
		// run would shorten it. Refuse rather than silently cut it short.
		if !next.TrialEndsAt.After(trialEnd(orgCreateTime, cur)) {
			return Record{}, ErrTrialNotExtended
		}
		// An org with no row has no plan either, and plan_slug is NOT NULL. The
		// floor is the honest value: extending a trial grants no tier.
		if next.PlanSlug == "" {
			next.PlanSlug = SlugFree
		}
		return next, nil
	})
}

// Clear deletes the row, returning the org to the derived floors. Recorded in
// the history as a snapshot with no values.
func (s *Service) Clear(ctx context.Context, orgID, actor string) error {
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	w := dbwrite.New(tx)
	// The same lock mutate takes, for the same reason: without it a concurrent
	// SetPlan can insert a row between this delete and its commit, leaving a stored
	// entitlement whose newest history entry says it was cleared.
	if err := w.LockBillingEntitlementOrg(ctx, orgID); err != nil {
		slog.ErrorContext(ctx, "failed to lock the org for a billing clear", slogx.Error(err),
			slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return err
	}
	n, err := w.DeleteBillingEntitlement(ctx, orgID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to delete the entitlement", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return err
	}
	// Deleting nothing is not success: it is either a typo'd org or a row that was
	// never there, and both would otherwise print as "cleared".
	if n == 0 {
		// Through the tx, not s.read: against a real replica, a lagging read would
		// report ErrOrgNotFound for an org that was just created.
		if _, err := w.GetOrgByID(ctx, orgID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrOrgNotFound
			}
			slog.ErrorContext(ctx, "failed to check the org exists", slogx.Error(err), slog.String("org_id", orgID))
			telemetry.RecordError(ctx, err)
			return err
		}
		return ErrNoEntitlement
	}
	if err := appendHistory(ctx, w, orgID, actor, Record{}); err != nil {
		return err
	}
	return s.commit(ctx, tx, orgID)
}

// mutate runs one read-modify-write under a row lock, appending the resulting
// snapshot to the history in the same transaction — so a change and its record
// commit together or not at all.
func (s *Service) mutate(ctx context.Context, orgID, actor string, edit func(Record) (Record, error)) (Record, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return Record{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	w := dbwrite.New(tx)
	// Before the read, because `for update` locks nothing when the row does not
	// exist yet: without this two concurrent first writes both read an empty
	// record and the second full-replaces the first.
	if err := w.LockBillingEntitlementOrg(ctx, orgID); err != nil {
		slog.ErrorContext(ctx, "failed to lock the org for a billing write", slogx.Error(err),
			slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return Record{}, err
	}
	cur, err := currentRecord(ctx, w, orgID)
	if err != nil {
		return Record{}, err
	}

	next, err := edit(cur)
	if err != nil {
		return Record{}, err
	}

	row, err := w.UpsertBillingEntitlement(ctx, upsertParams(orgID, next))
	if err != nil {
		if isOrgFKViolation(err) {
			return Record{}, ErrOrgNotFound
		}
		slog.ErrorContext(ctx, "failed to upsert the entitlement", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return Record{}, err
	}

	stored := recordFromWriteRow(row)
	if err := appendHistory(ctx, w, orgID, actor, stored); err != nil {
		return Record{}, err
	}
	if err := s.commit(ctx, tx, orgID); err != nil {
		return Record{}, err
	}
	return stored, nil
}

func (s *Service) begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.pgW.Begin(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed to begin the billing tx", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return nil, err
	}
	return tx, nil
}

func (s *Service) commit(ctx context.Context, tx pgx.Tx, orgID string) error {
	if err := tx.Commit(ctx); err != nil {
		slog.ErrorContext(ctx, "failed to commit the billing tx", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return err
	}
	return nil
}

// orgCreateTime reads the org's age, which is what the derived trial is measured
// from.
func (s *Service) orgCreateTime(ctx context.Context, orgID string) (time.Time, error) {
	row, err := s.read.GetOrgEntitlement(ctx, orgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return time.Time{}, ErrOrgNotFound
		}
		slog.ErrorContext(ctx, "failed to read the org create time", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return time.Time{}, err
	}
	return row.OrgCreateTime.Time, nil
}

func currentRecord(ctx context.Context, w *dbwrite.Queries, orgID string) (Record, error) {
	row, err := w.GetBillingEntitlementForUpdate(ctx, orgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Record{}, nil
		}
		slog.ErrorContext(ctx, "failed to lock the entitlement", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return Record{}, err
	}
	return recordFromWriteRow(row), nil
}

// recordFromWriteRow maps the writer-side row, which GetBillingEntitlementForUpdate
// and UpsertBillingEntitlement both return.
func recordFromWriteRow(row dbwrite.BillingEntitlement) Record {
	rec := Record{
		Present:             true,
		AnchorDay:           postgres.Int2ToInt(row.AnchorDay),
		ContractEndsAt:      row.ContractEndsAt.Time,
		DisplayNameOverride: row.DisplayNameOverride.String,
		Note:                row.Note,
		PlanSlug:            row.PlanSlug,
		TrialEndsAt:         row.TrialEndsAt.Time,
	}
	if row.IncludedEventsOverride.Valid {
		rec.IncludedEventsOverride = row.IncludedEventsOverride.Int64
	}
	return rec
}

func applyChange(cur Record, c Change) Record {
	next := cur
	next.Present = true
	next.PlanSlug = c.PlanSlug
	next.IncludedEventsOverride = orKeep(c.IncludedEvents, cur.IncludedEventsOverride)
	next.DisplayNameOverride = orKeep(c.DisplayName, cur.DisplayNameOverride)
	next.AnchorDay = orKeep(c.AnchorDay, cur.AnchorDay)
	next.ContractEndsAt = orKeep(c.ContractEndsAt, cur.ContractEndsAt)
	next.Note = orKeep(c.Note, cur.Note)

	if plan, ok := PlanBySlug(next.PlanSlug); ok {
		if !plan.isFloor() {
			// Converting to a paid tier ends the trial: leaving a future trial_ends_at
			// behind would make an entitlement whose state depends on which of two dates
			// the resolver consults first.
			next.TrialEndsAt = time.Time{}
		} else if c.ContractEndsAt == nil {
			// The mirror: the contract belongs to the granted plan, so falling back to a
			// floor tier ends it. Unless this change names one, which is a time-boxed
			// comped grant on the floor.
			next.ContractEndsAt = time.Time{}
		}
	}
	return next
}

func upsertParams(orgID string, rec Record) dbwrite.UpsertBillingEntitlementParams {
	return dbwrite.UpsertBillingEntitlementParams{
		AnchorDay:              postgres.NewOptionalInt2(rec.AnchorDay),
		ContractEndsAt:         postgres.NewOptionalTimestamptz(rec.ContractEndsAt),
		DisplayNameOverride:    postgres.NewOptionalText(rec.DisplayNameOverride),
		IncludedEventsOverride: postgres.NewOptionalInt8(rec.IncludedEventsOverride),
		Note:                   rec.Note,
		OrgID:                  orgID,
		PlanSlug:               rec.PlanSlug,
		TrialEndsAt:            postgres.NewOptionalTimestamptz(rec.TrialEndsAt),
	}
}

func appendHistory(ctx context.Context, w *dbwrite.Queries, orgID, actor string, rec Record) error {
	params := dbwrite.InsertBillingEntitlementHistoryParams{
		Actor:                  actor,
		AnchorDay:              postgres.NewOptionalInt2(rec.AnchorDay),
		ContractEndsAt:         postgres.NewOptionalTimestamptz(rec.ContractEndsAt),
		DisplayNameOverride:    postgres.NewOptionalText(rec.DisplayNameOverride),
		ID:                     xid.New().String(),
		IncludedEventsOverride: postgres.NewOptionalInt8(rec.IncludedEventsOverride),
		Note:                   rec.Note,
		OrgID:                  orgID,
		PlanSlug:               postgres.NewOptionalText(rec.PlanSlug),
		TrialEndsAt:            postgres.NewOptionalTimestamptz(rec.TrialEndsAt),
	}
	if err := w.InsertBillingEntitlementHistory(ctx, params); err != nil {
		slog.ErrorContext(ctx, "failed to append the entitlement history", slogx.Error(err),
			slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return err
	}
	return nil
}

// HistoryEntry is one recorded change, newest first from History.
type HistoryEntry struct {
	Actor     string
	ChangedAt time.Time
	Record    Record
}

// MaxHistoryRows caps one History read. An entitlement changes a handful of
// times a year, so this is decades of it.
const MaxHistoryRows = 200

// History is operator and support data — no RPC serves it.
func (s *Service) History(ctx context.Context, orgID string) ([]HistoryEntry, error) {
	rows, err := s.read.ListBillingEntitlementHistory(ctx, dbread.ListBillingEntitlementHistoryParams{
		OrgID:    orgID,
		RowLimit: MaxHistoryRows,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to read the entitlement history", slogx.Error(err),
			slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return nil, err
	}
	out := make([]HistoryEntry, 0, len(rows))
	for _, row := range rows {
		rec := Record{
			Present:             row.PlanSlug.Valid,
			AnchorDay:           postgres.Int2ToInt(row.AnchorDay),
			ContractEndsAt:      row.ContractEndsAt.Time,
			DisplayNameOverride: row.DisplayNameOverride.String,
			Note:                row.Note,
			PlanSlug:            row.PlanSlug.String,
			TrialEndsAt:         row.TrialEndsAt.Time,
		}
		if row.IncludedEventsOverride.Valid {
			rec.IncludedEventsOverride = row.IncludedEventsOverride.Int64
		}
		out = append(out, HistoryEntry{Actor: row.Actor, ChangedAt: row.ChangedAt.Time, Record: rec})
	}
	return out, nil
}

// isOrgFKViolation reports the upsert failing because no such org exists, which
// is a caller mistake rather than a database fault.
func isOrgFKViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}
