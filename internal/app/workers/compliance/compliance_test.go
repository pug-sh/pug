package compliance

import (
	"context"
	"errors"
	"testing"

	coreprofiles "github.com/pug-sh/pug/internal/core/profiles"
	natsworker "github.com/pug-sh/pug/internal/deps/nats"
	workercompliancev1 "github.com/pug-sh/pug/internal/gen/proto/workers/compliance/v1"
	"google.golang.org/protobuf/proto"
)

// fakeExecutor records how handleErase drives the service so the error
// classification + failure-recording can be asserted without PG/CH/NATS.
type fakeExecutor struct {
	execErr error
	markErr error

	executeCalls  int
	markCalls     int
	markedProject string
	markedRequest string
	markedCause   error
}

func (f *fakeExecutor) ExecuteErasure(ctx context.Context, projectID, requestID string) error {
	f.executeCalls++
	return f.execErr
}

func (f *fakeExecutor) MarkErasureFailed(ctx context.Context, projectID, requestID string, cause error) error {
	f.markCalls++
	f.markedProject = projectID
	f.markedRequest = requestID
	f.markedCause = cause
	return f.markErr
}

func mustMarshalErase(t *testing.T, projectID, requestID string) []byte {
	t.Helper()
	data, err := proto.Marshal(&workercompliancev1.EraseMessage{
		ProjectId: proto.String(projectID),
		RequestId: proto.String(requestID),
	})
	if err != nil {
		t.Fatalf("marshal erase message: %v", err)
	}
	return data
}

// A corrupt payload can never decode, so it must be terminated (DLQ), never
// retried, and the service must not be touched.
func TestHandleErase_GarbageData_IsPermanentAndSkipsExecute(t *testing.T) {
	exec := &fakeExecutor{}
	// 0xFF is an incomplete protobuf varint tag — guaranteed to fail Unmarshal.
	err := handleErase(context.Background(), exec, []byte{0xFF}, false)
	if !natsworker.IsPermanentError(err) {
		t.Fatalf("err = %v, want PermanentError", err)
	}
	if exec.executeCalls != 0 {
		t.Errorf("ExecuteErasure calls = %d, want 0", exec.executeCalls)
	}
}

// A well-formed message missing a required field fails protovalidate and is
// permanent — it can never succeed on retry.
func TestHandleErase_InvalidMessage_IsPermanentAndSkipsExecute(t *testing.T) {
	exec := &fakeExecutor{}
	data := mustMarshalErase(t, "proj-1", "") // request_id is required + min_len=1
	err := handleErase(context.Background(), exec, data, false)
	if !natsworker.IsPermanentError(err) {
		t.Fatalf("err = %v, want PermanentError", err)
	}
	if exec.executeCalls != 0 {
		t.Errorf("ExecuteErasure calls = %d, want 0", exec.executeCalls)
	}
}

// A missing request row is unrecoverable: route to DLQ, and do not try to mark a
// row that does not exist.
func TestHandleErase_NotFound_IsPermanentAndDoesNotMarkFailed(t *testing.T) {
	exec := &fakeExecutor{execErr: coreprofiles.ErrDeletionRequestNotFound}
	data := mustMarshalErase(t, "proj-1", "req-1")
	err := handleErase(context.Background(), exec, data, true) // last delivery shouldn't matter
	if !natsworker.IsPermanentError(err) {
		t.Fatalf("err = %v, want PermanentError", err)
	}
	if exec.executeCalls != 1 {
		t.Errorf("ExecuteErasure calls = %d, want 1", exec.executeCalls)
	}
	if exec.markCalls != 0 {
		t.Errorf("MarkErasureFailed calls = %d, want 0 (no row to mark)", exec.markCalls)
	}
}

