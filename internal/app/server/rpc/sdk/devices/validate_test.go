package devices_test

import (
	"testing"

	"buf.build/go/protovalidate"
	"google.golang.org/protobuf/proto"

	devicesv1 "github.com/fivebitsio/cotton/internal/gen/proto/sdk/devices/v1"
)

func TestSubscribeRequest_PlatformRequired(t *testing.T) {
	req := &devicesv1.SubscribeRequest{
		DeviceId: proto.String("device-1"),
		Token:    proto.String("token-abc"),
		// platform intentionally omitted
	}
	if err := protovalidate.Validate(req); err == nil {
		t.Error("expected validation error for missing platform, got nil")
	}
}

func TestSubscribeRequest_PlatformValid(t *testing.T) {
	req := &devicesv1.SubscribeRequest{
		DeviceId: proto.String("device-1"),
		Token:    proto.String("token-abc"),
		Platform: proto.String("ios"),
	}
	if err := protovalidate.Validate(req); err != nil {
		t.Errorf("expected valid, got error: %v", err)
	}
}

func TestUpdateStatusRequest_StatusRequired(t *testing.T) {
	req := &devicesv1.UpdateStatusRequest{
		Id: proto.String("device-1"),
		// status intentionally omitted
	}
	if err := protovalidate.Validate(req); err == nil {
		t.Error("expected validation error for missing status, got nil")
	}
}

func TestUpdateStatusRequest_StatusValid(t *testing.T) {
	req := &devicesv1.UpdateStatusRequest{
		Id:     proto.String("device-1"),
		Status: proto.String("active"),
	}
	if err := protovalidate.Validate(req); err != nil {
		t.Errorf("expected valid, got error: %v", err)
	}
}
