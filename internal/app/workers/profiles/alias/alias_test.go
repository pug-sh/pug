package alias

import (
	"context"
	"testing"

	natsworker "github.com/fivebitsio/cotton/internal/deps/nats"
	workerprofilesv1 "github.com/fivebitsio/cotton/internal/gen/proto/workers/profiles/v1"
	"google.golang.org/protobuf/proto"
)

func TestHandleAlias_ValidationRejectsMissingFields(t *testing.T) {
	tests := []struct {
		name string
		msg  *workerprofilesv1.ProfileAliasMessage
	}{
		{
			name: "empty alias_id",
			msg: &workerprofilesv1.ProfileAliasMessage{
				ProfileId:  proto.String("p1"),
				ExternalId: proto.String("e1"),
				ProjectId:  proto.String("proj1"),
			},
		},
		{
			name: "empty profile_id",
			msg: &workerprofilesv1.ProfileAliasMessage{
				AliasId:    proto.String("a1"),
				ExternalId: proto.String("e1"),
				ProjectId:  proto.String("proj1"),
			},
		},
		{
			name: "empty external_id",
			msg: &workerprofilesv1.ProfileAliasMessage{
				AliasId:   proto.String("a1"),
				ProfileId: proto.String("p1"),
				ProjectId: proto.String("proj1"),
			},
		},
		{
			name: "empty project_id",
			msg: &workerprofilesv1.ProfileAliasMessage{
				AliasId:    proto.String("a1"),
				ProfileId:  proto.String("p1"),
				ExternalId: proto.String("e1"),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data, err := proto.Marshal(tc.msg)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}

			err = handleAlias(context.Background(), nil, data)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !natsworker.IsPermanentError(err) {
				t.Errorf("expected PermanentError, got %T: %v", err, err)
			}
		})
	}
}

func TestHandleAlias_ValidationRejectsCorruptData(t *testing.T) {
	err := handleAlias(context.Background(), nil, []byte("not-proto"))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !natsworker.IsPermanentError(err) {
		t.Errorf("expected PermanentError, got %T: %v", err, err)
	}
}
