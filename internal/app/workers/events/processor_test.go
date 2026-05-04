package events

import (
	"context"
	"errors"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	natsworker "github.com/pug-sh/pug/internal/deps/nats"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	eventsv1 "github.com/pug-sh/pug/internal/gen/proto/sdk/events/v1"
)

// fakeChConn embeds a nil driver.Conn — only PrepareBatch is overridden. Any
// other method will panic via nil-interface dispatch, which is what we want
// for a failing-path test (the processor only calls PrepareBatch).
type fakeChConn struct {
	driver.Conn
	batch driver.Batch
	err   error
}

func (f *fakeChConn) PrepareBatch(ctx context.Context, query string, opts ...driver.PrepareBatchOption) (driver.Batch, error) {
	return f.batch, f.err
}

// fakeBatch lets the test control the Append return error.
type fakeBatch struct {
	driver.Batch
	appendErr error
}

func (b *fakeBatch) Append(...any) error { return b.appendErr }
func (b *fakeBatch) Send() error         { return nil }
func (b *fakeBatch) Abort() error        { return nil }
func (b *fakeBatch) IsSent() bool        { return false }

func TestProcessMessage_UnmarshalFailure(t *testing.T) {
	p := NewProcessor(nil)
	err := p.ProcessMessage(context.Background(), []byte("not-proto"))
	if err == nil {
		t.Fatal("expected error for invalid proto data, got nil")
	}
	if !natsworker.IsPermanentError(err) {
		t.Errorf("expected PermanentError, got %T: %v", err, err)
	}
}

func TestProcessMessage_MissingProjectID(t *testing.T) {
	batch := &eventsv1.EventBatch{
		Events: []*eventsv1.Event{
			{
				EventId:    proto.String("550e8400-e29b-41d4-a716-446655440000"),
				DistinctId: proto.String("user-1"),
				Kind:       proto.String("page_view"),
				OccurTime:  timestamppb.Now(),
				SessionId:  proto.String("550e8400-e29b-41d4-a716-446655440001"),
			},
		},
		// ProjectId intentionally omitted
	}
	data, err := proto.Marshal(batch)
	if err != nil {
		t.Fatalf("proto.Marshal: %v", err)
	}

	p := NewProcessor(nil)
	err = p.ProcessMessage(context.Background(), data)
	if err == nil {
		t.Fatal("expected validation error for missing project_id, got nil")
	}
	if !natsworker.IsPermanentError(err) {
		t.Errorf("expected PermanentError, got %T: %v", err, err)
	}
}

// TestProcessMessage_AppendErrorIsPermanent pins the contract that
// chBatch.Append failures (Variant slot mismatch, encode error, schema drift)
// surface as PermanentError so the NATS worker DLQs on first delivery instead
// of retrying the poison batch up to MaxDeliver times.
func TestProcessMessage_AppendErrorIsPermanent(t *testing.T) {
	batch := &eventsv1.EventBatch{
		ProjectId: proto.String("proj-123"),
		Events: []*eventsv1.Event{
			{
				EventId:    proto.String("550e8400-e29b-41d4-a716-446655440000"),
				DistinctId: proto.String("user-1"),
				Kind:       proto.String("page_view"),
				OccurTime:  timestamppb.Now(),
				SessionId:  proto.String("550e8400-e29b-41d4-a716-446655440001"),
				CustomProperties: map[string]*commonv1.PropertyValue{
					"k": {Value: &commonv1.PropertyValue_StringValue{StringValue: "v"}},
				},
			},
		},
	}
	data, err := proto.Marshal(batch)
	if err != nil {
		t.Fatalf("proto.Marshal: %v", err)
	}

	wantErr := errors.New("variant slot mismatch")
	conn := &fakeChConn{batch: &fakeBatch{appendErr: wantErr}}
	p := NewProcessor(conn)

	err = p.ProcessMessage(context.Background(), data)
	if err == nil {
		t.Fatal("expected error from Append, got nil")
	}
	if !natsworker.IsPermanentError(err) {
		t.Errorf("expected PermanentError, got %T: %v", err, err)
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("expected wrapped error to contain %v, got %v", wantErr, err)
	}
}

func TestProcessMessage_EmptyBatch(t *testing.T) {
	batch := &eventsv1.EventBatch{
		ProjectId: proto.String("proj-123"),
	}
	data, err := proto.Marshal(batch)
	if err != nil {
		t.Fatalf("proto.Marshal: %v", err)
	}

	p := NewProcessor(nil)
	if err := p.ProcessMessage(context.Background(), data); err != nil {
		t.Fatalf("expected nil for empty batch, got: %v", err)
	}
}
