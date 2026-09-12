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
	coreusage "github.com/pug-sh/pug/internal/core/usage"
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
	ErrPlanNotFound      = errors.New("billing: plan not found")
	ErrPlanRetired       = errors.New("billing: plan is retired and cannot be newly assigned")
	ErrTrialNotSettable  = errors.New("billing: use extend-trial to put an org on the trial plan")
	// ErrCustomNeedsPrice: an allowance alone is a free tier nobody agreed to.
	ErrCustomNeedsPrice        = errors.New("billing: a custom plan needs a flat fee or a block rate")
	ErrAllowanceNotWholeBlocks = errors.New("billing: the events override must be a whole number of 100,000-event blocks")
	ErrPriceNegative           = errors.New("billing: a fee or rate cannot be negative")
	ErrAnchorDayRange          = errors.New("billing: anchor day must be between 1 and 31")
	ErrQuotaNegative           = errors.New("billing: the events override must be positive")
	ErrRetentionNegative       = errors.New("billing: the retention override must be positive")
	ErrDisplayNameLong         = errors.New("billing: the display name override is too long")
	ErrTrialNotExtended        = errors.New("billing: that trial end is not later than the current one")
	ErrTrialDaysRange          = errors.New("billing: trial extension must be between 1 and 365 days")
	// ErrTrialOnGrantedPlan guards a trial date that would resolve to nothing: a
	// deal in force wins over it, so the write would look like it worked.
	ErrTrialOnGrantedPlan    = errors.New("billing: clear the granted plan before extending a trial")
	ErrTrialOnLapsedContract = errors.New("billing: clear the lapsed contract before extending a trial")
	ErrNoEntitlement         = errors.New("billing: no entitlement stored for this org")
	ErrActorRequired         = errors.New("billing: an actor is required")
)

// Service is the whole package: the dashboard reads, the operator writes, the
// webhook, the reconcile pass and the invoicing pass.
type Service struct {
	read  *dbread.Queries
	pgW   *pgxpool.Pool
	cfg   Config
	usage *coreusage.Service
	// payments is nil on a deployment with no provider credentials, which is a
	// supported mode: only the buy button is missing.
	payments *Payments
}

// NewService checks the catalog at wiring time so a malformed card fails startup
// rather than an invoice.
func NewService(pgRO *pgxpool.Pool, pgW *pgxpool.Pool, cfg Config, payments *Payments) (*Service, error) {
	if err := checkCatalog(); err != nil {
		return nil, err
	}
	return &Service{
		read:     dbread.New(pgRO),
		pgW:      pgW,
		cfg:      cfg,
		usage:    coreusage.NewService(pgRO, pgW),
		payments: payments,
	}, nil
}

// GetEntitlement resolves what the org may send right now and what it will be
// charged for it.
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
	if rec.Present && isCardSlug(rec.PlanSlug) {
		if _, known := CardBySlug(rec.PlanSlug); !known {
			slog.WarnContext(ctx, "entitlement names a rate card the catalog does not know",
				slog.String("org_id", orgID), slog.String("plan_slug", rec.PlanSlug))
		}
	}
	if !s.cfg.Enabled {
		return Resolve(row.OrgCreateTime.Time, rec, nil, now, false), nil
	}
	sub, err := s.liveSubscription(ctx, orgID)
	if err != nil {
		return Entitlement{}, err
	}
	ent := Resolve(row.OrgCreateTime.Time, rec, sub, now, true)
	ent.NextChargeAt = ent.PeriodEnd.Add(s.cfg.Grace())
	dunning, err := dbread.New(s.pgW).HasDunningBillingInvoice(ctx, orgID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to read the org's dunning invoices", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return Entitlement{}, err
	}
	if dunning {
		ent.Status = StatusPastDue
	}
	return ent, nil
}

