package billing

import (
	"context"
	"errors"
	"fmt"
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
	// ErrCustomerNotUnique is one provider customer holding subscriptions for two
	// orgs: the delivery names a buyer, and a buyer is not an org.
	ErrCustomerNotUnique = errors.New("billing: the provider customer maps to more than one org")
	// ErrPlanNotFound is a slug no card answers to. Distinct from ErrPlanRetired,
	// which is a card the catalog has but will not hand to a new org.
	ErrPlanNotFound = errors.New("billing: plan not found")
	ErrPlanRetired  = errors.New("billing: plan is retired and cannot be newly assigned")
	// ErrCustomNeedsPrice: an allowance alone is a free tier nobody agreed to.
	ErrCustomNeedsPrice  = errors.New("billing: a custom plan needs a flat fee or a rate")
	ErrPriceNegative     = errors.New("billing: a fee or rate cannot be negative")
	ErrAnchorDayRange    = errors.New("billing: anchor day must be between 1 and 31")
	ErrQuotaNegative     = errors.New("billing: the events override must be positive")
	ErrRetentionNegative = errors.New("billing: the retention override must be positive")
	ErrDisplayNameLong   = errors.New("billing: the display name override is too long")
	// ErrTrialNotExtended guards a date that would move the org's trial end
	// backwards, which "extend" must never do.
	ErrTrialNotExtended = errors.New("billing: that trial end is not later than the current one")
	ErrTrialDaysRange   = errors.New("billing: trial extension must be between 1 and 365 days")
	// ErrTrialOnGrantedPlan guards a trial date that would resolve to nothing: a
	// deal in force wins over it, so the write would look like it worked.
	ErrTrialOnGrantedPlan = errors.New("billing: clear the deal before extending a trial")
	// ErrTrialOnLapsedContract guards the same empty write reached the other way:
	// a lapsed contract expires the trial branch too.
	ErrTrialOnLapsedContract = errors.New("billing: clear the lapsed contract before extending a trial")
	// ErrNoEntitlement is a clear that found nothing stored. The org is already on
	// the derived floors, but nothing was deleted.
	ErrNoEntitlement = errors.New("billing: no entitlement stored for this org")
	// ErrActorRequired guards the history's only attribution: the column rejects
	// only the empty string, so a blank actor would store as unattributed.
	ErrActorRequired = errors.New("billing: an actor is required")
)

// Service is the whole package: GetEntitlement for the dashboard, the rest for
// `pug billing`. No RPC mutates an entitlement.
type Service struct {
	read *dbread.Queries
	pgW  *pgxpool.Pool // every mutation runs in a tx of its own, alongside its history append
	// billingEnabled mirrors PUG_BILLING_ENABLED. Off is a self-hosted install,
	// where every org resolves with no quota at all.
	billingEnabled bool
	// payments is nil on a deployment with no provider credentials, which is a
	// supported mode: only the buy button is missing.
	payments *Payments
}

