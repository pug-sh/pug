package customers

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/authn"
	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pug-sh/pug/internal/app/server/rpc"
	"github.com/pug-sh/pug/internal/apperr"
	customersv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/customers/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
)

// wantUnauthenticated asserts err is the *apperr.Error CodeUnauthenticated that
// MustGetPrincipalWithCustomer returns. Handler-direct calls surface the raw
// apperr (the wire-level *connect.Error is only built by the interceptor), so
// connect.CodeOf would not see the code.
func wantUnauthenticated(t *testing.T, err error) {
	t.Helper()
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code() != connect.CodeUnauthenticated {
		t.Errorf("err = %v (%T), want apperr CodeUnauthenticated", err, err)
	}
}

// ctxWithCustomer injects a *rpc.Principal into context using the same
// authn.SetInfo mechanism that getPrincipalFromContext reads via authn.GetInfo.
func ctxWithCustomer(p *rpc.Principal) context.Context {
	return authn.SetInfo(context.Background(), p)
}

func TestGetMe(t *testing.T) {
	ctx := ctxWithCustomer(&rpc.Principal{
		Customer: &dbread.Customer{
			ID:              "cust_123",
			Email:           "me@example.com",
			EmailVerifiedAt: pgtype.Timestamptz{Valid: true},
		},
	})

	resp, err := NewServer(nil).GetMe(ctx, connect.NewRequest(&customersv1.GetMeRequest{}))
	if err != nil {
		t.Fatalf("GetMe: %v", err)
	}
	if resp.Msg.GetCustomerId() != "cust_123" || resp.Msg.GetEmail() != "me@example.com" || !resp.Msg.GetEmailVerified() {
		t.Errorf("GetMe = %+v", resp.Msg)
	}

	// Unverified email → email_verified=false.
	ctx2 := ctxWithCustomer(&rpc.Principal{Customer: &dbread.Customer{ID: "c2", Email: "u@e.com", EmailVerifiedAt: pgtype.Timestamptz{Valid: false}}})
	resp2, err := NewServer(nil).GetMe(ctx2, connect.NewRequest(&customersv1.GetMeRequest{}))
	if err != nil {
		t.Fatalf("GetMe (unverified): %v", err)
	}
	if resp2.Msg.GetEmailVerified() {
		t.Error("expected email_verified=false for unverified customer")
	}

	// No principal → Unauthenticated.
	_, err = NewServer(nil).GetMe(context.Background(), connect.NewRequest(&customersv1.GetMeRequest{}))
	wantUnauthenticated(t, err)

	// Principal present but Customer nil (e.g. an API-key path) → Unauthenticated.
	ctxNilCust := ctxWithCustomer(&rpc.Principal{Customer: nil})
	_, err = NewServer(nil).GetMe(ctxNilCust, connect.NewRequest(&customersv1.GetMeRequest{}))
	wantUnauthenticated(t, err)
}