// StoredRecord is the row as stored. `pug billing show` prints it beside the
// resolved entitlement, where a lapsed deal's terms are invisible.
func (s *Service) StoredRecord(ctx context.Context, orgID string) (Record, error) {
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

func recordFromRow(row dbread.GetOrgEntitlementRow) Record {
	if !row.PlanSlug.Valid {
		return Record{}
	}
	return Record{
		Present:                true,
		AnchorDay:              postgres.Int2ToInt(row.AnchorDay),
		BlockRateCents:         row.BlockRateCents.Int64,
		ContractEndsAt:         row.ContractEndsAt.Time,
		CreateTime:             row.EntitlementCreateTime.Time,
		DisplayNameOverride:    row.DisplayNameOverride.String,
		FlatFeeCents:           row.FlatFeeCents.Int64,
		IncludedEventsOverride: row.IncludedEventsOverride.Int64,
		Note:                   row.Note.String,
		PlanSlug:               row.PlanSlug.String,
		RetentionDaysOverride:  row.RetentionDaysOverride.Int64,
		TrialEndsAt:            row.TrialEndsAt.Time,
	}
}

const MaxDisplayNameLen = 150

// Change is one operator edit. PlanSlug is required; every other field is nil to
// leave the stored value alone, or a value to write, the zero value clearing it.
type Change struct {
	PlanSlug       string
	IncludedEvents *int64
	RetentionDays  *int64
	DisplayName    *string
	AnchorDay      *int
	ContractEndsAt *time.Time
	Note           *string
	FlatFeeCents   *int64
	BlockRateCents *int64
}

func orKeep[T any](v *T, current T) T {
	if v == nil {
		return current
	}
	return *v
}

// SetPlan pins a card or records a deal, merging the change over whatever is
// stored. Returns the row as it now stands.
func (s *Service) SetPlan(ctx context.Context, orgID, actor string, change Change) (Record, error) {
	if change.PlanSlug == SlugTrial {
		return Record{}, ErrTrialNotSettable
	}
	card, isCard := CardBySlug(change.PlanSlug)
	if !isCard && change.PlanSlug != SlugFree && change.PlanSlug != SlugCustom {
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
	if isCard && retiredGrant(cur, card) {
		return Record{}, ErrPlanRetired
	}
	if next.FlatFeeCents < 0 || next.BlockRateCents < 0 {
		return Record{}, ErrPriceNegative
	}
	if next.PlanSlug == SlugCustom && next.FlatFeeCents == 0 && next.BlockRateCents == 0 {
		return Record{}, ErrCustomNeedsPrice
	}
	if next.IncludedEventsOverride < 0 {
		return Record{}, ErrQuotaNegative
	}
	if next.IncludedEventsOverride%BlockEvents != 0 {
		return Record{}, ErrAllowanceNotWholeBlocks
	}
	if next.RetentionDaysOverride < 0 {
		return Record{}, ErrRetentionNegative
	}
	if next.AnchorDay < 0 || next.AnchorDay > 31 {
		return Record{}, ErrAnchorDayRange
	}
	if utf8.RuneCountInString(next.DisplayNameOverride) > MaxDisplayNameLen {
		return Record{}, ErrDisplayNameLong
	}
	return s.storeRecord(ctx, tx, w, orgID, actor, next)
}

// ExtendTrial moves the org's trial end to days from now, which is the only thing
// that puts a trial_ends_at on the row.
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

	orgCreateTime, err := orgCreateTime(ctx, w, orgID)
	if err != nil {
		return Record{}, err
	}
	if cur.Present {
		if _, deal := cur.Terms(); deal {
			return Record{}, ErrTrialOnGrantedPlan
		}
		if contractLapsed(cur, now) {
			return Record{}, ErrTrialOnLapsedContract
		}
	}
	next := cur
	next.Present = true
	next.TrialEndsAt = now.AddDate(0, 0, days).UTC().Truncate(time.Second)
	if !next.TrialEndsAt.After(trialEnd(orgCreateTime, cur)) {
		return Record{}, ErrTrialNotExtended
	}
	if next.PlanSlug == "" {
		next.PlanSlug = SlugFree
	}
	return s.storeRecord(ctx, tx, w, orgID, actor, next)
}

// Clear deletes the row, returning the org to the derived floors and the current
// card. Recorded in the history as a snapshot with no values.
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
	if err := lockOrg(ctx, w, orgID); err != nil {
		return err
	}
	n, err := w.DeleteBillingEntitlement(ctx, orgID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to delete the entitlement", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return err
	}
	if n == 0 {
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
// exist yet.
func lockOrg(ctx context.Context, w *dbwrite.Queries, orgID string) error {
	if err := w.LockBillingEntitlementOrg(ctx, orgID); err != nil {
		slog.ErrorContext(ctx, "failed to lock the org for a billing write", slogx.Error(err),
			slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return err
	}
	return nil
}

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

func recordFromWriteRow(row dbwrite.BillingEntitlement) Record {
	return Record{
		Present:                true,
		AnchorDay:              postgres.Int2ToInt(row.AnchorDay),
		BlockRateCents:         row.BlockRateCents.Int64,
		ContractEndsAt:         row.ContractEndsAt.Time,
		CreateTime:             row.CreateTime.Time,
		DisplayNameOverride:    row.DisplayNameOverride.String,
		FlatFeeCents:           row.FlatFeeCents.Int64,
		IncludedEventsOverride: row.IncludedEventsOverride.Int64,
		Note:                   row.Note,
		PlanSlug:               row.PlanSlug,
		RetentionDaysOverride:  row.RetentionDaysOverride.Int64,
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
	next.BlockRateCents = orKeep(c.BlockRateCents, cur.BlockRateCents)

	if next.PlanSlug != SlugCustom {
		// Only a deal carries a price, so a card pin cannot leave one behind for the
		// next custom set to satisfy its guard with.
		next.FlatFeeCents, next.BlockRateCents = 0, 0
	}
	switch next.PlanSlug {
	case SlugCustom:
		// A deal in force resolves ahead of any trial date.
		next.TrialEndsAt = time.Time{}
	case SlugFree:
		// The contract belongs to the deal, so the floor ends it and the overrides
		// it gated. A real date here is a comped grant and keeps them.
		if c.ContractEndsAt == nil || c.ContractEndsAt.IsZero() {
			next.ContractEndsAt = time.Time{}
			next.IncludedEventsOverride = orKeep(c.IncludedEvents, 0)
			next.RetentionDaysOverride = orKeep(c.RetentionDays, 0)
			next.DisplayNameOverride = orKeep(c.DisplayName, "")
		}
	}
	return next
}

func upsertParams(orgID string, rec Record) dbwrite.UpsertBillingEntitlementParams {
	return dbwrite.UpsertBillingEntitlementParams{
		AnchorDay:              postgres.NewOptionalInt2(rec.AnchorDay),
		BlockRateCents:         postgres.NewOptionalInt8(rec.BlockRateCents),
		ContractEndsAt:         postgres.NewOptionalTimestamptz(rec.ContractEndsAt),
		DisplayNameOverride:    postgres.NewOptionalText(rec.DisplayNameOverride),
		FlatFeeCents:           postgres.NewOptionalInt8(rec.FlatFeeCents),
		IncludedEventsOverride: postgres.NewOptionalInt8(rec.IncludedEventsOverride),
		Note:                   rec.Note,
		OrgID:                  orgID,
		PlanSlug:               rec.PlanSlug,
		RetentionDaysOverride:  postgres.NewOptionalInt8(rec.RetentionDaysOverride),
		TrialEndsAt:            postgres.NewOptionalTimestamptz(rec.TrialEndsAt),
	}
}

func appendHistory(ctx context.Context, w *dbwrite.Queries, orgID, actor string, rec Record) error {
	params := dbwrite.InsertBillingEntitlementHistoryParams{
		Actor:                  actor,
		AnchorDay:              postgres.NewOptionalInt2(rec.AnchorDay),
		BlockRateCents:         postgres.NewOptionalInt8(rec.BlockRateCents),
		ContractEndsAt:         postgres.NewOptionalTimestamptz(rec.ContractEndsAt),
		DisplayNameOverride:    postgres.NewOptionalText(rec.DisplayNameOverride),
		FlatFeeCents:           postgres.NewOptionalInt8(rec.FlatFeeCents),
		ID:                     xid.New().String(),
		IncludedEventsOverride: postgres.NewOptionalInt8(rec.IncludedEventsOverride),
		Note:                   rec.Note,
		OrgID:                  orgID,
		PlanSlug:               postgres.NewOptionalText(rec.PlanSlug),
		RetentionDaysOverride:  postgres.NewOptionalInt8(rec.RetentionDaysOverride),
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

type HistoryEntry struct {
	Actor     string
	ChangedAt time.Time
	Record    Record
}

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
			Present:                row.PlanSlug.Valid,
			AnchorDay:              postgres.Int2ToInt(row.AnchorDay),
			BlockRateCents:         row.BlockRateCents.Int64,
			ContractEndsAt:         row.ContractEndsAt.Time,
			DisplayNameOverride:    row.DisplayNameOverride.String,
			FlatFeeCents:           row.FlatFeeCents.Int64,
			IncludedEventsOverride: row.IncludedEventsOverride.Int64,
			Note:                   row.Note,
			PlanSlug:               row.PlanSlug.String,
			RetentionDaysOverride:  row.RetentionDaysOverride.Int64,
			TrialEndsAt:            row.TrialEndsAt.Time,
		}
		out = append(out, HistoryEntry{Actor: row.Actor, ChangedAt: row.ChangedAt.Time, Record: rec})
	}
	return out, nil
}

func isOrgFKViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

// Preview prices a number of events on what the org would be charged today. It
// resolves as if the switch were on: the CLI is ungated so a deal can be checked
// before billing is turned on, and the free floor has nothing to price.
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
	ent := Resolve(row.OrgCreateTime.Time, recordFromRow(row), nil, now, true)
	if ent.Pricing().IsZero() {
		return ent, Quote{}, fmt.Errorf("%w: %s", ErrPlanNotFound, ent.Slug)
	}
	return ent, ent.Pricing().Quote(events), nil
}