// NewService checks the rate cards at wiring time, so a malformed catalog fails
// startup instead of mispricing or panicking inside a request.
func NewService(pgRO *pgxpool.Pool, pgW *pgxpool.Pool, billingEnabled bool, payments *Payments) (*Service, error) {
	if err := checkRateCards(); err != nil {
		return nil, err
	}
	return &Service{
		read:           dbread.New(pgRO),
		pgW:            pgW,
		billingEnabled: billingEnabled,
		payments:       payments,
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
	// Here rather than in Resolve, which is pure and has no ctx. A warning, not an
	// error: only an operator can clear it, and every load would record one.
	if rec.Present && isCardSlug(rec.PlanSlug) {
		if _, known := CardBySlug(rec.PlanSlug); !known {
			slog.WarnContext(ctx, "entitlement names a rate card the catalog does not know",
				slog.String("org_id", orgID), slog.String("plan_slug", rec.PlanSlug))
		}
	}
	// Only when billing is on: with it off every org resolves to the free floor
	// regardless, so the read is a query per dashboard load that cannot change it.
	var sub *Subscription
	if s.billingEnabled {
		if sub, err = s.liveSubscription(ctx, orgID); err != nil {
			return Entitlement{}, err
		}
	}
	return Resolve(row.OrgCreateTime.Time, rec, sub, now, s.billingEnabled), nil
}

// StoredRecord is the row as stored. `pug billing show` prints it beside the
// resolved entitlement, where a lapsed deal's quota is invisible.
func (s *Service) StoredRecord(ctx context.Context, orgID string) (Record, error) {
	// The write pool, as liveSubscription does: on the webhook path a lagging
	// replica would reject a paid delivery over a just-pasted product id.
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

// recordFromRow maps the left-joined read. The entitlement's own create_time is
// what says the row exists: plan_slug is nullable now, so an org can hold a trial
// end or an anchor day with no pin, and a NULL slug no longer means "no row".
func recordFromRow(row dbread.GetOrgEntitlementRow) Record {
	if !row.EntitlementCreateTime.Valid {
		return Record{}
	}
	return Record{
		Present:                true,
		AnchorDay:              postgres.Int2ToInt(row.AnchorDay),
		ContractEndsAt:         row.ContractEndsAt.Time,
		CreateTime:             row.EntitlementCreateTime.Time,
		DisplayNameOverride:    row.DisplayNameOverride.String,
		FlatFeeCents:           row.FlatFeeCents.Int64,
		IncludedEventsOverride: row.IncludedEventsOverride.Int64,
		Note:                   row.Note.String,
		PlanSlug:               row.PlanSlug.String,
		RateCentsPerMillion:    row.RateCentsPerMillion.Int64,
		RetentionDaysOverride:  row.RetentionDaysOverride.Int64,
		TermsEffectiveAt:       row.TermsEffectiveAt.Time,
		TrialEndsAt:            row.TrialEndsAt.Time,
	}
}

// MaxDisplayNameLen mirrors display_name_override's varchar(150), which bounds
// characters rather than bytes.
const MaxDisplayNameLen = 150

// Change is one operator edit. PlanSlug is required; every other field is nil to
// leave the stored value alone, or a value to write — the zero value clearing it.
// Nil keeps, because the common re-set is a renewal on unchanged terms.
type Change struct {
	PlanSlug            string
	IncludedEvents      *int64
	RetentionDays       *int64
	DisplayName         *string
	AnchorDay           *int
	ContractEndsAt      *time.Time
	Note                *string
	FlatFeeCents        *int64
	RateCentsPerMillion *int64
}

// orKeep resolves one Change field against the value already stored.
func orKeep[T any](v *T, current T) T {
	if v == nil {
		return current
	}
	return *v
}

// SetPlan pins a rate card or records a deal, merging the change over whatever is
// stored. An empty slug removes the pin and keeps the rest of the row. Returns
// the row as it now stands.
func (s *Service) SetPlan(ctx context.Context, orgID, actor string, change Change) (Record, error) {
	card, isCard := CardBySlug(change.PlanSlug)
	if change.PlanSlug != "" && change.PlanSlug != SlugCustom && !isCard {
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
	// A retired card keeps its existing holders but is never handed to somebody
	// new, so this is checked against what the org held BEFORE the change.
	if isCard && card.Retired && cur.PlanSlug != card.Slug {
		return Record{}, ErrPlanRetired
	}
	now := time.Now().UTC().Truncate(time.Second)
	// A deal ends a running trial rather than erasing it: a close still skips the days
	// the trial covered.
	if next.PlanSlug == SlugCustom && next.TrialEndsAt.After(now) {
		next.TrialEndsAt = now
	}
	// Stamped when a deal starts or a lapsed one renews; any other write keeps it, so
	// the terms price every day not yet invoiced.
	if _, deal := next.Terms(); !deal {
		next.TermsEffectiveAt = time.Time{}
	} else if _, had := cur.Terms(); !had || contractLapsed(cur, now) && !contractLapsed(next, now) {
		next.TermsEffectiveAt = now
	}
	if next.FlatFeeCents < 0 || next.RateCentsPerMillion < 0 {
		return Record{}, ErrPriceNegative
	}
	if next.PlanSlug == SlugCustom && next.FlatFeeCents == 0 && next.RateCentsPerMillion == 0 {
		return Record{}, ErrCustomNeedsPrice
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

// ExtendTrial moves the org's trial end to days from now, which is the only thing
// that puts a trial_ends_at on the row: every other trial is derived from org age.
func (s *Service) ExtendTrial(ctx context.Context, orgID, actor string, days int, now time.Time) (Record, error) {
	if days <= 0 || days > MaxTrialDays {
		return Record{}, ErrTrialDaysRange
	}
	if strings.TrimSpace(actor) == "" {
		return Record{}, ErrActorRequired
	}

	tx, w, cur, err := s.beginLocked(ctx, orgID)
	if err != nil {
		return Record{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The trial being extended is usually the derived one, which the locked row
	// cannot see. Through the tx: a lagging replica would report ErrOrgNotFound.
	orgCreateTime, err := orgCreateTime(ctx, w, orgID)
	if err != nil {
		return Record{}, err
	}
	// A deal in force resolves ahead of any trial date, so the write would change
	// nothing and still print as a success. A card pin is not a conversion: it
	// prices usage the trial is not charged for, so it does not block one.
	if cur.Present {
		if _, deal := cur.Terms(); deal {
			return Record{}, ErrTrialOnGrantedPlan
		}
		// A lapsed contract expires the trial branch too, so the date would be just
		// as empty — a comped pilot that has ended needs a fresh grant, not a trial.
		if contractLapsed(cur, now) {
			return Record{}, ErrTrialOnLapsedContract
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
	return s.storeRecord(ctx, tx, w, orgID, actor, next)
}

// Clear deletes the row, returning the org to the derived floors. Recorded in
// the history as a snapshot with no values.
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
	if err := appendHistory(ctx, w, orgID, actor, Record{}, true); err != nil {
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
	w := dbwrite.New(tx)
	if err := lockOrg(ctx, w, orgID); err != nil {
		_ = tx.Rollback(ctx)
		return nil, nil, Record{}, err
	}
	cur, err := currentRecord(ctx, w, orgID)
	if err != nil {
		_ = tx.Rollback(ctx)
		return nil, nil, Record{}, err
	}
	return tx, w, cur, nil
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
	if err := appendHistory(ctx, w, orgID, actor, stored, false); err != nil {
		return Record{}, err
	}
	if err := s.commit(ctx, tx, orgID); err != nil {
		return Record{}, err
	}
	return stored, nil
}

// write is the pool-backed writer, for the single-statement paths with no
// history row to commit alongside them.
func (s *Service) write() *dbwrite.Queries { return dbwrite.New(s.pgW) }

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
func orgCreateTime(ctx context.Context, w *dbwrite.Queries, orgID string) (time.Time, error) {
	org, err := w.GetOrgByID(ctx, orgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return time.Time{}, ErrOrgNotFound
		}
		slog.ErrorContext(ctx, "failed to read the org create time", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return time.Time{}, err
	}
	return org.CreateTime.Time, nil
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
	return Record{
		Present:                true,
		AnchorDay:              postgres.Int2ToInt(row.AnchorDay),
		ContractEndsAt:         row.ContractEndsAt.Time,
		CreateTime:             row.CreateTime.Time,
		DisplayNameOverride:    row.DisplayNameOverride.String,
		FlatFeeCents:           row.FlatFeeCents.Int64,
		IncludedEventsOverride: row.IncludedEventsOverride.Int64,
		Note:                   row.Note,
		PlanSlug:               row.PlanSlug.String,
		RateCentsPerMillion:    row.RateCentsPerMillion.Int64,
		RetentionDaysOverride:  row.RetentionDaysOverride.Int64,
		TermsEffectiveAt:       row.TermsEffectiveAt.Time,
		TrialEndsAt:            row.TrialEndsAt.Time,
	}
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
	next.FlatFeeCents = orKeep(c.FlatFeeCents, cur.FlatFeeCents)
	next.RateCentsPerMillion = orKeep(c.RateCentsPerMillion, cur.RateCentsPerMillion)

	if next.PlanSlug != SlugCustom {
		// Only a deal carries money, so a card pin — or no pin at all — cannot leave a
		// price behind for the next custom set to satisfy its guard with.
		next.FlatFeeCents, next.RateCentsPerMillion = 0, 0
	}
	return next
}

func upsertParams(orgID string, rec Record) dbwrite.UpsertBillingEntitlementParams {
	return dbwrite.UpsertBillingEntitlementParams{
		AnchorDay:              postgres.NewOptionalInt2(rec.AnchorDay),
		ContractEndsAt:         postgres.NewOptionalTimestamptz(rec.ContractEndsAt),
		DisplayNameOverride:    postgres.NewOptionalText(rec.DisplayNameOverride),
		FlatFeeCents:           postgres.NewOptionalInt8(rec.FlatFeeCents),
		IncludedEventsOverride: postgres.NewOptionalInt8(rec.IncludedEventsOverride),
		Note:                   rec.Note,
		OrgID:                  orgID,
		PlanSlug:               postgres.NewOptionalText(rec.PlanSlug),
		RateCentsPerMillion:    postgres.NewOptionalInt8(rec.RateCentsPerMillion),
		RetentionDaysOverride:  postgres.NewOptionalInt8(rec.RetentionDaysOverride),
		TermsEffectiveAt:       postgres.NewOptionalTimestamptz(rec.TermsEffectiveAt),
		TrialEndsAt:            postgres.NewOptionalTimestamptz(rec.TrialEndsAt),
	}
}

// appendHistory records the row as it now stands. deleted is the marker a clear
// writes: a NULL plan_slug used to mean one, and a live row can hold one now.
func appendHistory(ctx context.Context, w *dbwrite.Queries, orgID, actor string, rec Record, deleted bool) error {
	params := dbwrite.InsertBillingEntitlementHistoryParams{
		Actor:                  actor,
		AnchorDay:              postgres.NewOptionalInt2(rec.AnchorDay),
		ContractEndsAt:         postgres.NewOptionalTimestamptz(rec.ContractEndsAt),
		Deleted:                deleted,
		DisplayNameOverride:    postgres.NewOptionalText(rec.DisplayNameOverride),
		FlatFeeCents:           postgres.NewOptionalInt8(rec.FlatFeeCents),
		ID:                     xid.New().String(),
		IncludedEventsOverride: postgres.NewOptionalInt8(rec.IncludedEventsOverride),
		Note:                   rec.Note,
		OrgID:                  orgID,
		PlanSlug:               postgres.NewOptionalText(rec.PlanSlug),
		RateCentsPerMillion:    postgres.NewOptionalInt8(rec.RateCentsPerMillion),
		RetentionDaysOverride:  postgres.NewOptionalInt8(rec.RetentionDaysOverride),
		TermsEffectiveAt:       postgres.NewOptionalTimestamptz(rec.TermsEffectiveAt),
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
			Present:                !row.Deleted,
			AnchorDay:              postgres.Int2ToInt(row.AnchorDay),
			ContractEndsAt:         row.ContractEndsAt.Time,
			DisplayNameOverride:    row.DisplayNameOverride.String,
			FlatFeeCents:           row.FlatFeeCents.Int64,
			IncludedEventsOverride: row.IncludedEventsOverride.Int64,
			Note:                   row.Note,
			PlanSlug:               row.PlanSlug.String,
			RateCentsPerMillion:    row.RateCentsPerMillion.Int64,
			RetentionDaysOverride:  row.RetentionDaysOverride.Int64,
			TermsEffectiveAt:       row.TermsEffectiveAt.Time,
			TrialEndsAt:            row.TrialEndsAt.Time,
		}
		out = append(out, HistoryEntry{Actor: row.Actor, ChangedAt: row.ChangedAt.Time, Record: rec})
	}
	return out, nil
}

// Preview prices a count of events on what the org would be charged today: the
// same two functions an invoice uses. It resolves as if the switch were on,
// because the CLI is ungated — a deal is checked before billing is turned on.
func (s *Service) Preview(ctx context.Context, orgID string, events int64, now time.Time) (Entitlement, Quote, error) {
	row, err := s.read.GetOrgEntitlement(ctx, orgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Entitlement{}, Quote{}, ErrOrgNotFound
		}
		slog.ErrorContext(ctx, "failed to read the org entitlement", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return Entitlement{}, Quote{}, err
	}
	sub, err := s.liveSubscription(ctx, orgID)
	if err != nil {
		return Entitlement{}, Quote{}, err
	}
	// With the mandate: a grandfathered org is previewed on the card it pinned,
	// which is the one it will be charged on.
	ent := Resolve(row.OrgCreateTime.Time, recordFromRow(row), sub, now, true)
	quote, ok := ent.quote(events)
	if !ok {
		return ent, Quote{}, fmt.Errorf("%w: %s", ErrPlanNotFound, ent.Slug)
	}
	return ent, quote, nil
}

// isOrgFKViolation reports the upsert failing because no such org exists, which
// is a caller mistake rather than a database fault.
func isOrgFKViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}
