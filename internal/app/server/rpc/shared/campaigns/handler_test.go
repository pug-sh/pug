package campaigns

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/authn"
	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/pug-sh/pug/internal/app/server/rpc"
	"github.com/pug-sh/pug/internal/apperr"
	campaignsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/campaigns/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
)

// fakeCampaignService satisfies campaignService for handler unit tests.
type fakeCampaignService struct{ getErr error }

func (f fakeCampaignService) CreateCampaign(context.Context, dbwrite.CreateCampaignParams) (dbwrite.Campaign, error) {
	return dbwrite.Campaign{}, nil
}
func (f fakeCampaignService) GetCampaignByIDAndProjectID(context.Context, string, string) (dbread.Campaign, error) {
	return dbread.Campaign{}, f.getErr
}
func (f fakeCampaignService) GetCampaignsByProjectID(context.Context, string) ([]dbread.Campaign, error) {
	return nil, nil
}
func (f fakeCampaignService) DeleteCampaign(context.Context, string, string) error { return nil }
func (f fakeCampaignService) UpdateCampaign(context.Context, dbwrite.UpdateCampaignParams) (dbwrite.Campaign, error) {
	return dbwrite.Campaign{}, nil
}

func TestGet_NotFound(t *testing.T) {
	s := &server{service: fakeCampaignService{getErr: pgx.ErrNoRows}}
	req := connect.NewRequest(&campaignsv1.GetRequest{}) // id unused: the fake returns ErrNoRows regardless
	_, err := s.Get(ctxWithProject(context.Background()), req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var ae *apperr.Error
	if !errors.As(err, &ae) {
		t.Fatalf("want *apperr.Error, got %T: %v", err, err)
	}
	if ae.Code() != connect.CodeNotFound {
		t.Errorf("want CodeNotFound, got %v", ae.Code())
	}
	if ae.Reason() != apperr.ReasonCampaignNotFound {
		t.Errorf("want reason %q, got %q", apperr.ReasonCampaignNotFound, ae.Reason())
	}
}

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
	if ae.Code() != connect.CodeInvalidArgument {
		t.Errorf("want CodeInvalidArgument, got %v", ae.Code())
	}
	if ae.Reason() != apperr.ReasonInvalidNotificationData {
		t.Errorf("want reason %q, got %q", apperr.ReasonInvalidNotificationData, ae.Reason())
	}
}