// A transient failure that is NOT the last delivery must retry (non-permanent)
// and must not prematurely mark the ledger row failed.
func TestHandleErase_TransientNotLastDelivery_RetriesWithoutMarking(t *testing.T) {
	exec := &fakeExecutor{execErr: errors.New("clickhouse unavailable")}
	data := mustMarshalErase(t, "proj-1", "req-1")
	err := handleErase(context.Background(), exec, data, false)
	if err == nil {
		t.Fatal("err = nil, want transient error for retry")
	}
	if natsworker.IsPermanentError(err) {
		t.Error("err is PermanentError, want transient so the framework Naks/retries")
	}
	if exec.markCalls != 0 {
		t.Errorf("MarkErasureFailed calls = %d, want 0 (not last delivery)", exec.markCalls)
	}
}

// On the final delivery a transient failure is about to be dead-lettered forever,
// so the row must be marked failed — but the returned error must stay transient
// so the framework still routes the message to the DLQ.
func TestHandleErase_TransientLastDelivery_MarksFailed(t *testing.T) {
	cause := errors.New("clickhouse unavailable")
	exec := &fakeExecutor{execErr: cause}
	data := mustMarshalErase(t, "proj-1", "req-1")
	err := handleErase(context.Background(), exec, data, true)
	if err == nil || natsworker.IsPermanentError(err) {
		t.Fatalf("err = %v, want non-permanent error so the framework DLQs after marking", err)
	}
	if exec.markCalls != 1 {
		t.Fatalf("MarkErasureFailed calls = %d, want 1", exec.markCalls)
	}
	if exec.markedProject != "proj-1" || exec.markedRequest != "req-1" {
		t.Errorf("marked (%q, %q), want (proj-1, req-1)", exec.markedProject, exec.markedRequest)
	}
	if !errors.Is(exec.markedCause, cause) {
		t.Errorf("marked cause = %v, want %v", exec.markedCause, cause)
	}
}

// If marking failed itself errors (e.g. the same outage), handleErase must still
// return the original transient error so the message is dead-lettered rather than
// silently acked.
func TestHandleErase_MarkFailedError_StillDeadLetters(t *testing.T) {
	exec := &fakeExecutor{execErr: errors.New("ch down"), markErr: errors.New("pg down too")}
	data := mustMarshalErase(t, "proj-1", "req-1")
	err := handleErase(context.Background(), exec, data, true)
	if err == nil || natsworker.IsPermanentError(err) {
		t.Fatalf("err = %v, want non-permanent error so the message DLQs", err)
	}
	if exec.markCalls != 1 {
		t.Errorf("MarkErasureFailed calls = %d, want 1", exec.markCalls)
	}
}

// A request that resolves no identifiers can never succeed. The row exists, so it
// must be marked failed (unlike the not-found case) and terminated, even though
// this is not the last delivery.
func TestHandleErase_NoIdentifiers_MarksFailedAndIsPermanent(t *testing.T) {
	exec := &fakeExecutor{execErr: coreprofiles.ErrNoErasableIdentifiers}
	data := mustMarshalErase(t, "proj-1", "req-1")
	err := handleErase(context.Background(), exec, data, false)
	if !natsworker.IsPermanentError(err) {
		t.Fatalf("err = %v, want PermanentError", err)
	}
	if exec.markCalls != 1 {
		t.Fatalf("MarkErasureFailed calls = %d, want 1 (row exists, must be marked)", exec.markCalls)
	}
	if !errors.Is(exec.markedCause, coreprofiles.ErrNoErasableIdentifiers) {
		t.Errorf("marked cause = %v, want ErrNoErasableIdentifiers", exec.markedCause)
	}
}

func TestHandleErase_Success(t *testing.T) {
	exec := &fakeExecutor{}
	data := mustMarshalErase(t, "proj-1", "req-1")
	if err := handleErase(context.Background(), exec, data, false); err != nil {
		t.Fatalf("handleErase: %v", err)
	}
	if exec.executeCalls != 1 {
		t.Errorf("ExecuteErasure calls = %d, want 1", exec.executeCalls)
	}
	if exec.markCalls != 0 {
		t.Errorf("MarkErasureFailed calls = %d, want 0 on success", exec.markCalls)
	}
}
