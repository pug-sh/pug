package billing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
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
	ErrTrialNotSettable   = errors.New("billing: use extend-trial to put an org on the trial plan")
	ErrCustomNeedsQuota   = errors.New("billing: the custom plan requires an events override")
	ErrPriceNeedsCurrency = errors.New("billing: a price override requires a currency")
	ErrCurrencyNeedsPrice = errors.New("billing: a currency override requires a price")
	ErrAnchorDayRange     = errors.New("billing: anchor day must be between 1 and 31")
	ErrCurrencyInvalid = errors.New("billing: currency must be a three-letter ISO 4217 code")
	ErrQuotaNegative   = errors.New("billing: the events override must be positive")
	// ErrTrialNotExtended guards a date that would move the org's trial end
	// backwards, which "extend" must never do.
	ErrTrialNotExtended = errors.New("billing: that trial end is not later than the current one")
	ErrTrialDaysRange   = errors.New("billing: trial extension is capped at one year")
	// ErrTrialOnGrantedPlan guards a trial date that would resolve to nothing: a
	// granted plan wins over it, so the write would look like it worked.
	ErrTrialOnGrantedPlan = errors.New("billing: clear the granted plan before extending a trial")
	// ErrNoEntitlement is a clear that found nothing stored. The org is already on
	// the derived floors, but nothing was deleted.
	ErrNoEntitlement = errors.New("billing: no entitlement stored for this org")
)

// Reader answers the dashboard's one question. It holds no write pool, so the
// endpoint on the viewer floor has no reachable path to a mutation.
type Reader struct {
	read *dbread.Queries
	// Off means a self-hosted install: no quota anywhere, decided once here
	// rather than re-read per request.
	billingEnabled bool
}

func NewReader(pgRO *pgxpool.Pool, billingEnabled bool) *Reader {
	// Wiring time rather than the first request: a catalog missing a floor would
	// otherwise serve every org a blank plan with no quota, and look healthy doing it.
	mustPlan(SlugFree)
	mustPlan(SlugTrial)
	return &Reader{read: dbread.New(pgRO), billingEnabled: billingEnabled}
}

// GetEntitlement resolves what the org may send right now.
func (r *Reader) GetEntitlement(ctx context.Context, orgID string, now time.Time) (Entitlement, error) {
	row, err := r.read.GetOrgEntitlement(ctx, orgID)
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
	return Resolve(row.OrgCreateTime.Time, rec, now, r.billingEnabled), nil
}

// StoredRecord is the row as stored. `pug billing show` prints it beside the
// resolved entitlement, because an override that is not in force today — a
// lapsed deal's quota, say — is invisible in the resolved answer alone.
func (r *Reader) StoredRecord(ctx context.Context, orgID string) (Record, error) {
	row, err := r.read.GetOrgEntitlement(ctx, orgID)
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
		AnchorDay:           anchorDay(row.AnchorDay),
		ContractEndsAt:      row.ContractEndsAt.Time,
		CurrencyOverride:    row.CurrencyOverride.String,
		DisplayNameOverride: row.DisplayNameOverride.String,
		Note:                row.Note.String,
		PlanSlug:            row.PlanSlug.String,
		TrialEndsAt:         row.TrialEndsAt.Time,
	}
	if row.IncludedEventsOverride.Valid {
		rec.IncludedEventsOverride = row.IncludedEventsOverride.Int64
	}
	if row.PriceCentsOverride.Valid {
		v := row.PriceCentsOverride.Int64
		rec.PriceCentsOverride = &v
	}
	return rec
}

// Service adds the operator writes. Only `pug billing` builds one — no RPC
// mutates an entitlement, so nothing the server serves can reach these.
type Service struct {
	*Reader
	pool *pgxpool.Pool
}

func NewService(pgRO, pgW *pgxpool.Pool, billingEnabled bool) *Service {
	return &Service{Reader: NewReader(pgRO, billingEnabled), pool: pgW}
}

// Opt is a tri-state update field: unset leaves the stored value alone, Set
// replaces it, Clear removes it.
//
// Leaving values alone is the default because the common re-set is a renewal — a
// new end date on terms that have not changed — and a flag that silently
// reverted a customer's negotiated quota to a catalog number would be the most
// expensive bug this API could have.
type Opt[T any] struct {
	set   bool
	clear bool
	val   T
}

