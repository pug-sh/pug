package entitlement

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

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
	// ErrPlanNotFound is a slug that is neither a state nor a catalog plan.
	ErrPlanNotFound = errors.New("billing: plan not found")
	// ErrPlanNotAssignable is a catalog plan given to `pug billing set`: a usage plan
	// is held only through a subscription, so a grant of one would bill nothing.
	ErrPlanNotAssignable = errors.New("billing: a usage plan is held through a subscription; set free or custom")
	// ErrCustomNeedsProduct is a deal with nothing to buy: its price is its product.
	ErrCustomNeedsProduct = errors.New("billing: a custom plan requires a provider product")
	// ErrProductNeedsCustom is a product on anything but a deal, which would offer a
	// buy button for no deal at all.
	ErrProductNeedsCustom = errors.New(`billing: a provider product belongs to a custom plan; clear it with --provider-product ""`)
	// ErrClearWouldStrandSubscription refuses to leave custom, or clear the row,
	// while a live custom subscription maps its renewals through the row's product.
	ErrClearWouldStrandSubscription = errors.New("billing: this org has a live custom subscription; cancel it with the provider first")
	ErrAnchorDayRange               = errors.New("billing: anchor day must be between 1 and 31")
	ErrQuotaNegative                = errors.New("billing: the events override must be positive")
	ErrRetentionNegative            = errors.New("billing: the retention override must be positive")
	ErrDisplayNameLong              = errors.New("billing: the display name override is too long")
	// ErrNoEntitlement is a clear that found nothing stored. The org is already on
	// the free allowance, but nothing was deleted.
	ErrNoEntitlement = errors.New("billing: no entitlement stored for this org")
	// ErrActorRequired guards the history's only attribution: the column rejects
	// only the empty string, so a blank actor would store as unattributed.
	ErrActorRequired = errors.New("billing: an actor is required")
)

// Service is the entitlement store. Its reads, GetEntitlement and StoredRecord,
// serve the dashboard and `pug billing show`; the subscription package reads the stored
// row too, and takes the billing switch and the org lock from here. The mutations
// and History are `pug billing`'s alone. No RPC mutates an entitlement.
type Service struct {
	read *dbread.Queries
	pgW  *pgxpool.Pool // every mutation runs in a tx of its own, alongside its history append
	// billingEnabled mirrors PUG_BILLING_ENABLED. Off is a self-hosted install,
	// where every org resolves with no quota at all.
	billingEnabled bool
}

// NewService checks the catalog at wiring time: CurrentPlan would otherwise panic
// inside Resolve on a request, once per dashboard load.
func NewService(pgRO *pgxpool.Pool, pgW *pgxpool.Pool, billingEnabled bool) (*Service, error) {
	if err := validateCatalog(catalog); err != nil {
		return nil, err
	}
	return &Service{
		read:           dbread.New(pgRO),
		pgW:            pgW,
		billingEnabled: billingEnabled,
	}, nil
}

// BillingEnabled is the switch this service resolves under. The subscription package
// reads it from here rather than holding a copy: two copies can disagree, and one
// response would then report billing off beside a working buy button.
func (s *Service) BillingEnabled() bool { return s.billingEnabled }

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
	// Only when billing is on: with it off every org resolves free regardless, so the
	// read is a query per dashboard load that cannot change it.
	var sub *Subscription
	if s.billingEnabled {
		if sub, err = s.liveSubscription(ctx, orgID); err != nil {
			return Entitlement{}, err
		}
		// Here rather than in Resolve, which is pure and has no ctx. A warning, not an
		// error: only an operator can fix it, and every load would record one.
		if sub != nil && sub.PlanSlug != SlugCustom {
			if _, known := PlanBySlug(sub.PlanSlug); !known {
				slog.WarnContext(ctx, "live subscription names a plan the catalog does not know",
					slog.String("org_id", orgID), slog.String("plan_slug", sub.PlanSlug))
			}
		}
	}
	return Resolve(row.OrgCreateTime.Time, rec, sub, now, s.billingEnabled), nil
}

// StoredRecord is the row as stored. `pug billing show` prints it beside the
// resolved entitlement, where a lapsed deal's quota is invisible, and the
// dashboard and subscription read it as well, chiefly for a deal's product id.
func (s *Service) StoredRecord(ctx context.Context, orgID string) (Record, error) {
	// The write pool, as liveSubscription reads: ConfirmCheckout maps a paid checkout
	// through this row, and a lagging replica would refuse it over a just-pasted
	// product id.
	row, err := dbread.New(s.pgW).GetOrgEntitlement(ctx, orgID)
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
		ProviderProductID:   row.ProviderProductID.String,
	}
	if row.IncludedEventsOverride.Valid {
		rec.IncludedEventsOverride = row.IncludedEventsOverride.Int64
	}
	if row.RetentionDaysOverride.Valid {
		rec.RetentionDaysOverride = row.RetentionDaysOverride.Int64
	}
	return rec
}

