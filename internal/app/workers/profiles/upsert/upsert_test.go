package upsert

import (
	"context"
	"strings"
	"testing"
	"time"

	natsworker "github.com/pug-sh/pug/internal/deps/nats"
	workerprofilesv1 "github.com/pug-sh/pug/internal/gen/proto/workers/profiles/v1"
	"github.com/pug-sh/pug/internal/testutil"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestHandleUpsert_ValidationRejectsMissingFields(t *testing.T) {
	tests := []struct {
		name string
		msg  *workerprofilesv1.ProfileUpsertMessage
	}{
		{
			name: "empty profile_id",
			msg: &workerprofilesv1.ProfileUpsertMessage{
				ProjectId: proto.String("proj1"),
			},
		},
		{
			name: "empty project_id",
			msg: &workerprofilesv1.ProfileUpsertMessage{
				ProfileId: proto.String("p1"),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data, err := proto.Marshal(tc.msg)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}

			err = handleUpsert(context.Background(), nil, data)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !natsworker.IsPermanentError(err) {
				t.Errorf("expected PermanentError, got %T: %v", err, err)
			}
		})
	}
}

func TestHandleUpsert_ValidationRejectsCorruptData(t *testing.T) {
	err := handleUpsert(context.Background(), nil, []byte("not-proto"))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !natsworker.IsPermanentError(err) {
		t.Errorf("expected PermanentError, got %T: %v", err, err)
	}
}

// TestHandleUpsert_InsertsProfileIntoClickHouse drives the handler against a
// real ClickHouse so the shared ProfilesInsertStmt column list is exercised
// end-to-end (PrepareBatch → Append → Send) against the live profiles schema.
// Any drift between the const and the table would surface here as an insert
// error rather than silently in production.
func TestHandleUpsert_InsertsProfileIntoClickHouse(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ch := testutil.SetupClickHouse(t)
	ctx := context.Background()

	const (
		profileID  = "p1"
		projectID  = "proj1"
		externalID = "user@example.com"
	)
	props, err := structpb.NewStruct(map[string]any{"plan": "pro"})
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)

	data, err := proto.Marshal(&workerprofilesv1.ProfileUpsertMessage{
		ProfileId:  proto.String(profileID),
		ProjectId:  proto.String(projectID),
		ExternalId: proto.String(externalID),
		Properties: props,
		CreateTime: timestamppb.New(now),
		UpdateTime: timestamppb.New(now),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if err := handleUpsert(ctx, ch.Conn, data); err != nil {
		t.Fatalf("handleUpsert: %v", err)
	}

	var (
		gotExternalID string
		gotProps      string
		gotIsDeleted  uint8
	)
	if err := ch.Conn.QueryRow(ctx,
		"SELECT external_id, CAST(properties AS String), is_deleted FROM profiles FINAL WHERE id = ? AND project_id = ?",
		profileID, projectID,
	).Scan(&gotExternalID, &gotProps, &gotIsDeleted); err != nil {
		t.Fatalf("query profile: %v", err)
	}

	if gotExternalID != externalID {
		t.Errorf("external_id = %q, want %q", gotExternalID, externalID)
	}
	if gotIsDeleted != 0 {
		t.Errorf("is_deleted = %d, want 0", gotIsDeleted)
	}
	if !strings.Contains(gotProps, "pro") {
		t.Errorf("properties = %q, want it to contain the plan value", gotProps)
	}
}
