package campaigns

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/authn"
	"connectrpc.com/connect"
	"github.com/pug-sh/pug/internal/app/server/rpc"
	"github.com/pug-sh/pug/internal/apperr"
	campaignsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/campaigns/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
)

func ctxWithProject(ctx context.Context) context.Context {
	return authn.SetInfo(ctx, &rpc.Principal{
		AuthType: rpc.AuthTypePrivateKey,
		Project:  &dbread.Project{},
	})
}

func TestCreate_InvalidNotificationData(t *testing.T) {
	s := &server{}
	req := connect.NewRequest(&campaignsv1.CreateRequest{
		NotificationData: []byte(`not-valid-json{`),
	})
	ctx := ctxWithProject(context.Background())
	_, err := s.Create(ctx, req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var ae *apperr.Error
	if !errors.As(err, &ae) {
		t.Fatalf("want *apperr.Error, got %T: %v", err, err)
	}
	if ae.Code != connect.CodeInvalidArgument {
		t.Errorf("want CodeInvalidArgument, got %v", ae.Code)
	}
	if ae.Reason != apperr.ReasonInvalidNotificationData {
		t.Errorf("want reason %q, got %q", apperr.ReasonInvalidNotificationData, ae.Reason)
	}
}
