package rpc

import (
	"context"
	"errors"
	"io"
	"testing"

	"connectrpc.com/connect"
)

func TestConnectCtxErr_Nil(t *testing.T) {
	if got := ConnectCtxErr(nil); got != nil {
		t.Errorf("ConnectCtxErr(nil) = %v, want nil", got)
	}
}

func TestConnectCtxErr_Canceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := ConnectCtxErr(ctx.Err())
	if code := connect.CodeOf(err); code != connect.CodeCanceled {
		t.Errorf("got code %v, want CodeCanceled", code)
	}
}

func TestConnectCtxErr_DeadlineExceeded(t *testing.T) {
	err := ConnectCtxErr(context.DeadlineExceeded)
	if code := connect.CodeOf(err); code != connect.CodeDeadlineExceeded {
		t.Errorf("got code %v, want CodeDeadlineExceeded", code)
	}
}

func TestConnectCtxErr_NonContextError(t *testing.T) {
	// A non-context error reaching ConnectCtxErr is a programmer error;
	// surface as Internal rather than mis-mapping to Canceled.
	err := ConnectCtxErr(io.EOF)
	if code := connect.CodeOf(err); code != connect.CodeInternal {
		t.Errorf("got code %v, want CodeInternal", code)
	}
}

func TestConnectCtxErr_WrappedDeadlineExceeded(t *testing.T) {
	wrapped := errors.Join(errors.New("wrap"), context.DeadlineExceeded)
	err := ConnectCtxErr(wrapped)
	if code := connect.CodeOf(err); code != connect.CodeDeadlineExceeded {
		t.Errorf("got code %v, want CodeDeadlineExceeded", code)
	}
}
