package profiles

import (
	"context"
	"errors"
	"sync"
	"testing"

	"connectrpc.com/authn"
	"connectrpc.com/connect"
	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/pug-sh/pug/internal/app/server/rpc"
	"github.com/pug-sh/pug/internal/apperr"
	natsdeps "github.com/pug-sh/pug/internal/deps/nats"
	sdkprofilesv1 "github.com/pug-sh/pug/internal/gen/proto/sdk/profiles/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// spyJetStream captures Publish calls. Embedding the interface lets the struct
// satisfy jetstream.JetStream; only Publish is safe to call: other methods
// panic on the nil embedded value, which is fine for tests.
type spyJetStream struct {
	jetstream.JetStream
	mu        sync.Mutex
	calls     int
	published []publishedMsg
	// publishErr, when set, fails every call.
	publishErr error
	// failOn, when set, fails only the call with this 1-based number.
	failOn int
	// onPublish, when set, runs after each stored message, so a test can act
	// partway through a batch.
	onPublish func()
}

var errPublishFailed = errors.New("publish failed")

type publishedMsg struct {
	Subject string
	Data    []byte
}

// Publish honours ctx as the real client does: a done context fails the call.
func (s *spyJetStream) Publish(ctx context.Context, subject string, data []byte, _ ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.calls++
	if s.publishErr != nil {
		return nil, s.publishErr
	}
	if s.calls == s.failOn {
		return nil, errPublishFailed
	}
	s.published = append(s.published, publishedMsg{Subject: subject, Data: data})
	if s.onPublish != nil {
		s.onPublish()
	}
	return &jetstream.PubAck{}, nil
}

func sdkContext(projectID string) context.Context {
	return authn.SetInfo(context.Background(), &rpc.Principal{
		Project: &dbread.Project{ID: projectID},
	})
}

func TestIdentify_Success(t *testing.T) {
	spy := &spyJetStream{}
	srv := NewServer(spy)

	traits, _ := structpb.NewStruct(map[string]any{"plan": "pro"})
	req := connect.NewRequest(&sdkprofilesv1.IdentifyRequest{
		ExternalId:  proto.String("user-42"),
		Traits:      traits,
		AnonymousId: proto.String("anon-1"),
		DeviceId:    proto.String("xid-device-id-test1234"),
	})

	ctx := sdkContext("proj-test")
	resp, err := srv.Identify(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}

	spy.mu.Lock()
	defer spy.mu.Unlock()

	if len(spy.published) != 1 {
		t.Fatalf("expected 1 published message, got %d", len(spy.published))
	}

	msg := spy.published[0]
	if msg.Subject != natsdeps.ProfileIdentifySubject {
		t.Errorf("subject = %q, want %q", msg.Subject, natsdeps.ProfileIdentifySubject)
	}

	var ident sdkprofilesv1.ProfileIdentifyMessage
	if err := proto.Unmarshal(msg.Data, &ident); err != nil {
		t.Fatalf("unmarshal published message: %v", err)
	}
	if ident.GetExternalId() != "user-42" {
		t.Errorf("ExternalId = %q, want %q", ident.GetExternalId(), "user-42")
	}
	if ident.GetProjectId() != "proj-test" {
		t.Errorf("ProjectId = %q, want %q", ident.GetProjectId(), "proj-test")
	}
	if ident.GetAnonymousId() != "anon-1" {
		t.Errorf("AnonymousId = %q, want %q", ident.GetAnonymousId(), "anon-1")
	}
	if ident.GetDeviceId() != "xid-device-id-test1234" {
		t.Errorf("DeviceId = %q, want %q", ident.GetDeviceId(), "xid-device-id-test1234")
	}
	if ident.Traits == nil || ident.Traits.Fields["plan"].GetStringValue() != "pro" {
		t.Errorf("Traits.plan = %v, want %q", ident.Traits, "pro")
	}
}

func TestIdentify_StripsClientSuppliedIP(t *testing.T) {
	spy := &spyJetStream{}
	srv := NewServer(spy)

	// An untrusted SDK caller injects $ip into traits. It must never be
	// persisted onto the profile: the handler strips it while keeping
	// legitimate (non-reserved) traits.
	traits, _ := structpb.NewStruct(map[string]any{
		"$ip":  "203.0.113.7",
		"plan": "pro",
	})
	req := connect.NewRequest(&sdkprofilesv1.IdentifyRequest{
		ExternalId: proto.String("user-42"),
		Traits:     traits,
	})

	if _, err := srv.Identify(sdkContext("proj-test"), req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	spy.mu.Lock()
	defer spy.mu.Unlock()
	if len(spy.published) != 1 {
		t.Fatalf("expected 1 published message, got %d", len(spy.published))
	}

	var ident sdkprofilesv1.ProfileIdentifyMessage
	if err := proto.Unmarshal(spy.published[0].Data, &ident); err != nil {
		t.Fatalf("unmarshal published message: %v", err)
	}
	if _, ok := ident.GetTraits().GetFields()["$ip"]; ok {
		t.Error("$ip must be stripped from traits, but it was published")
	}
	if ident.GetTraits().GetFields()["plan"].GetStringValue() != "pro" {
		t.Errorf("legitimate trait dropped: plan = %v, want %q",
			ident.GetTraits().GetFields()["plan"], "pro")
	}
}

// jsonb can't hold a NUL and the worker would fail the whole identify on one, so the
// trait holding it is dropped and the rest, the merge and the device link still apply.
func TestIdentify_DropsNULTraits(t *testing.T) {
	spy := &spyJetStream{}
	traits, _ := structpb.NewStruct(map[string]any{
		"note":   "a\x00b",
		"a\x00b": "key",
		"plan":   "pro",
	})
	req := connect.NewRequest(&sdkprofilesv1.IdentifyRequest{
		ExternalId:  proto.String("user-42"),
		Traits:      traits,
		AnonymousId: proto.String("anon-1"),
		DeviceId:    proto.String("device-1"),
	})
	if _, err := NewServer(spy).Identify(sdkContext("proj-test"), req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	msgs := publishedIdentifies(t, spy)
	if len(msgs) != 1 {
		t.Fatalf("published %d messages, want 1", len(msgs))
	}
	if got := msgs[0].GetTraits().GetFields(); len(got) != 1 || got["plan"].GetStringValue() != "pro" {
		t.Errorf("traits = %v, want plan=pro only", got)
	}
	if msgs[0].GetAnonymousId() != "anon-1" || msgs[0].GetDeviceId() != "device-1" {
		t.Errorf("message = %v, want anonymous_id and device_id kept", msgs[0])
	}
}

func TestIdentify_NilTraitsIsSafe(t *testing.T) {
	spy := &spyJetStream{}
	srv := NewServer(spy)

	// Traits unset (nil Struct). The $ip strip dereferences GetTraits().GetFields(),
	// a nil map, so this pins the handler comment's "safe no-op" claim: no panic,
	// and the identify still publishes.
	req := connect.NewRequest(&sdkprofilesv1.IdentifyRequest{
		ExternalId: proto.String("user-7"),
	})

	if _, err := srv.Identify(sdkContext("proj-test"), req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	spy.mu.Lock()
	defer spy.mu.Unlock()
	if len(spy.published) != 1 {
		t.Fatalf("expected 1 published message, got %d", len(spy.published))
	}

	var ident sdkprofilesv1.ProfileIdentifyMessage
	if err := proto.Unmarshal(spy.published[0].Data, &ident); err != nil {
		t.Fatalf("unmarshal published message: %v", err)
	}
	if len(ident.GetTraits().GetFields()) != 0 {
		t.Errorf("expected no traits, got %v", ident.GetTraits().GetFields())
	}
}

func TestIdentify_Unauthenticated(t *testing.T) {
	srv := NewServer(&spyJetStream{})

	req := connect.NewRequest(&sdkprofilesv1.IdentifyRequest{
		ExternalId: proto.String("user-42"),
	})

	// No principal in context.
	_, err := srv.Identify(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for unauthenticated request")
	}
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code() != connect.CodeUnauthenticated {
		t.Fatalf("want unauthenticated apperr, got %v (%T)", err, err)
	}
}

func TestIdentify_PublishError(t *testing.T) {
	spy := &spyJetStream{publishErr: errors.New("nats down")}
	srv := NewServer(spy)

	req := connect.NewRequest(&sdkprofilesv1.IdentifyRequest{
		ExternalId: proto.String("user-42"),
	})

	_, err := srv.Identify(sdkContext("proj-test"), req)
	if err == nil {
		t.Fatal("expected error when NATS publish fails")
	}
	if code := connect.CodeOf(err); code != connect.CodeInternal {
		t.Errorf("code = %v, want %v", code, connect.CodeInternal)
	}
}

// An identify over the NATS server's max_payload can never publish, so a code a
// client treats as transient would have it resend the same bytes forever.
func TestIdentify_OverMaxPayloadIsInvalid(t *testing.T) {
	spy := &spyJetStream{publishErr: natsgo.ErrMaxPayload}
	req := connect.NewRequest(&sdkprofilesv1.IdentifyRequest{ExternalId: proto.String("user-42")})

	_, err := NewServer(spy).Identify(sdkContext("proj-test"), req)

	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code() != connect.CodeInvalidArgument {
		t.Fatalf("want invalid-argument apperr, got %v (%T)", err, err)
	}
	if ae.Reason() != apperr.ReasonIdentifyTooLarge {
		t.Errorf("reason = %v, want %v", ae.Reason(), apperr.ReasonIdentifyTooLarge)
	}
}