// MaxDisplayNameLen mirrors display_name_override's varchar(150), which bounds
// characters rather than bytes.
const MaxDisplayNameLen = 150

// Change is one operator edit. PlanSlug is required; every other field is nil to
// leave the stored value alone, or a value to write — the zero value clearing it.
// Nil keeps, because the common re-set is a renewal on unchanged terms.
type Change struct {
	PlanSlug          string
	IncludedEvents    *int64
	RetentionDays     *int64
	DisplayName       *string
	AnchorDay         *int
	ContractEndsAt    *time.Time
	Note              *string
	ProviderProductID *string
}

// orKeep resolves one Change field against the value already stored.
func orKeep[T any](v *T, current T) T {
	if v == nil {
		return current
	}
	return *v
}

// SetPlan puts an org on free or on a deal, merging the change over whatever is
// stored. A usage plan is held only through a subscription, so it is never set
// here. Returns the row as it now stands.
func (s *Service) SetPlan(ctx context.Context, orgID, actor string, change Change) (Record, error) {
	switch change.PlanSlug {
	case SlugFree, SlugCustom:
	default:
		if _, ok := PlanBySlug(change.PlanSlug); ok {
			return Record{}, ErrPlanNotAssignable
		}
		return Record{}, ErrPlanNotFound
	}
	if strings.TrimSpace(actor) == "" {
		return Record{}, ErrActorRequired
	}

	tx, w, cur, err := s.beginLocked(ctx, orgID)
	if err != nil {
		return Record{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	next := applyChange(cur, change)
	// A deal's price is its product, and a product names a deal and nothing else —
	// the rule the custom_needs_product constraint holds too, said as a sentence.
	if next.PlanSlug == SlugCustom && next.ProviderProductID == "" {
		return Record{}, ErrCustomNeedsProduct
	}
	if next.PlanSlug != SlugCustom && next.ProviderProductID != "" {
		return Record{}, ErrProductNeedsCustom
	}
	// A live custom subscription maps its renewals through this row's product, so
	// leaving custom under one strands it. Through the tx, as Clear reads it: off the
	// pool this waits on a connection it is holding.
	if cur.PlanSlug == SlugCustom && next.PlanSlug != SlugCustom {
		sub, err := readLiveSubscription(ctx, dbread.New(tx), orgID)
		if err != nil {
			return Record{}, err
		}
		if sub != nil && sub.PlanSlug == SlugCustom {
			return Record{}, ErrClearWouldStrandSubscription
		}
	}
	// Mirrors the columns' `> 0` checks, which would otherwise surface as a raw
	// SQLSTATE logged as a pug fault.
	if next.IncludedEventsOverride < 0 {
		return Record{}, ErrQuotaNegative
	}
	if next.RetentionDaysOverride < 0 {
		return Record{}, ErrRetentionNegative
	}
	if next.AnchorDay < 0 || next.AnchorDay > 31 {
		return Record{}, ErrAnchorDayRange
	}
	// Mirrors the column's varchar(150), for the same reason as the checks above.
	if utf8.RuneCountInString(next.DisplayNameOverride) > MaxDisplayNameLen {
		return Record{}, ErrDisplayNameLong
	}
	return s.storeRecord(ctx, tx, w, orgID, actor, next)
}

// Clear deletes the row, returning the org to the free allowance. Recorded in the
// history as a snapshot with no values.
func (s *Service) Clear(ctx context.Context, orgID, actor string) error {
	if strings.TrimSpace(actor) == "" {
		return ErrActorRequired
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	w := dbwrite.New(tx)
	// The same lock the other two take: without it a concurrent SetPlan inserts
	// between this delete and its commit, and the history says "cleared".
	if err := lockOrg(ctx, w, orgID); err != nil {
		return err
	}
	// A live custom subscription takes its negotiated terms from the row this deletes,
	// and maps its renewals through the row's product, so clearing it strands a deal
	// that is still being charged. Under the lock, which a subscription writer takes
	// before it maps its product.
	sub, err := readLiveSubscription(ctx, dbread.New(tx), orgID)
	if err != nil {
		return err
	}
	if sub != nil && sub.PlanSlug == SlugCustom {
		return ErrClearWouldStrandSubscription
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

// WithOrgLock runs fn in a transaction holding the org's billing lock, hands it the
// row as it stands under that lock, and commits only if fn returns nil. It is how a
// writer outside this package maps onto the row: the lock is the one every write
// here takes, held to the commit, so a `billing clear` cannot land between what fn
// reads and what it writes, and nothing fn decides was decided before the lock.
//
// fn gets queries bound to the transaction rather than the transaction itself:
// ending it early would drop the lock with fn's write half made. No row is
// Record{}, the ordinary state. The lock is advisory, so an org that does not
// exist reads the same way.
func (s *Service) WithOrgLock(ctx context.Context, orgID string, fn func(w *dbwrite.Queries, cur Record) error) error {
	tx, w, cur, err := s.beginLocked(ctx, orgID)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(w, cur); err != nil {
		return err
	}
	return s.commit(ctx, tx, orgID)
}

// beginLocked opens the transaction every mutation runs in and hands back the row
// as it stands. The caller owns the rollback.
func (s *Service) beginLocked(ctx context.Context, orgID string) (pgx.Tx, *dbwrite.Queries, Record, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return nil, nil, Record{}, err
	}
	cur, err := lockedRecord(ctx, tx, orgID)
	if err != nil {
		_ = tx.Rollback(ctx)
		return nil, nil, Record{}, err
	}
	return tx, dbwrite.New(tx), cur, nil
}

// lockedRecord takes the org's billing lock on tx and returns the row as it stands
// under it, held until tx ends. A pgx.Tx, not a query handle: off a transaction the
// lock dies with its own statement, and a pool-backed read would hold nothing while
// looking locked.
func lockedRecord(ctx context.Context, tx pgx.Tx, orgID string) (Record, error) {
	w := dbwrite.New(tx)
	if err := lockOrg(ctx, w, orgID); err != nil {
		return Record{}, err
	}
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

// lockOrg goes before the read: `for update` locks nothing when the row does not
// exist yet, so two concurrent first writes would both read an empty record.
func lockOrg(ctx context.Context, w *dbwrite.Queries, orgID string) error {
	if err := w.LockBillingEntitlementOrg(ctx, orgID); err != nil {
		slog.ErrorContext(ctx, "failed to lock the org for a billing write", slogx.Error(err),
			slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return err
	}
	return nil
}

// storeRecord writes the merged row and appends it to the history in the same
// transaction, so a change and its record commit together or not at all.
func (s *Service) storeRecord(
	ctx context.Context, tx pgx.Tx, w *dbwrite.Queries, orgID, actor string, next Record,
) (Record, error) {
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
		ProviderProductID:   row.ProviderProductID.String,
	}
	if row.IncludedEventsOverride.Valid {
		rec.IncludedEventsOverride = row.IncludedEventsOverride.Int64
	}
	if row.RetentionDaysOverride.Valid {
		rec.RetentionDaysOverride = row.RetentionDaysOverride.Int64
	}
	return rec
}

func applyChange(cur Record, c Change) Record {
	next := cur
	next.Present = true
	next.PlanSlug = c.PlanSlug
	next.IncludedEventsOverride = orKeep(c.IncludedEvents, cur.IncludedEventsOverride)
	next.RetentionDaysOverride = orKeep(c.RetentionDays, cur.RetentionDaysOverride)
	next.DisplayNameOverride = orKeep(c.DisplayName, cur.DisplayNameOverride)
	next.AnchorDay = orKeep(c.AnchorDay, cur.AnchorDay)
	next.ContractEndsAt = orKeep(c.ContractEndsAt, cur.ContractEndsAt)
	next.Note = orKeep(c.Note, cur.Note)
	next.ProviderProductID = orKeep(c.ProviderProductID, cur.ProviderProductID)

	if next.PlanSlug == SlugFree && (c.ContractEndsAt == nil || c.ContractEndsAt.IsZero()) {
		// The contract belongs to the deal, so free ends it and the overrides it gated.
		// A real date here is a comped grant and keeps them.
		next.ContractEndsAt = time.Time{}
		next.IncludedEventsOverride = orKeep(c.IncludedEvents, 0)
		next.RetentionDaysOverride = orKeep(c.RetentionDays, 0)
		next.DisplayNameOverride = orKeep(c.DisplayName, "")
		// Dropped with them: a product id left behind would keep offering a buy button
		// for the deal that just ended.
		next.ProviderProductID = orKeep(c.ProviderProductID, "")
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
		ProviderProductID:      postgres.NewOptionalText(rec.ProviderProductID),
		RetentionDaysOverride:  postgres.NewOptionalInt8(rec.RetentionDaysOverride),
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
		ProviderProductID:      postgres.NewOptionalText(rec.ProviderProductID),
		RetentionDaysOverride:  postgres.NewOptionalInt8(rec.RetentionDaysOverride),
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
			ProviderProductID:   row.ProviderProductID.String,
		}
		if row.IncludedEventsOverride.Valid {
			rec.IncludedEventsOverride = row.IncludedEventsOverride.Int64
		}
		if row.RetentionDaysOverride.Valid {
			rec.RetentionDaysOverride = row.RetentionDaysOverride.Int64
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
