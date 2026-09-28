package orgs_test

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"
	orgshandler "github.com/pug-sh/pug/internal/app/server/rpc/dashboard/orgs"
	"github.com/pug-sh/pug/internal/apperr"
	"github.com/pug-sh/pug/internal/core/instance"
	orgsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/orgs/v1"
	"google.golang.org/protobuf/proto"
)

func TestManagedModeDeniesOrdinaryCreateRPC(t *testing.T) {
	policy, err := instance.ParsePolicy("managed", "admin@example.com")
	if err != nil {
		t.Fatal(err)
	}
	server := orgshandler.NewServerWithPolicy(nil, policy)
	_, err = server.Create(context.Background(), connect.NewRequest(&orgsv1.CreateRequest{DisplayName: proto.String("bypass")}))
	var appError *apperr.Error
	if !errors.As(err, &appError) || appError.Code() != connect.CodePermissionDenied || appError.Reason() != apperr.ReasonOrgCreationDisabled {
		t.Fatalf("ordinary organization creation should be denied: %v", err)
	}
}
