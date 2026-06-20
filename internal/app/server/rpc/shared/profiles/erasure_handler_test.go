package profiles

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pug-sh/pug/internal/apperr"
	coreprofiles "github.com/pug-sh/pug/internal/core/profiles"
	natsdeps "github.com/pug-sh/pug/internal/deps/nats"
	profilesv1 "github.com/pug-sh/pug/internal/gen/proto/shared/profiles/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/testutil"
	"google.golang.org/protobuf/proto"
)

func TestDeleteDataSubject_Unauthenticated(t *testing.T) {
	s := NewServer(coreprofiles.NewService(nil, nil, &natsdeps.NATSClient{}))
	_, err := s.DeleteDataSubject(context.Background(),
		connect.NewRequest(&profilesv1.DeleteDataSubjectRequest{ExternalId: proto.String("ext-1")}))
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code() != connect.CodeUnauthenticated {
		t.Fatalf("want unauthenticated apperr, got %v (%T)", err, err)
	}
}

func TestGetDeletionRequest_Unauthenticated(t *testing.T) {
	s := NewServer(coreprofiles.NewService(nil, nil, &natsdeps.NATSClient{}))
	_, err := s.GetDeletionRequest(context.Background(),
		connect.NewRequest(&profilesv1.GetDeletionRequestRequest{RequestId: proto.String("req-1")}))
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code() != connect.CodeUnauthenticated {
		t.Fatalf("want unauthenticated apperr, got %v (%T)", err, err)
	}
}

// toDeletionRequestResponse is the controller's proof-of-fulfilment payload, so
// every field — including the FAILED status enum and the error reason — must map.
func TestToDeletionRequestResponse_MapsAllFields(t *testing.T) {
	requestedAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	completedAt := time.Date(2026, 1, 2, 3, 5, 0, 0, time.UTC)
	dr := dbread.ComplianceRequest{
		ID:             "req-1",
		Status:         string(coreprofiles.ComplianceStatusFailed),
		EventsAffected: 42,
		RequestedAt:    pgtype.Timestamptz{Time: requestedAt, Valid: true},
		CompletedAt:    pgtype.Timestamptz{Time: completedAt, Valid: true},
		ExternalID:     pgtype.Text{String: "ext-1", Valid: true},
		ProfileID:      pgtype.Text{String: "prof-1", Valid: true},
		Error:          pgtype.Text{String: "boom", Valid: true},
	}

	resp := toDeletionRequestResponse(dr)
	if resp.GetRequestId() != "req-1" {
		t.Errorf("request_id = %q, want req-1", resp.GetRequestId())
	}
	if resp.GetStatus() != profilesv1.ComplianceRequestStatus_COMPLIANCE_REQUEST_STATUS_FAILED {
		t.Errorf("status = %v, want FAILED", resp.GetStatus())
	}
	if resp.GetEventsIdentified() != 42 {
		t.Errorf("events_identified = %d, want 42", resp.GetEventsIdentified())
	}
	if resp.GetExternalId() != "ext-1" {
		t.Errorf("external_id = %q, want ext-1", resp.GetExternalId())
	}
	if resp.GetProfileId() != "prof-1" {
		t.Errorf("profile_id = %q, want prof-1", resp.GetProfileId())
	}
	if resp.GetError() != "boom" {
		t.Errorf("error = %q, want boom", resp.GetError())
	}
	if !resp.GetRequestedAt().AsTime().Equal(requestedAt) {
		t.Errorf("requested_at = %v, want %v", resp.GetRequestedAt().AsTime(), requestedAt)
	}
	if !resp.GetCompletedAt().AsTime().Equal(completedAt) {
		t.Errorf("completed_at = %v, want %v", resp.GetCompletedAt().AsTime(), completedAt)
	}
}