func Set[T any](v T) Opt[T] { return Opt[T]{set: true, val: v} }

func Clear[T any]() Opt[T] { return Opt[T]{clear: true} }

// Apply resolves the option against the value already stored.
func (o Opt[T]) Apply(current T) T {
	switch {
	case o.set:
		return o.val
	case o.clear:
		var zero T
		return zero
	}
	return current
}

// Change is one operator edit. PlanSlug is required; everything else defaults to
// leaving the stored value alone.
type Change struct {
	PlanSlug       string
	IncludedEvents Opt[int64]
	DisplayName    Opt[string]
	PriceCents     Opt[int64]
	Currency       Opt[string]
	AnchorDay      Opt[int]
	ContractEndsAt Opt[time.Time]
	Note           Opt[string]
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
		if next.CurrencyOverride != "" {
			next.CurrencyOverride = strings.ToUpper(next.CurrencyOverride)
			if !isCurrencyCode(next.CurrencyOverride) {
				return Record{}, ErrCurrencyInvalid
			}
		}
		if next.PriceCentsOverride != nil && next.CurrencyOverride == "" {
			return Record{}, ErrPriceNeedsCurrency
		}
		// The mirror of the guard above, because the pair is stored, not passed:
		// clearing only the price leaves the currency behind on the row.
		if next.CurrencyOverride != "" && next.PriceCentsOverride == nil {
			return Record{}, ErrCurrencyNeedsPrice
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
	if days <= 0 {
		return Record{}, fmt.Errorf("billing: trial extension must be positive, got %d", days)
	}
	if days > MaxTrialDays {
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
		// store a date that changes nothing and still print as a success.
		if plan, ok := PlanBySlug(cur.PlanSlug); cur.Present && ok && !plan.isFloor() {
			return Record{}, ErrTrialOnGrantedPlan
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
	n, err := w.DeleteBillingEntitlement(ctx, orgID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to delete the entitlement", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return err
	}
	// Deleting nothing is not success: it is either a typo'd org or a row that was
	// never there, and both would otherwise print as "cleared".
	if n == 0 {
		if _, err := s.read.GetOrgEntitlement(ctx, orgID); err != nil {
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
	tx, err := s.pool.Begin(ctx)
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
		AnchorDay:           anchorDay(row.AnchorDay),
		ContractEndsAt:      row.ContractEndsAt.Time,
		CurrencyOverride:    row.CurrencyOverride.String,
		DisplayNameOverride: row.DisplayNameOverride.String,
		Note:                row.Note,
		PlanSlug:            row.PlanSlug,
		TrialEndsAt:         row.TrialEndsAt.Time,
	}
	if row.IncludedEventsOverride.Valid {
		rec.IncludedEventsOverride = row.IncludedEventsOverride.Int64
	}
	if row.PriceCentsOverride.Valid {
		v := row.PriceCentsOverride.Int64
		rec.PriceCentsOverride = &v
	}
	return rec
}

func applyChange(cur Record, c Change) Record {
	next := cur
	next.Present = true
	next.PlanSlug = c.PlanSlug
	next.IncludedEventsOverride = c.IncludedEvents.Apply(cur.IncludedEventsOverride)
	next.DisplayNameOverride = c.DisplayName.Apply(cur.DisplayNameOverride)
	next.CurrencyOverride = c.Currency.Apply(cur.CurrencyOverride)
	next.AnchorDay = c.AnchorDay.Apply(cur.AnchorDay)
	next.ContractEndsAt = c.ContractEndsAt.Apply(cur.ContractEndsAt)
	next.Note = c.Note.Apply(cur.Note)

	switch {
	case c.PriceCents.set:
		v := c.PriceCents.val
		next.PriceCentsOverride = &v
	case c.PriceCents.clear:
		// The currency goes with it: it is only ever the unit of this price, and
		// leaving it behind would violate the row's currency-needs-price constraint.
		next.PriceCentsOverride = nil
		next.CurrencyOverride = ""
	}
	if plan, ok := PlanBySlug(next.PlanSlug); ok {
		if !plan.isFloor() {
			// Converting to a paid tier ends the trial: leaving a future trial_ends_at
			// behind would make an entitlement whose state depends on which of two dates
			// the resolver consults first.
			next.TrialEndsAt = time.Time{}
		} else if !c.ContractEndsAt.set {
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
		AnchorDay:              nullableInt2(rec.AnchorDay),
		ContractEndsAt:         postgres.NewOptionalTimestamptz(rec.ContractEndsAt),
		CurrencyOverride:       postgres.NewOptionalText(rec.CurrencyOverride),
		DisplayNameOverride:    postgres.NewOptionalText(rec.DisplayNameOverride),
		IncludedEventsOverride: nullableInt8(rec.IncludedEventsOverride),
		Note:                   rec.Note,
		OrgID:                  orgID,
		PlanSlug:               rec.PlanSlug,
		PriceCentsOverride:     pointerInt8(rec.PriceCentsOverride),
		TrialEndsAt:            postgres.NewOptionalTimestamptz(rec.TrialEndsAt),
	}
}

func appendHistory(ctx context.Context, w *dbwrite.Queries, orgID, actor string, rec Record) error {
	params := dbwrite.InsertBillingEntitlementHistoryParams{
		Actor:                  actor,
		AnchorDay:              nullableInt2(rec.AnchorDay),
		ContractEndsAt:         postgres.NewOptionalTimestamptz(rec.ContractEndsAt),
		CurrencyOverride:       postgres.NewOptionalText(rec.CurrencyOverride),
		DisplayNameOverride:    postgres.NewOptionalText(rec.DisplayNameOverride),
		ID:                     xid.New().String(),
		IncludedEventsOverride: nullableInt8(rec.IncludedEventsOverride),
		Note:                   rec.Note,
		OrgID:                  orgID,
		PlanSlug:               postgres.NewOptionalText(rec.PlanSlug),
		PriceCentsOverride:     pointerInt8(rec.PriceCentsOverride),
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
func (r *Reader) History(ctx context.Context, orgID string) ([]HistoryEntry, error) {
	rows, err := r.read.ListBillingEntitlementHistory(ctx, dbread.ListBillingEntitlementHistoryParams{
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
			AnchorDay:           anchorDay(row.AnchorDay),
			ContractEndsAt:      row.ContractEndsAt.Time,
			CurrencyOverride:    row.CurrencyOverride.String,
			DisplayNameOverride: row.DisplayNameOverride.String,
			Note:                row.Note,
			PlanSlug:            row.PlanSlug.String,
			TrialEndsAt:         row.TrialEndsAt.Time,
		}
		if row.IncludedEventsOverride.Valid {
			rec.IncludedEventsOverride = row.IncludedEventsOverride.Int64
		}
		if row.PriceCentsOverride.Valid {
			v := row.PriceCentsOverride.Int64
			rec.PriceCentsOverride = &v
		}
		out = append(out, HistoryEntry{Actor: row.Actor, ChangedAt: row.ChangedAt.Time, Record: rec})
	}
	return out, nil
}

// anchorDay unpacks the nullable column, mirroring nullableInt2 in the other
// direction: 0 is "no override", which is what NULL means here.
func anchorDay(v pgtype.Int2) int {
	if !v.Valid {
		return 0
	}
	return int(v.Int16)
}

func nullableInt2(v int) pgtype.Int2 {
	if v == 0 {
		return pgtype.Int2{}
	}
	return pgtype.Int2{Int16: int16(v), Valid: true}
}

func nullableInt8(v int64) pgtype.Int8 {
	if v == 0 {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: v, Valid: true}
}

func pointerInt8(v *int64) pgtype.Int8 {
	if v == nil {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: *v, Valid: true}
}

// isCurrencyCode reports a well-formed ISO 4217 code. Shape only — the list of
// live codes is not pug's to police; the column's own ^[A-Z]{3}$ check would
// otherwise surface a typo as a raw SQLSTATE logged as a pug fault.
func isCurrencyCode(s string) bool {
	if len(s) != 3 {
		return false
	}
	for _, r := range s {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

// isOrgFKViolation reports the upsert failing because no such org exists, which
// is a caller mistake rather than a database fault.
func isOrgFKViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}
