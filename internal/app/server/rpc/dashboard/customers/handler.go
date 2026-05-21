package customers

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"github.com/pug-sh/pug/internal/app/server/rpc"
	"github.com/pug-sh/pug/internal/apperr"
	corecustomers "github.com/pug-sh/pug/internal/core/customers"
	customersv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/customers/v1"
	"google.golang.org/protobuf/proto"
)

type server struct {
	customers *corecustomers.Service
}

func NewServer(customersSvc *corecustomers.Service) *server { return &server{customers: customersSvc} }

func (s *server) GetMe(
	ctx context.Context,
	_ *connect.Request[customersv1.GetMeRequest],
) (*connect.Response[customersv1.GetMeResponse], error) {
	principal, err := rpc.MustGetPrincipalWithCustomer(ctx)
	if err != nil {
		return nil, err // already an apperr from the extractor
	}
	return connect.NewResponse(&customersv1.GetMeResponse{
		CustomerId:    proto.String(principal.Customer.ID),
		Email:         proto.String(principal.Customer.Email),
		EmailVerified: proto.Bool(principal.Customer.EmailVerifiedAt.Valid),
	}), nil
}

func (s *server) SetPassword(
	ctx context.Context,
	req *connect.Request[customersv1.SetPasswordRequest],
) (*connect.Response[customersv1.SetPasswordResponse], error) {
	principal, err := rpc.MustGetPrincipalWithCustomer(ctx)
	if err != nil {
		return nil, err // already an apperr from the extractor
	}
	if err := s.customers.SetPassword(ctx, principal.Customer.ID, req.Msg.GetPassword()); err != nil {
		if errors.Is(err, corecustomers.ErrPasswordTooLong) {
			return nil, apperr.Invalid(apperr.ReasonPasswordTooLong, "password must be 72 bytes or fewer")
		}
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	return connect.NewResponse(&customersv1.SetPasswordResponse{}), nil
}
