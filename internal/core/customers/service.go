package customers

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/slogx"
	"golang.org/x/crypto/bcrypt"
)

// ErrPasswordTooLong is returned when a password exceeds bcrypt's 72-byte input
// limit. Proto validation enforces the same bound at the interceptor, so this is
// reached only by direct/non-RPC callers.
var ErrPasswordTooLong = errors.New("password is too long")

type Service struct {
	write *dbwrite.Queries
}

func NewService(pgW *pgxpool.Pool) *Service {
	return &Service{write: dbwrite.New(pgW)}
}

// SetPassword hashes and stores a password for the given customer (used by the
// authenticated dashboard SetPassword RPC so magic-link accounts can gain a
// password). It overwrites any existing hash.
func (s *Service) SetPassword(ctx context.Context, customerID, password string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		if errors.Is(err, bcrypt.ErrPasswordTooLong) {
			return ErrPasswordTooLong
		}
		slog.ErrorContext(ctx, "failed to hash password", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return err
	}
	if _, err := s.write.UpdateCustomerPasswordHash(ctx, dbwrite.UpdateCustomerPasswordHashParams{
		ID:           customerID,
		PasswordHash: string(hash),
	}); err != nil {
		slog.ErrorContext(ctx, "failed to update customer password hash", slogx.Error(err), slog.String("customer_id", customerID))
		telemetry.RecordError(ctx, err)
		return err
	}
	return nil
}
