package projects

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestTranslateUniqueViolation(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		wantSet bool // true => expect ErrDashboardTileDisplayNameConflict, false => expect nil
	}{
		{
			name:    "nil error",
			err:     nil,
			wantSet: false,
		},
		{
			name:    "plain non-pgconn error",
			err:     errors.New("boom"),
			wantSet: false,
		},
		{
			name:    "pgconn check-violation (different code)",
			err:     &pgconn.PgError{Code: pgerrcode.CheckViolation},
			wantSet: false,
		},
		{
			name:    "pgconn foreign-key-violation (different code)",
			err:     &pgconn.PgError{Code: pgerrcode.ForeignKeyViolation},
			wantSet: false,
		},
		{
			name:    "pgconn unique-violation (the only match)",
			err:     &pgconn.PgError{Code: pgerrcode.UniqueViolation},
			wantSet: true,
		},
		{
			name:    "wrapped pgconn unique-violation",
			err:     fmt.Errorf("write tile: %w", &pgconn.PgError{Code: pgerrcode.UniqueViolation}),
			wantSet: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := translateUniqueViolation(tc.err)
			if tc.wantSet {
				if !errors.Is(got, ErrDashboardTileDisplayNameConflict) {
					t.Errorf("translateUniqueViolation(%v) = %v, want ErrDashboardTileDisplayNameConflict", tc.err, got)
				}
			} else {
				if got != nil {
					t.Errorf("translateUniqueViolation(%v) = %v, want nil", tc.err, got)
				}
			}
		})
	}
}
