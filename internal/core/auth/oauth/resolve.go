package oauth

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/slogx"
	"github.com/rs/xid"
)

const (
	CustomersEmailLowerIdx                  = "customers_email_lower_idx"
	CustomerIdentitiesProviderSubjectUnique = "customer_identities_provider_subject_key"
)

type resolveResult struct {
	CustomerID string
	CreatedNew bool
}

// FinalizeFunc runs in the same transaction as identity resolution (org provisioning, verify, etc.).
type FinalizeFunc func(ctx context.Context, w *dbwrite.Queries, customerID string, createdNew bool) error

// WithIdentityTx finds-or-creates the customer for a verified identity and runs
// finalize in the same transaction as identity resolution on the common path. On
// a concurrent-signup or provider-subject race it retries link + finalize in a
// fresh transaction (so finalize may run in a second transaction, but never
// partially). The Identity type guarantees a verified, non-empty email, so there
// is no email re-check here.
func WithIdentityTx(ctx context.Context, pool *pgxpool.Pool, provider ProviderName, ident *Identity, finalize FinalizeFunc) (customerID string, createdNew bool, err error) {
	result, err := resolveAndFinalizeInTx(ctx, pool, provider, ident, finalize)
	if err == nil {
		return result.CustomerID, result.CreatedNew, nil
	}
	if !IsUniqueViolationOn(err, CustomersEmailLowerIdx) {
		return "", false, err
	}

	// Concurrent signup: email row was created in another tx — link and finalize in a fresh tx.
	linked, linkErr := linkByEmailAndFinalize(ctx, pool, provider, ident, finalize)
	if linkErr != nil {
		return "", false, linkErr
	}
	return linked.CustomerID, linked.CreatedNew, nil
}

