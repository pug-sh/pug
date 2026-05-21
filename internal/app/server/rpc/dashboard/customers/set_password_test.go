package customers

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	"github.com/pug-sh/pug/internal/app/server/rpc"
	corecustomers "github.com/pug-sh/pug/internal/core/customers"
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
