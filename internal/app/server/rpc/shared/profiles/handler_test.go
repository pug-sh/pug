package profiles

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	natsdeps "github.com/fivebitsio/cotton/internal/deps/nats"
	profilesv1 "github.com/fivebitsio/cotton/internal/gen/proto/shared/profiles/v1"
)

func TestNewServer_NilNATSPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for nil nats, got none")
		}
	}()
	NewServer(nil, nil, nil)
}

func TestNewServer_NonNilNATS(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("unexpected panic: %v", r)
		}
	}()
	// Provide a non-nil NATSClient; pgRO/pgW can be nil since we won't call DB methods.
	NewServer(nil, nil, &natsdeps.NATSClient{})
}

func TestDelete_Unauthenticated(t *testing.T) {
	s := NewServer(nil, nil, &natsdeps.NATSClient{})
	_, err := s.Delete(context.Background(), connect.NewRequest(&profilesv1.DeleteRequest{Id: "p1"}))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if code := connect.CodeOf(err); code != connect.CodeUnauthenticated {
		t.Errorf("got code %v, want CodeUnauthenticated", code)
	}
}

func TestGet_Unauthenticated(t *testing.T) {
	s := NewServer(nil, nil, &natsdeps.NATSClient{})
	_, err := s.Get(context.Background(), connect.NewRequest(&profilesv1.GetRequest{Id: "p1"}))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if code := connect.CodeOf(err); code != connect.CodeUnauthenticated {
		t.Errorf("got code %v, want CodeUnauthenticated", code)
	}
}
