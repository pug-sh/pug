package customers

import (
	"context"
	"testing"

	"connectrpc.com/authn"
	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pug-sh/pug/internal/app/server/rpc"
	customersv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/customers/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
)

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

	resp, err := NewServer().GetMe(ctx, connect.NewRequest(&customersv1.GetMeRequest{}))
	if err != nil {
		t.Fatalf("GetMe: %v", err)
	}
	if resp.Msg.GetCustomerId() != "cust_123" || resp.Msg.GetEmail() != "me@example.com" || !resp.Msg.GetEmailVerified() {
		t.Errorf("GetMe = %+v", resp.Msg)
	}

	// Unverified email → email_verified=false.
	ctx2 := ctxWithCustomer(&rpc.Principal{Customer: &dbread.Customer{ID: "c2", Email: "u@e.com", EmailVerifiedAt: pgtype.Timestamptz{Valid: false}}})
	resp2, err := NewServer().GetMe(ctx2, connect.NewRequest(&customersv1.GetMeRequest{}))
	if err != nil {
		t.Fatalf("GetMe (unverified): %v", err)
	}
	if resp2.Msg.GetEmailVerified() {
		t.Error("expected email_verified=false for unverified customer")
	}

	// No principal → CodeUnauthenticated.
	if _, err := NewServer().GetMe(context.Background(), connect.NewRequest(&customersv1.GetMeRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("no-principal code = %v, want Unauthenticated", connect.CodeOf(err))
	}
}