// NULL optional columns must stay unset on the wire (not zero-valued), and a
// pending request carries no completion timestamp or error.
func TestToDeletionRequestResponse_OmitsNullOptionalFields(t *testing.T) {
	dr := dbread.ComplianceRequest{
		ID:          "req-2",
		Status:      string(coreprofiles.ComplianceStatusPending),
		RequestedAt: pgtype.Timestamptz{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true},
		// ExternalID, ProfileID, CompletedAt, Error all NULL.
	}

	resp := toDeletionRequestResponse(dr)
	if resp.GetStatus() != profilesv1.ComplianceRequestStatus_COMPLIANCE_REQUEST_STATUS_PENDING {
		t.Errorf("status = %v, want PENDING", resp.GetStatus())
	}
	if resp.ExternalId != nil {
		t.Errorf("external_id = %v, want unset", resp.ExternalId)
	}
	if resp.ProfileId != nil {
		t.Errorf("profile_id = %v, want unset", resp.ProfileId)
	}
	if resp.CompletedAt != nil {
		t.Errorf("completed_at = %v, want unset", resp.CompletedAt)
	}
	if resp.Error != nil {
		t.Errorf("error = %v, want unset", resp.Error)
	}
	if resp.GetEventsIdentified() != 0 {
		t.Errorf("events_identified = %d, want 0", resp.GetEventsIdentified())
	}
}

func TestDeleteDataSubject_RecordsRequestAndIsReadable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	pg := testutil.SetupPostgres(t)
	tn := testutil.SetupNATS(t)
	t.Setenv("NATS_URL", tn.URL)

	natsClient, err := natsdeps.New(ctx)
	if err != nil {
		t.Fatalf("create nats client: %v", err)
	}
	defer natsClient.Close()

	projectID := seedProject(t, ctx, pg)
	s := NewServer(coreprofiles.NewService(pg.PgW, nil, natsClient))

	// Erase by external_id with no profile row — must still record the request.
	const externalID = "ghost@example.com"
	resp, err := s.DeleteDataSubject(authCtx(projectID),
		connect.NewRequest(&profilesv1.DeleteDataSubjectRequest{ExternalId: proto.String(externalID)}))
	if err != nil {
		t.Fatalf("DeleteDataSubject: %v", err)
	}
	if resp.Msg.GetRequestId() == "" {
		t.Fatal("request_id is empty")
	}
	if resp.Msg.GetStatus() != profilesv1.ComplianceRequestStatus_COMPLIANCE_REQUEST_STATUS_PENDING {
		t.Errorf("status = %v, want PENDING", resp.Msg.GetStatus())
	}

	// GetDeletionRequest returns the recorded audit row (the DSAR proof).
	got, err := s.GetDeletionRequest(authCtx(projectID),
		connect.NewRequest(&profilesv1.GetDeletionRequestRequest{RequestId: proto.String(resp.Msg.GetRequestId())}))
	if err != nil {
		t.Fatalf("GetDeletionRequest: %v", err)
	}
	if got.Msg.GetExternalId() != externalID {
		t.Errorf("external_id = %q, want %q", got.Msg.GetExternalId(), externalID)
	}
	if got.Msg.GetStatus() != profilesv1.ComplianceRequestStatus_COMPLIANCE_REQUEST_STATUS_PENDING {
		t.Errorf("status = %v, want PENDING", got.Msg.GetStatus())
	}
}

func TestGetDeletionRequest_NotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	pg := testutil.SetupPostgres(t)
	tn := testutil.SetupNATS(t)
	t.Setenv("NATS_URL", tn.URL)

	natsClient, err := natsdeps.New(ctx)
	if err != nil {
		t.Fatalf("create nats client: %v", err)
	}
	defer natsClient.Close()

	projectID := seedProject(t, ctx, pg)
	s := NewServer(coreprofiles.NewService(pg.PgW, nil, natsClient))

	_, err = s.GetDeletionRequest(authCtx(projectID),
		connect.NewRequest(&profilesv1.GetDeletionRequestRequest{RequestId: proto.String("does-not-exist")}))
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code() != connect.CodeNotFound {
		t.Fatalf("want not-found apperr, got %v (%T)", err, err)
	}
	if ae.Reason() != apperr.ReasonDeletionRequestNotFound {
		t.Errorf("reason = %v, want DELETION_REQUEST_NOT_FOUND", ae.Reason())
	}
}