func resolveAndFinalizeInTx(ctx context.Context, pool *pgxpool.Pool, provider ProviderName, ident *Identity, finalize FinalizeFunc) (resolveResult, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed to begin oauth identity tx", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return resolveResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	r := dbread.New(tx)
	w := dbwrite.New(tx)

	result, err := resolveIdentityWithQueries(ctx, r, w, provider, ident)
	if err != nil {
		if IsUniqueViolationOn(err, CustomerIdentitiesProviderSubjectUnique) {
			if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
				slog.ErrorContext(ctx, "failed rolling back identity tx after provider-subject race", slogx.Error(rbErr))
			}
			lookedUp, lookupErr := lookupByProviderSubject(ctx, pool, provider, ident.Subject())
			if lookupErr != nil {
				return resolveResult{}, lookupErr
			}
			return finalizeExistingCustomer(ctx, pool, lookedUp.CustomerID, finalize)
		}
		return resolveResult{}, err
	}

	if finalize != nil {
		// finalize errors are recorded by finalize itself (coreorgs at its detect
		// site; the oauth callback for FinalizeVerifiedCustomer), so return bare.
		if err := finalize(ctx, w, result.CustomerID, result.CreatedNew); err != nil {
			return resolveResult{}, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		slog.ErrorContext(ctx, "failed to commit oauth identity tx", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return resolveResult{}, err
	}
	return result, nil
}

func finalizeExistingCustomer(ctx context.Context, pool *pgxpool.Pool, customerID string, finalize FinalizeFunc) (resolveResult, error) {
	if finalize == nil {
		return resolveResult{CustomerID: customerID, CreatedNew: false}, nil
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed to begin oauth finalize tx", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return resolveResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	w := dbwrite.New(tx)
	if err := finalize(ctx, w, customerID, false); err != nil {
		return resolveResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		slog.ErrorContext(ctx, "failed to commit oauth finalize tx", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return resolveResult{}, err
	}
	return resolveResult{CustomerID: customerID, CreatedNew: false}, nil
}

func linkByEmailAndFinalize(ctx context.Context, pool *pgxpool.Pool, provider ProviderName, ident *Identity, finalize FinalizeFunc) (resolveResult, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed to begin oauth link tx", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return resolveResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	r := dbread.New(tx)
	w := dbwrite.New(tx)

	customer, err := r.GetCustomerByEmail(ctx, ident.Email())
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			lookedUp, lookupErr := lookupByProviderSubject(ctx, pool, provider, ident.Subject())
			if lookupErr != nil {
				return resolveResult{}, lookupErr
			}
			return finalizeExistingCustomer(ctx, pool, lookedUp.CustomerID, finalize)
		}
		slog.ErrorContext(ctx, "failed to look up customer for oauth link retry", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return resolveResult{}, err
	}

	if err := createIdentity(ctx, w, customer.ID, provider, ident.Subject()); err != nil {
		if IsUniqueViolationOn(err, CustomerIdentitiesProviderSubjectUnique) {
			if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
				slog.ErrorContext(ctx, "failed rolling back oauth link retry tx", slogx.Error(rbErr))
			}
			lookedUp, lookupErr := lookupByProviderSubject(ctx, pool, provider, ident.Subject())
			if lookupErr != nil {
				return resolveResult{}, lookupErr
			}
			return finalizeExistingCustomer(ctx, pool, lookedUp.CustomerID, finalize)
		}
		return resolveResult{}, err
	}

	if finalize != nil {
		if err := finalize(ctx, w, customer.ID, false); err != nil {
			return resolveResult{}, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		slog.ErrorContext(ctx, "failed to commit oauth link tx", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return resolveResult{}, err
	}
	return resolveResult{CustomerID: customer.ID, CreatedNew: false}, nil
}

func lookupByProviderSubject(ctx context.Context, pool *pgxpool.Pool, provider ProviderName, subject string) (resolveResult, error) {
	r := dbread.New(pool)
	row, err := r.GetCustomerIdentityByProviderSubject(ctx, dbread.GetCustomerIdentityByProviderSubjectParams{
		Provider:        string(provider),
		ProviderSubject: subject,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The race "winner" should have written the identity row; its absence
			// is a genuine integrity anomaly, not an expected branch — record it.
			slog.ErrorContext(ctx, "oauth identity race lost with no linked row",
				slog.String("provider", string(provider)), slog.String("subject", subject))
			telemetry.RecordError(ctx, ErrIdentityResolutionFailed)
			return resolveResult{}, ErrIdentityResolutionFailed
		}
		slog.ErrorContext(ctx, "failed to look up customer identity after race", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return resolveResult{}, err
	}
	return resolveResult{CustomerID: row.CustomerID, CreatedNew: false}, nil
}

func resolveIdentityWithQueries(ctx context.Context, r *dbread.Queries, w *dbwrite.Queries, provider ProviderName, ident *Identity) (resolveResult, error) {
	if row, err := r.GetCustomerIdentityByProviderSubject(ctx, dbread.GetCustomerIdentityByProviderSubjectParams{
		Provider:        string(provider),
		ProviderSubject: ident.Subject(),
	}); err == nil {
		return resolveResult{CustomerID: row.CustomerID, CreatedNew: false}, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		slog.ErrorContext(ctx, "failed to look up oauth identity", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return resolveResult{}, err
	}

	if customer, err := r.GetCustomerByEmail(ctx, ident.Email()); err == nil {
		if err := createIdentity(ctx, w, customer.ID, provider, ident.Subject()); err != nil {
			return resolveResult{}, err
		}
		return resolveResult{CustomerID: customer.ID, CreatedNew: false}, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		slog.ErrorContext(ctx, "failed to look up customer by email for oauth", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return resolveResult{}, err
	}

	customerID := xid.New().String()
	if _, err := w.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID:           customerID,
		Email:        ident.Email(),
		DisplayName:  ident.DisplayName(),
		PictureUri:   ident.PictureURI(),
		PasswordHash: "",
	}); err != nil {
		slog.ErrorContext(ctx, "failed to create oauth customer", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return resolveResult{}, err
	}
	if err := createIdentity(ctx, w, customerID, provider, ident.Subject()); err != nil {
		return resolveResult{}, err
	}
	return resolveResult{CustomerID: customerID, CreatedNew: true}, nil
}

// createIdentity inserts the (provider, subject) → customer link. A unique
// violation on CustomerIdentitiesProviderSubjectUnique is an EXPECTED outcome of
// a concurrent-signup race that callers recover from by re-looking-up the winner,
// so it is returned unlogged; only genuinely unexpected failures are recorded.
func createIdentity(ctx context.Context, w *dbwrite.Queries, customerID string, provider ProviderName, subject string) error {
	_, err := w.CreateCustomerIdentity(ctx, dbwrite.CreateCustomerIdentityParams{
		ID:              xid.New().String(),
		CustomerID:      customerID,
		Provider:        string(provider),
		ProviderSubject: subject,
	})
	if err != nil && !IsUniqueViolationOn(err, CustomerIdentitiesProviderSubjectUnique) {
		slog.ErrorContext(ctx, "failed to create customer identity", slogx.Error(err))
		telemetry.RecordError(ctx, err)
	}
	return err
}

// IsUniqueViolationOn reports whether err is a Postgres unique violation on constraint.
func IsUniqueViolationOn(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == pgerrcode.UniqueViolation && pgErr.ConstraintName == constraint
}
