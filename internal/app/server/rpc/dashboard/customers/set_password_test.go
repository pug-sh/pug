package customers

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"
	"github.com/pug-sh/pug/internal/app/server/rpc"
	"github.com/pug-sh/pug/internal/apperr"
	corecustomers "github.com/pug-sh/pug/internal/core/customers"
	coreorgs "github.com/pug-sh/pug/internal/core/orgs"
	customersv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/customers/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
	"google.golang.org/protobuf/proto"
)

func TestSetPasswordHandler(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	read := dbread.New(db.PgRO)
	ctx := context.Background()

	if _, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{ID: "cust-h-setpw", Email: "h-setpw@example.com", DisplayName: "", PasswordHash: "", PictureUri: ""}); err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}
	custRow, err := read.GetCustomerByID(ctx, "cust-h-setpw")
	if err != nil {
		t.Fatalf("GetCustomerByID: %v", err)
	}

	srv := NewServer(corecustomers.NewService(db.PgW))
	authedCtx := ctxWithCustomer(&rpc.Principal{Customer: &custRow})
	if _, err := srv.SetPassword(authedCtx, connect.NewRequest(&customersv1.SetPasswordRequest{Password: proto.String("a-new-password")})); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	updated, err := read.GetCustomerByID(ctx, "cust-h-setpw")
	if err != nil {
		t.Fatalf("GetCustomerByID after: %v", err)
	}
	if updated.PasswordHash == "" {
		t.Fatal("expected password_hash to be set after SetPassword")
	}

	// No principal → Unauthenticated.
	_, err = srv.SetPassword(context.Background(), connect.NewRequest(&customersv1.SetPasswordRequest{Password: proto.String("x")}))
	wantUnauthenticated(t, err)
}

func TestSetPasswordHandlerSSORequired(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	ctx := context.Background()
	orgs := coreorgs.NewService(db.PgRO, db.PgW, nil)

	for _, c := range []dbwrite.CreateCustomerParams{{ID: "cust-h-admin", Email: "admin@acme.com"}, {ID: "cust-h-bob", Email: "bob@acme.com"}} {
		if _, err := write.CreateCustomer(ctx, c); err != nil {
			t.Fatalf("CreateCustomer: %v", err)
		}
	}
	org, err := orgs.CreateOrgWithDefaults(ctx, "cust-h-admin", "acme")
	if err != nil {
		t.Fatal(err)
	}
	d, err := orgs.VerifyDomainByOperator(ctx, org.ID, "acme.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := coreorgs.MarkSSOSeenInTx(ctx, write, "acme.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := orgs.UpdateDomain(ctx, org.ID, d.ID, true); err != nil {
		t.Fatal(err)
	}
	bob, err := dbread.New(db.PgRO).GetCustomerByID(ctx, "cust-h-bob")
	if err != nil {
		t.Fatal(err)
	}

	srv := NewServer(corecustomers.NewService(db.PgW))
	_, err = srv.SetPassword(ctxWithCustomer(&rpc.Principal{Customer: &bob}), connect.NewRequest(&customersv1.SetPasswordRequest{Password: proto.String("a-new-password")}))
	ae, ok := errors.AsType[*apperr.Error](err)
	if !ok || ae.Code() != connect.CodeFailedPrecondition || ae.Reason() != apperr.ReasonSSORequired {
		t.Fatalf("err = %v, want FailedPrecondition / SSO_REQUIRED", err)
	}
}
