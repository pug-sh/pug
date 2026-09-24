package rpc

import (
	"context"
	"testing"

	"connectrpc.com/authn"
	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pug-sh/pug/internal/core/instance"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
)

func TestInstanceAuthorizationRequiresVerifiedAllowlistedEnabledAccount(t *testing.T) {
	policy, err := instance.ParsePolicy("managed", "ADMIN@example.com")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, email        string
		verified, disabled bool
		want               connect.Code
	}{
		{"verified allowlisted", "admin@example.com", true, false, 0},
		{"unverified allowlisted", "admin@example.com", false, false, connect.CodePermissionDenied},
		{"disabled allowlisted", "admin@example.com", true, true, connect.CodePermissionDenied},
		{"other verified", "other@example.com", true, false, connect.CodePermissionDenied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			customer := &dbread.Customer{Email: tc.email, EmailVerifiedAt: pgtype.Timestamptz{Valid: tc.verified}, DisabledAt: pgtype.Timestamptz{Valid: tc.disabled}}
			ctx := authn.SetInfo(context.Background(), &Principal{AuthType: AuthTypeJWT, Customer: customer})
			err := authorizeInstanceGated(ctx, policy)
			if tc.want == 0 {
				if err != nil {
					t.Fatalf("expected access: %v", err)
				}
				return
			}
			if got := apperrCode(err); got != tc.want {
				t.Fatalf("authorization code=%v, want %v (err=%v)", got, tc.want, err)
			}
		})
	}
	if got := apperrCode(authorizeInstanceGated(context.Background(), policy)); got != connect.CodeUnauthenticated {
		t.Fatalf("no principal code=%v", got)
	}
}
