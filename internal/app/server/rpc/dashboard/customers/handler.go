package customers

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"github.com/pug-sh/pug/internal/app/server/rpc"
	customersv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/customers/v1"
	"google.golang.org/protobuf/proto"
)

type server struct{}

func NewServer() *server { return &server{} }

func (s *server) GetMe(
	ctx context.Context,
	_ *connect.Request[customersv1.GetMeRequest],
) (*connect.Response[customersv1.GetMeResponse], error) {
	principal, err := rpc.MustGetPrincipalWithCustomer(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
	}
	return connect.NewResponse(&customersv1.GetMeResponse{
		CustomerId:    proto.String(principal.Customer.ID),
		Email:         proto.String(principal.Customer.Email),
		EmailVerified: proto.Bool(principal.Customer.EmailVerifiedAt.Valid),
	}), nil
}
